package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	tasktype "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

const (
	cniVersion         = "0.4.0"
	defaultNetworkName = "bridge"
	debounceDelay      = 150 * time.Millisecond
	pollInterval       = 500 * time.Millisecond
	scanTimeout        = 10 * time.Second
)

// cniPortMapping is the shape of the anvil/ports label.
type cniPortMapping struct {
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
	HostIP        string `json:"hostIP"`
	// Ephemeral marks a binding without a fixed host port (`-p 80`, `-P`,
	// `-p 8000-8010:80`): startDockerContainer picks a free port and
	// stores it in HostPort, which stays 0 until the first start.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// RangeLo/RangeHi bound the pick for a host port range; zero means
	// the ephemeral range.
	RangeLo int `json:"rangeLo,omitempty"`
	RangeHi int `json:"rangeHi,omitempty"`
}

// portsLabel returns the container's port mappings as JSON (the shape of the
// anvil/ports label written at create time).
func portsLabel(c client.Container, nsCtx context.Context) string {
	labels, err := c.Labels(nsCtx)
	if err != nil || labels == nil {
		return ""
	}
	return labels[labelPorts]
}

type portScanner struct {
	mu      sync.Mutex
	current []PortMapping
	// running counts running containers in every namespace. The host keeps
	// the VM awake while it is non-zero: an idle pause would freeze
	// databases, servers and workers between CLI calls.
	running    int
	watchPaths []string
	domains    []DomainEntry
	// pushedGuestIP is the address in the last state (guestIP is updated
	// by every scan, before the comparison).
	pushedGuestIP string
	subscribers   map[chan PortMapState]struct{}
	guestIP       string
	// containerIPs caches (namespace, containerd id) -> {task pid, CNI IP}
	// so per-scan address lookups only run for new or restarted containers.
	containerIPs map[string]containerIPEntry
	// scanInfo caches per-task metadata (bind paths, host networking).
	scanInfo map[string]containerScanInfo
	// hostNetCache holds host-network containers' discovered ports.
	hostNetCache map[string]*hostNetCacheEntry
}

type containerIPEntry struct {
	pid uint32
	ip  string
}

func newPortScanner() *portScanner {
	return &portScanner{
		subscribers:  make(map[chan PortMapState]struct{}),
		containerIPs: make(map[string]containerIPEntry),
	}
}

// containerIPFor returns the container's CNI address, cached by task pid.
func (s *portScanner) containerIPFor(ns, id string, pid uint32, name string) string {
	key := ns + "/" + id
	if e, ok := s.containerIPs[key]; ok && e.pid == pid && e.ip != "" {
		return e.ip
	}
	_, ip := containerNetworkInfo(ns, id, name)
	if ip != "" {
		s.containerIPs[key] = containerIPEntry{pid: pid, ip: ip}
	}
	return ip
}

func (s *portScanner) run() {
	var (
		cl            *client.Client
		debounceTimer *time.Timer
	)

	connect := func() *client.Client {
		for {
			c, err := client.New(containerdSocket)
			if err == nil {
				log.Printf("[scanner] connected to containerd")
				return c
			}
			log.Printf("[scanner] waiting for containerd: %v", err)
			time.Sleep(500 * time.Millisecond)
		}
	}

	cl = connect()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Trigger an immediate full scan once containerd is up.
	s.scanAndNotify(cl)

	for range ticker.C {
		// Reconnect if containerd went away.
		pctx, pcancel := context.WithTimeout(context.Background(), scanTimeout)
		_, err := cl.NamespaceService().List(pctx)
		pcancel()
		if err != nil {
			log.Printf("[scanner] containerd connection lost, reconnecting")
			cl.Close() //nolint:errcheck
			cl = connect()
		}

		changed := s.scanAndNotify(cl)
		if changed {
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(debounceDelay, func() {
				s.pushCurrentState()
			})
		}
	}
}

func (s *portScanner) scanAndNotify(cl *client.Client) bool {
	state, err := s.buildState(cl)
	if err != nil {
		log.Printf("[scanner] build state: %v", err)
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if stateEqual(s.current, state.Mappings) && s.running == state.RunningContainers &&
		slices.Equal(s.watchPaths, state.WatchPaths) && domainsEqual(s.domains, state.Domains) &&
		s.pushedGuestIP == state.GuestIP {
		return false
	}
	s.current = state.Mappings
	s.running = state.RunningContainers
	s.watchPaths = state.WatchPaths
	s.domains = state.Domains
	s.pushedGuestIP = state.GuestIP
	return true
}

// pushCurrentState hands the latest state to every subscriber. It sends
// under s.mu so unsubscribe cannot close a channel mid-send (a send on a
// closed channel panics PID 1), and it never blocks: a subscriber that has
// not taken the previous state gets it replaced, so the host always ends up
// with the newest full state rather than a stale one.
func (s *portScanner) pushCurrentState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.currentStateLocked()
	for ch := range s.subscribers {
		offerLatest(ch, state)
	}
}

