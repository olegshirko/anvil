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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/opencontainers/runtime-spec/specs-go"
)

const (
	cniVersion         = "0.4.0"
	defaultNetworkName = "bridge"
	debounceDelay      = 150 * time.Millisecond
	pollInterval       = 500 * time.Millisecond
)

// cniPortMapping is the shape of the nerdctl/ports label.
type cniPortMapping struct {
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
	HostIP        string `json:"hostIP"`
}

// getNerdctlPortsLabel returns the container's port mappings as the legacy
// nerdctl/ports label JSON. Sources, in order: the label itself, the OCI spec
// annotations (containers on non-default networks), and — since nerdctl 2.2
// (PR #4290) deprecated the label — the nerdctl network store on disk.
func getNerdctlPortsLabel(c client.Container, nsCtx context.Context) string {
	if labels, err := c.Labels(nsCtx); err == nil && labels != nil {
		if v := labels["nerdctl/ports"]; v != "" {
			return v
		}
	}
	var spec *specs.Spec
	var err error
	if spec, err = c.Spec(nsCtx); err == nil && spec != nil {
		if v := spec.Annotations["nerdctl/ports"]; v != "" {
			return v
		}
	}
	if ns, ok := namespaces.Namespace(nsCtx); ok {
		if v := readNetworkStorePorts(ns, c.ID()); v != "" {
			return v
		}
	}
	return ""
}

// nerdctlNetworkConfig mirrors nerdctl's networkstore.NetworkConfig.
type nerdctlNetworkConfig struct {
	PortMappings []cniPortMapping `json:"portMappings"`
}

// readNetworkStorePorts reads the port mappings nerdctl >= 2.2 persists in
// <dataRoot>/<addrHash>/containers/<ns>/<id>/network-config.json and renders
// them as the legacy label JSON so callers keep a single parse path. The
// addrHash directory (sha256 of the containerd socket address) is located by
// glob — only one exists in practice.
func readNetworkStorePorts(ns, id string) string {
	matches, _ := filepath.Glob(filepath.Join(nerdctlStoreRoot, "*", "containers", ns, id, "network-config.json"))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var conf nerdctlNetworkConfig
		if err := json.Unmarshal(data, &conf); err != nil || len(conf.PortMappings) == 0 {
			continue
		}
		out, err := json.Marshal(conf.PortMappings)
		if err == nil {
			return string(out)
		}
	}
	return ""
}

type portScanner struct {
	mu          sync.Mutex
	current     []PortMapping
	subscribers map[chan PortMapState]struct{}
	guestIP     string
	// containerIPs caches (namespace, containerd id) -> {task pid, CNI IP}
	// so per-scan nerdctl inspects only run for new or restarted containers.
	containerIPs map[string]containerIPEntry
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
		if _, err := cl.NamespaceService().List(context.Background()); err != nil {
			log.Printf("[scanner] containerd connection lost, reconnecting")
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

	if stateEqual(s.current, state.Mappings) {
		return false
	}
	s.current = state.Mappings
	return true
}

func (s *portScanner) pushCurrentState() {
	s.mu.Lock()
	state := s.currentStateLocked()
	chans := make([]chan PortMapState, 0, len(s.subscribers))
	for ch := range s.subscribers {
		chans = append(chans, ch)
	}
	s.mu.Unlock()

	for _, ch := range chans {
		select {
		case ch <- state:
		default:
			// Drop to slow subscribers; they will get the next update.
		}
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
	return PortMapState{Mappings: mappings}
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
	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return PortMapState{}, fmt.Errorf("list namespaces: %w", err)
	}

	guestIP := s.guestIP
	if guestIP == "" {
		guestIP = detectGuestIP()
		s.guestIP = guestIP
	}

	var mappings []PortMapping

	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			log.Printf("[scanner] list containers in %s: %v", ns, err)
			continue
		}

		for _, c := range containers {
			labels, err := c.Labels(nsCtx)
			if err != nil {
				continue
			}

			// Skip containers that are not running.
			task, err := c.Task(nsCtx, nil)
			if err != nil {
				continue
			}
			status, err := task.Status(nsCtx)
			if err != nil || status.Status != "running" {
				continue
			}

			portsJSON := getNerdctlPortsLabel(c, nsCtx)
			if portsJSON == "" {
				continue
			}

			var ports []cniPortMapping
			if err := json.Unmarshal([]byte(portsJSON), &ports); err != nil {
				log.Printf("[scanner] parse ports for %s: %v", c.ID(), err)
				continue
			}

			// The host forwarder reaches containers through the guest-side
			// port proxy, so it needs the CNI address (10.10.x.y), not the
			// guest NAT IP. nerdctl inspect costs ~100ms, so cache per
			// (namespace, id) keyed by task pid — a restart gets a new pid.
			containerIP := s.containerIPFor(ns, c.ID(), task.Pid(), labels["nerdctl/name"])

			for _, p := range ports {
				proto := p.Protocol
				if proto == "" {
					proto = "tcp"
				}
				mappings = append(mappings, PortMapping{
					Namespace:     ns,
					ContainerID:   c.ID(),
					Name:          labels["nerdctl/name"],
					HostPort:      p.HostPort,
					ContainerPort: p.ContainerPort,
					Protocol:      proto,
					GuestIP:       guestIP,
					ContainerIP:   containerIP,
				})
			}
		}
	}

	sortPortMappings(mappings)
	return PortMapState{Mappings: mappings}, nil
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
	path := filepath.Join(cniConfDir, "nerdctl-"+base+".conflist")

	bridge := "br-" + base
	if len(bridge) > 15 {
		bridge = bridge[:15]
	}

	octet := projectSubnetOctet(ns)
	subnet := fmt.Sprintf("10.10.%d.0/24", octet)
	gateway := fmt.Sprintf("10.10.%d.1", octet)

	labels := map[string]string{}
	if ns == "default" {
		labels["nerdctl/default-network"] = "true"
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

	conf := map[string]interface{}{
		"cniVersion":    cniVersion,
		"name":          netName,
		"nerdctlID":     networkID(ns),
		"nerdctlLabels": labels,
		"plugins": []interface{}{
			map[string]interface{}{
				"type":        "bridge",
				"bridge":      bridge,
				"isGateway":   true,
				"ipMasq":      true,
				"hairpinMode": true,
				"ipam": map[string]interface{}{
					"type": "host-local",
					"ranges": []interface{}{
						[]interface{}{
							map[string]interface{}{
								"subnet":     subnet,
								"rangeStart": fmt.Sprintf("10.10.%d.2", octet),
								"rangeEnd":   fmt.Sprintf("10.10.%d.254", octet),
								"gateway":    gateway,
							},
						},
					},
					"routes": []interface{}{
						map[string]interface{}{"dst": "0.0.0.0/0"},
					},
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
	log.Printf("[cni-gen] created CNI config for namespace %s: %s", ns, subnet)
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