// offerLatest puts state into a 1-slot channel, replacing an unread value.
func offerLatest(ch chan PortMapState, state PortMapState) {
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- state:
	default:
	}
}

func (s *portScanner) currentState() PortMapState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentStateLocked()
}

func (s *portScanner) currentStateLocked() PortMapState {
	mappings := make([]PortMapping, len(s.current))
	copy(mappings, s.current)
	return PortMapState{Mappings: mappings, RunningContainers: s.running,
		WatchPaths: slices.Clone(s.watchPaths), Domains: slices.Clone(s.domains), GuestIP: s.pushedGuestIP}
}

func (s *portScanner) subscribe() chan PortMapState {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan PortMapState, 1)
	s.subscribers[ch] = struct{}{}
	return ch
}

func (s *portScanner) unsubscribe(ch chan PortMapState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subscribers, ch)
	close(ch)
}

func (s *portScanner) buildState(cl *client.Client) (PortMapState, error) {
	// Bounded: a hung containerd must not freeze port forwarding forever;
	// the next tick simply retries.
	ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
	defer cancel()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return PortMapState{}, fmt.Errorf("list namespaces: %w", err)
	}

	// Re-read every scan: the address can change after the first scan (the
	// DHCP lease lands after boot, or a new machine identifier gets a new
	// lease). A value cached once kept the host forwarder dialing a dead
	// address. A changed IP changes every mapping, so the host rebuilds its
	// listeners on the next push.
	guestIP := detectGuestIP()
	if guestIP == "" {
		guestIP = s.guestIP
	}
	s.guestIP = guestIP

	var mappings []PortMapping
	var watch []string
	var listeners map[string]tcpListener
	var listenSig string
	var domains []DomainEntry
	infoSeen := map[string]bool{}
	seen := make(map[string]bool)
	running := 0

	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		// One task listing per namespace instead of a Task+Status
		// round-trip per container.
		live := namespaceRunningTasks(nsCtx, cl)
		running += len(live)
		if len(live) == 0 {
			continue
		}
		for id, pid := range live {
			key := ns + "/" + id
			info, ok := s.scanInfo[key]
			if !ok || info.pid != pid {
				var labels map[string]string
				if c, lerr := cl.LoadContainer(nsCtx, id); lerr == nil {
					if ci, ierr := c.Info(nsCtx, client.WithoutRefreshedMetadata); ierr == nil {
						labels = ci.Labels
					}
				}
				info = loadContainerScanInfo(ns, id, pid, labels)
				if s.scanInfo == nil {
					s.scanInfo = map[string]containerScanInfo{}
				}
				s.scanInfo[key] = info
			}
			infoSeen[key] = true
			watch = append(watch, info.watchPaths...)
			if !info.hostNet {
				if len(info.domains) > 0 {
					seen[key] = true // keeps the address cached
					if ip := s.containerIPFor(ns, id, pid, info.name); ip != "" {
						domains = append(domains, DomainEntry{Names: info.domains, IP: ip, Port: info.httpPort})
					}
				}
				continue
			}
			if listeners == nil {
				listeners = rootNetnsListeners()
				listenSig = listenersSignature(listeners)
			}
			if s.hostNetCache == nil {
				s.hostNetCache = map[string]*hostNetCacheEntry{}
			}
			entry := s.hostNetCache[key]
			if entry == nil {
				entry = &hostNetCacheEntry{}
				s.hostNetCache[key] = entry
			}
			for port, target := range hostNetworkPorts(pid, listeners, listenSig, entry) {
				mappings = append(mappings, PortMapping{
					Namespace:     ns,
					ContainerID:   id,
					Name:          info.name,
					HostPort:      port,
					ContainerPort: port,
					Protocol:      "tcp",
					GuestIP:       guestIP,
					ContainerIP:   target,
					HostIP:        "127.0.0.1", // the Mac's loopback, as Docker Desktop does
				})
			}
		}
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			log.Printf("[scanner] list containers in %s: %v", ns, err)
			continue
		}

		for _, c := range containers {
			// The list call already fetched the record; do not re-fetch it
			// per container every tick. Containers without published ports
			// are skipped before any task round-trip.
			info, err := c.Info(nsCtx, client.WithoutRefreshedMetadata)
			if err != nil {
				continue
			}
			labels := info.Labels
			portsJSON := labels[labelPorts]
			if portsJSON == "" {
				continue
			}

			// Skip containers that are not running.
			pid, ok := live[c.ID()]
			if !ok {
				continue
			}

			var ports []cniPortMapping
			if err := json.Unmarshal([]byte(portsJSON), &ports); err != nil {
				log.Printf("[scanner] parse ports for %s: %v", c.ID(), err)
				continue
			}

			// The host forwarder reaches containers through the guest-side
			// port proxy, so it needs the CNI address (10.10.x.y), not the
			// guest NAT IP. Address lookups cost ~ms, so cache per
			// (namespace, id) keyed by task pid — a restart gets a new pid.
			containerIP := s.containerIPFor(ns, c.ID(), pid, labels[labelName])
			seen[ns+"/"+c.ID()] = true

			for _, p := range ports {
				if p.HostPort <= 0 {
					continue // ephemeral binding not assigned yet
				}
				proto := p.Protocol
				if proto == "" {
					proto = "tcp"
				}
				mappings = append(mappings, PortMapping{
					Namespace:     ns,
					ContainerID:   c.ID(),
					Name:          labels[labelName],
					HostPort:      p.HostPort,
					ContainerPort: p.ContainerPort,
					Protocol:      proto,
					GuestIP:       guestIP,
					ContainerIP:   containerIP,
					HostIP:        pushedHostIP(p.HostIP),
				})
			}
		}
	}

	for key := range s.scanInfo {
		if !infoSeen[key] {
			delete(s.scanInfo, key)
			delete(s.hostNetCache, key)
		}
	}
	// Removed and stopped containers leave the address cache.
	for key := range s.containerIPs {
		if !seen[key] {
			delete(s.containerIPs, key)
		}
	}

	sortPortMappings(mappings)
	sortDomains(domains)
	return PortMapState{Mappings: mappings, RunningContainers: running, WatchPaths: minimalWatchPaths(watch),
		Domains: domains, GuestIP: guestIP}, nil
}

// namespaceRunningTasks maps containerd container ID -> task pid for every
// running task in one namespace.
func namespaceRunningTasks(nsCtx context.Context, cl *client.Client) map[string]uint32 {
	out := map[string]uint32{}
	resp, err := cl.TaskService().List(nsCtx, &tasks.ListTasksRequest{})
	if err != nil {
		return out
	}
	for _, p := range resp.Tasks {
		if p.Status == tasktype.Status_RUNNING {
			out[p.ID] = p.Pid
		}
	}
	return out
}

func stateEqual(a, b []PortMapping) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortPortMappings(m []PortMapping) {
	sort.Slice(m, func(i, j int) bool {
		if m[i].Namespace != m[j].Namespace {
			return m[i].Namespace < m[j].Namespace
		}
		if m[i].ContainerID != m[j].ContainerID {
			return m[i].ContainerID < m[j].ContainerID
		}
		if m[i].HostPort != m[j].HostPort {
			return m[i].HostPort < m[j].HostPort
		}
		return m[i].ContainerPort < m[j].ContainerPort
	})
}

func detectGuestIP() string {
	iface, err := net.InterfaceByName("eth0")
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return ""
}

func generateCNIConfig(ns string) error {
	return generateCNIConfigWithLabels(ns, nil)
}

func generateCNIConfigWithLabels(ns string, extraLabels map[string]string) error {
	netAllocMu.Lock()
	defer netAllocMu.Unlock()
	return generateCNIConfigLocked(ns, extraLabels)
}

// generateCNIConfigLocked writes a network's conflist; the caller holds
// netAllocMu.
func generateCNIConfigLocked(ns string, extraLabels map[string]string) error {
	// Docker clients expect the default network to be called "bridge".
	// Per-project networks keep their own name (e.g. project-a, compose-test_default).
	if ns == "" {
		ns = "default"
	}
	netName := ns
	if ns == "default" {
		netName = "bridge"
	}
	base := sanitizeCNIName(netName)
	path := filepath.Join(cniConfDir, "anvil-"+base+".conflist")

	existing, _ := loadCNIConflists()
	if _, ok := existing[netName]; !ok {
		sweepOrphanBridges(existing)
	}
	pool := loadNetworkPool(netName)
	alloc := pickNetAlloc(netName, projectSubnetOctet(ns), existing, pool)
	bridge := alloc.bridge
	subnet := alloc.subnet
	ipRange := map[string]interface{}{"subnet": subnet}
	if pool != nil {
		ipRange["gateway"] = pool.Gateway
		if pool.RangeStart != "" {
			ipRange["rangeStart"] = pool.RangeStart
			ipRange["rangeEnd"] = pool.RangeEnd
		}
	} else {
		ipRange["gateway"] = fmt.Sprintf("10.10.%d.1", alloc.octet)
		ipRange["rangeStart"] = fmt.Sprintf("10.10.%d.2", alloc.octet)
		ipRange["rangeEnd"] = fmt.Sprintf("10.10.%d.254", alloc.octet)
	}

	// A dual-stack network gets a second range set: host-local then hands
	// out one address per family, and the bridge plugin puts both gateways
	// on the bridge. IPv4-only networks keep the exact conflist they had.
	ranges := []interface{}{[]interface{}{ipRange}}
	v6 := loadNetworkIPv6(netName)
	logSubnets := subnet
	if v6 != nil {
		subnet6 := pickIPv6Subnet(netName, alloc.octet, existing, v6)
		ipRange6 := map[string]interface{}{"subnet": subnet6}
		if v6.Subnet != "" {
			ipRange6["gateway"] = v6.Gateway
			if v6.RangeStart != "" {
				ipRange6["rangeStart"] = v6.RangeStart
				ipRange6["rangeEnd"] = v6.RangeEnd
			}
		} else if _, n, err := net.ParseCIDR(subnet6); err == nil {
			ipRange6["gateway"] = offsetIP6(n.IP, 1).String()
		}
		ranges = append(ranges, []interface{}{ipRange6})
		logSubnets += " " + subnet6
	}

	labels := map[string]string{}
	if ns == "default" {
		labels[labelDefaultNetwork] = "true"
	}
	// Docker Compose names its default network `<project>_default`. Add the
	// labels Compose expects so it treats a pre-created CNI network as its own.
	if strings.HasSuffix(ns, "_default") {
		labels["com.docker.compose.project"] = strings.TrimSuffix(ns, "_default")
		labels["com.docker.compose.network"] = "default"
	}
	// If the caller (e.g. Compose via POST /networks/create) supplied project
	// labels, use them verbatim so custom named networks are recognised as
	// managed by Compose.
	for k, v := range extraLabels {
		labels[k] = v
	}

	// An --internal network has no way out, so it must not take the
	// default route: a container also on a normal network leaves through
	// that one (and one only on this network fails fast, as in Docker).
	internal := networkIsInternal(netName)
	var routes []interface{}
	if !internal {
		routes = []interface{}{map[string]interface{}{"dst": "0.0.0.0/0"}}
		if v6 != nil {
			routes = append(routes, map[string]interface{}{"dst": "::/0"})
		}
	}

	conf := map[string]interface{}{
		"cniVersion":    cniVersion,
		"name":          netName,
		"anvilID":       networkID(ns),
		"anvilLabels":   labels,
		"anvilInternal": internal,
		"plugins": []interface{}{
			// The loopback plugin brings `lo` up inside the fresh netns —
			// without it 127.0.0.1 does not answer inside containers.
			map[string]interface{}{
				"type": "loopback",
			},
			map[string]interface{}{
				"type":      "bridge",
				"bridge":    bridge,
				"isGateway": true,
				// Masquerading is one static rule per subnet
				// (ensureNetworkMasquerade), not a chain per container:
				// the bridge plugin's per-container chain cost ~150 ms
				// per stop and kept its teardown bound to a live netns.
				"ipMasq":      false,
				"hairpinMode": true,
				"ipam": map[string]interface{}{
					"type":   "host-local",
					"ranges": ranges,
					"routes": routes,
				},
			},
			map[string]interface{}{
				"type":         "portmap",
				"capabilities": map[string]bool{"portMappings": true},
			},
			map[string]interface{}{
				"type":          "firewall",
				"ingressPolicy": "same-bridge",
			},
			map[string]interface{}{
				"type": "tuning",
			},
		},
	}

	data, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cni config for %s: %w", ns, err)
	}

	if err := os.MkdirAll(cniConfDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", cniConfDir, err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write cni config %s: %w", path, err)
	}
	log.Printf("[cni-gen] created CNI config for namespace %s: %s", ns, logSubnets)
	return nil
}

func projectSubnetOctet(project string) int {
	h := fnv.New32a()
	h.Write([]byte(project))
	return int(h.Sum32()%250) + 1
}

func networkID(project string) string {
	h := fnv.New128a()
	h.Write([]byte("anvil-" + project))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func sanitizeCNIName(ns string) string {
	var b strings.Builder
	for _, r := range ns {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune('-')
		}
	}
	base := b.String()
	if base == "" {
		base = "default"
	}
	// Limit file name length.
	if len(base) > 64 {
		base = base[:64]
	}
	return base
}

// pushedHostIP maps a published host address to the host forwarder's form:
// the wildcard addresses become empty (bind every interface), anything else
// is passed through so the host binds only that address.
func pushedHostIP(ip string) string {
	switch ip {
	case "", "0.0.0.0", "::", "[::]":
		return ""
	}
	return ip
}
