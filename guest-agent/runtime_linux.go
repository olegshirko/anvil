//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	cniclient "github.com/containerd/go-cni"
	"github.com/containerd/log"
	cnilibrary "github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/invoke"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	unix "golang.org/x/sys/unix"
)

// Native runtime plumbing: CNI attachment and named network namespaces —
// the low-level pieces of the container lifecycle that nerdctl used to own.

// --- named network namespaces -------------------------------------------

// createNamedNetNS creates a persistent named network namespace at
// /var/run/netns/<name> and returns its path. The bind mount survives task
// death so stop/start cycles reuse one netns and CNI plugins can run before
// any process exists inside.
func createNamedNetNS(name string) (string, error) {
	if err := os.MkdirAll(netnsDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(netnsDir, name)
	os.Remove(path)
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	f.Close()

	// The unshare/mount/setns sequence must run on one locked OS thread:
	// unshare switches only the calling thread's netns; we bind-mount the
	// new ns onto the named file while inside it, then switch back.
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Unlock only when the thread is back in the agent's netns: a locked
		// goroutine that exits takes its thread down with it, so a thread
		// stuck in the new netns is discarded instead of returning to the
		// pool, where any goroutine could inherit the wrong network.
		inOrig := true
		defer func() {
			if inOrig {
				runtime.UnlockOSThread()
			}
		}()

		tid := unix.Gettid()
		selfPath := fmt.Sprintf("/proc/self/task/%d/ns/net", tid)
		orig, err := os.Open(selfPath)
		if err != nil {
			errCh <- err
			return
		}
		defer orig.Close()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			errCh <- fmt.Errorf("unshare netns: %w", err)
			return
		}
		inOrig = false
		newNS, err := os.Open(selfPath)
		if err != nil {
			if unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET) == nil {
				inOrig = true
			}
			errCh <- fmt.Errorf("open new netns: %w", err)
			return
		}
		defer newNS.Close()
		srcRef := fmt.Sprintf("/proc/self/fd/%d", newNS.Fd())
		mountErr := unix.Mount(srcRef, path, "none", unix.MS_BIND, "")
		if setnsErr := unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET); setnsErr != nil {
			if mountErr == nil {
				mountErr = fmt.Errorf("restore netns: %w", setnsErr)
			}
			errCh <- mountErr
			return
		}
		inOrig = true
		errCh <- mountErr
	}()
	if err := <-errCh; err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// releaseNamedNetNS unmounts and deletes /var/run/netns/<name>.
func releaseNamedNetNS(name string) {
	path := filepath.Join(netnsDir, name)
	_ = unix.Unmount(path, unix.MNT_DETACH)
	os.Remove(path)
}

// --- CNI attachment -------------------------------------------------------

const cniBinDir = "/opt/cni/bin"

// cniManager caches one go-cni instance per conflist file. Each anvil network
// is exactly one conflist in /etc/cni/net.d; attaching a container means
// Setup against that single list with port-mapping capabilities.
type cniManager struct {
	mu      sync.Mutex
	byFile  map[string]cniclient.CNI
	confDir string
}

var cnim = &cniManager{
	byFile:  make(map[string]cniclient.CNI),
	confDir: cniConfDir,
}

// invalidate drops cached instances (called after conflist writes/removals).
func (m *cniManager) invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byFile = make(map[string]cniclient.CNI)
}

func (m *cniManager) forConflist(path string) (cniclient.CNI, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.byFile[path]; ok {
		return c, nil
	}
	// WithConfListFile parses the conflist and registers the network right
	// away. Do NOT call Load afterwards: it resets the network list.
	c, err := cniclient.New(
		cniclient.WithPluginDir([]string{cniBinDir}),
		cniclient.WithInterfacePrefix("eth"),
		cniclient.WithConfListFile(path),
	)
	if err != nil {
		return nil, fmt.Errorf("cni init %s: %w", path, err)
	}
	m.byFile[path] = c
	return c, nil
}

// staticCNI is a one-off CNI instance over the widened conflist (not
// cached: static attaches are rare and the variant must follow the file).
func staticCNI(conflist string) (cniclient.CNI, error) {
	data, err := os.ReadFile(conflist)
	if err != nil {
		return nil, err
	}
	if data, err = staticConflistBytes(data); err != nil {
		return nil, err
	}
	return cniclient.New(
		cniclient.WithPluginDir([]string{cniBinDir}),
		cniclient.WithInterfacePrefix("eth"),
		cniclient.WithConfListBytes(data),
	)
}

// findConflistForNetwork locates the conflist file for a logical network
// name. Files are written by generateCNIConfigWithLabels with name == netName.
func findConflistForNetwork(netName string) (string, error) {
	entries, err := os.ReadDir(cniConfDir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if ext != ".conflist" && ext != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(cniConfDir, e.Name()))
		if err != nil {
			continue
		}
		var head struct {
			Name string `json:"name"`
		}
		if jsonUnmarshal(data, &head) == nil && head.Name == netName {
			return filepath.Join(cniConfDir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no CNI config for network %q", netName)
}

// attachNetwork attaches a container's netns to the given logical network
// with port mappings and returns the assigned addresses.
func attachNetwork(ctx context.Context, netName, ns, id, netnsPath string, ports []cniPortMapping) (cniAddrs, error) {
	if netName == noneNetwork {
		return cniAddrs{}, attachLoopbackOnly(ctx, id, netnsPath)
	}
	conflist, err := findConflistForNetwork(netName)
	if err != nil {
		return cniAddrs{}, err
	}
	if err := ensureNetworkMasquerade(netName); err != nil {
		log.G(ctx).WithError(err).Warnf("[cni] masquerade rule for %s", netName)
	}
	staticIP, preferred := staticIPFor(ns, id, netName), false
	if staticIP == "" {
		staticIP = preferredIP(ns, id, netName)
		preferred = staticIP != ""
	}
	var c cniclient.CNI
	if staticIP != "" {
		c, err = staticCNI(conflist)
	} else {
		c, err = cnim.forConflist(conflist)
	}
	if err != nil {
		return cniAddrs{}, err
	}
	var opts []cniclient.NamespaceOpts
	if len(ports) > 0 {
		pms := make([]cniclient.PortMapping, 0, len(ports))
		for _, p := range ports {
			if p.HostPort <= 0 {
				continue // ephemeral binding not assigned yet
			}
			proto := strings.ToUpper(p.Protocol)
			if proto == "" {
				proto = "TCP"
			}
			pms = append(pms, cniclient.PortMapping{
				HostPort:      int32(p.HostPort),
				ContainerPort: int32(p.ContainerPort),
				Protocol:      proto,
				HostIP:        p.HostIP,
			})
		}
		opts = append(opts, cniclient.WithCapabilityPortMap(pms))
	}
	// A static address: host-local takes it from CNI_ARGS IP.
	if staticIP != "" {
		opts = append(opts, cniclient.WithArgs("IgnoreUnknown", "1"), cniclient.WithArgs("IP", staticIP))
	}
	res, err := c.Setup(ctx, id, netnsPath, opts...)
	if err != nil && preferred {
		// The previous address is only a preference: undo the attempt and
		// let IPAM pick any free one.
		log.G(ctx).WithError(err).Debugf("[cni] previous address %s on %s unavailable", staticIP, netName)
		c.Remove(ctx, id, netnsPath, opts...) //nolint:errcheck
		if c, err = cnim.forConflist(conflist); err == nil {
			res, err = c.Setup(ctx, id, netnsPath, opts[:len(opts)-2]...)
		}
	}
	if err != nil {
		return cniAddrs{}, fmt.Errorf("cni setup %s: %w", netName, err)
	}
	// The firewall plugin may just have put its CNI-FORWARD jump (which
	// accepts the container's traffic) above the isolation rules.
	if cl, err := readNetworkConflist(netName); err == nil && cl.Internal {
		if err := ensureInternalIsolation(cl); err != nil {
			log.G(ctx).WithError(err).Warnf("[cni] isolation rules for %s", netName)
		}
	}
	addrs := resultAddresses(res)
	rememberIP(ns, id, netName, addrs.IP)
	log.G(ctx).WithField("network", netName).Debugf("[cni] %s attached ip=%s ipv6=%s", id[:12], addrs.IP, addrs.IPv6)
	return addrs, nil
}

// detachNetwork tears down a container endpoint on a logical network.
func detachNetwork(ctx context.Context, netName, ns, id, netnsPath string, ports []cniPortMapping) error {
	if netName == noneNetwork {
		return detachLoopbackOnly(ctx, id, netnsPath)
	}
	conflist, err := findConflistForNetwork(netName)
	if err != nil {
		return err
	}
	c, err := cnim.forConflist(conflist)
	if err != nil {
		return err
	}
	var opts []cniclient.NamespaceOpts
	if len(ports) > 0 {
		pms := make([]cniclient.PortMapping, 0, len(ports))
		for _, p := range ports {
			if p.HostPort <= 0 {
				continue // ephemeral binding not assigned yet
			}
			proto := strings.ToUpper(p.Protocol)
			if proto == "" {
				proto = "TCP"
			}
			pms = append(pms, cniclient.PortMapping{
				HostPort:      int32(p.HostPort),
				ContainerPort: int32(p.ContainerPort),
				Protocol:      proto,
				HostIP:        p.HostIP,
			})
		}
		opts = append(opts, cniclient.WithCapabilityPortMap(pms))
	}
	return c.Remove(ctx, id, netnsPath, opts...)
}

// jsonUnmarshal is a thin alias keeping runtime.go free of a second direct
// encoding/json import at call sites above.
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

// --- secondary interfaces ---------------------------------------------------

// go-cni names interfaces by position in its own network list, so every
// single-conflist instance above yields eth0. Secondary networks (compose
// services on several networks, docker network connect) need eth1, eth2...:
// they go through libcni directly with an explicit interface name. No port
// mappings there — published ports live on the primary network.
var extraCNI = cnilibrary.NewCNIConfig([]string{cniBinDir}, &invoke.DefaultExec{
	RawExec:       &invoke.RawExec{Stderr: os.Stderr},
	PluginDecoder: version.PluginDecoder{},
})

func attachExtraNetwork(ctx context.Context, netName, id, netnsPath, ifName, staticIP string) (cniAddrs, error) {
	conflist, err := findConflistForNetwork(netName)
	if err != nil {
		return cniAddrs{}, err
	}
	// A dual-stack network needs IPv6 forwarding (eth0 keeping its router
	// advertisements) and its IPv6 masquerade or isolation before the
	// bridge plugin runs, on a secondary endpoint too.
	if cl, cerr := readNetworkConflist(netName); cerr == nil && anyIPv6(networkSubnets(cl)) {
		if err := ensureNetworkMasquerade(netName); err != nil {
			log.G(ctx).WithError(err).Warnf("[cni] masquerade rule for %s", netName)
		}
	}
	data, err := os.ReadFile(conflist)
	if err == nil && staticIP != "" {
		data, err = staticConflistBytes(data)
	}
	if err != nil {
		return cniAddrs{}, fmt.Errorf("cni config %s: %w", netName, err)
	}
	list, err := cnilibrary.ConfListFromBytes(data)
	if err != nil {
		return cniAddrs{}, fmt.Errorf("cni config %s: %w", netName, err)
	}
	list = withoutLoopback(list)
	rt := &cnilibrary.RuntimeConf{ContainerID: id, NetNS: netnsPath, IfName: ifName}
	if staticIP != "" {
		rt.Args = [][2]string{{"IgnoreUnknown", "1"}, {"IP", staticIP}}
	}
	raw, err := extraCNI.AddNetworkList(ctx, list, rt)
	if err != nil {
		// A failed ADD may leave a veth or an IPAM lease behind; the CNI
		// spec has the runtime issue DEL for it.
		extraCNI.DelNetworkList(context.WithoutCancel(ctx), list, rt) //nolint:errcheck
		return cniAddrs{}, fmt.Errorf("cni setup %s (%s): %w", netName, ifName, err)
	}
	res, err := types100.NewResultFromResult(raw)
	if err != nil {
		return cniAddrs{}, fmt.Errorf("cni result %s: %w", netName, err)
	}
	addrs := extraResultAddresses(res, ifName)
	log.G(ctx).WithField("network", netName).Debugf("[cni] %s attached %s ip=%s ipv6=%s", id[:12], ifName, addrs.IP, addrs.IPv6)
	return addrs, nil
}

func detachExtraNetwork(ctx context.Context, netName, id, netnsPath, ifName string) error {
	conflist, err := findConflistForNetwork(netName)
	if err != nil {
		return err
	}
	list, err := cnilibrary.ConfListFromFile(conflist)
	if err != nil {
		return err
	}
	return extraCNI.DelNetworkList(ctx, withoutLoopback(list), &cnilibrary.RuntimeConf{ContainerID: id, NetNS: netnsPath, IfName: ifName})
}

// withoutLoopback drops the loopback plugin from a network's list for an
// endpoint that is not the container's whole network. Its DEL brings lo
// down whatever interface it is given: disconnecting a second network cut
// the container's 127.0.0.1 (a k3s node's API load balancer on
// 127.0.0.1:6444 hung until the node restarted).
func withoutLoopback(list *cnilibrary.NetworkConfigList) *cnilibrary.NetworkConfigList {
	out := *list
	out.Plugins = nil
	for _, p := range list.Plugins {
		if p.Network.Type != "loopback" {
			out.Plugins = append(out.Plugins, p)
		}
	}
	return &out
}

// detachPrimaryLive removes a running container's primary endpoint (eth0
// and its published ports) and leaves lo and the other endpoints alone —
// a live `docker network disconnect` from the primary network.
func detachPrimaryLive(ctx context.Context, netName, id, netnsPath string, ports []cniPortMapping) error {
	conflist, err := findConflistForNetwork(netName)
	if err != nil {
		return err
	}
	list, err := cnilibrary.ConfListFromFile(conflist)
	if err != nil {
		return err
	}
	rt := &cnilibrary.RuntimeConf{ContainerID: id, NetNS: netnsPath, IfName: "eth0"}
	var pms []map[string]interface{}
	for _, p := range ports {
		if p.HostPort <= 0 {
			continue
		}
		proto := strings.ToLower(p.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		pms = append(pms, map[string]interface{}{"hostPort": p.HostPort, "containerPort": p.ContainerPort, "protocol": proto, "hostIP": p.HostIP})
	}
	if len(pms) > 0 {
		rt.CapabilityArgs = map[string]interface{}{"portMappings": pms}
	}
	return extraCNI.DelNetworkList(ctx, withoutLoopback(list), rt)
}

// --- --network none -----------------------------------------------------------

// loopbackOnlyConf is the whole network of a --network none container: the
// loopback plugin bringing lo up in its fresh netns, and nothing else.
var loopbackOnlyConf = []byte(`{"cniVersion":"1.0.0","name":"none","plugins":[{"type":"loopback"}]}`)

func attachLoopbackOnly(ctx context.Context, id, netnsPath string) error {
	list, err := cnilibrary.ConfListFromBytes(loopbackOnlyConf)
	if err != nil {
		return err
	}
	if _, err := extraCNI.AddNetworkList(ctx, list, &cnilibrary.RuntimeConf{ContainerID: id, NetNS: netnsPath, IfName: "lo"}); err != nil {
		return fmt.Errorf("cni loopback: %w", err)
	}
	return nil
}

func detachLoopbackOnly(ctx context.Context, id, netnsPath string) error {
	list, err := cnilibrary.ConfListFromBytes(loopbackOnlyConf)
	if err != nil {
		return err
	}
	return extraCNI.DelNetworkList(ctx, list, &cnilibrary.RuntimeConf{ContainerID: id, NetNS: netnsPath, IfName: "lo"})
}

// --- fast endpoint teardown -------------------------------------------------

// ensureNamedNetNS recreates the container's named netns when a previous
// stop released it (see releaseEndpoints).
func ensureNamedNetNS(name string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(filepath.Join(netnsDir, name), &st); err == nil && st.Type == unix.NSFS_MAGIC {
		return nil
	}
	_, err := createNamedNetNS(name)
	return err
}

// networkConflist is the part of a conflist teardown decisions need.
type networkConflist struct {
	Internal bool `json:"anvilInternal"`
	Plugins  []struct {
		Type   string `json:"type"`
		Bridge string `json:"bridge"`
		IPMasq bool   `json:"ipMasq"`
		IPAM   struct {
			Ranges [][]struct {
				Subnet string `json:"subnet"`
			} `json:"ranges"`
		} `json:"ipam"`
	} `json:"plugins"`
}

func readNetworkConflist(netName string) (*networkConflist, error) {
	path, err := findConflistForNetwork(netName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cl networkConflist
	if err := json.Unmarshal(data, &cl); err != nil {
		return nil, err
	}
	return &cl, nil
}

// networkUsesPluginMasq reports whether the bridge plugin masquerades per
// container (networks created before the static rule).
func networkUsesPluginMasq(netName string) bool {
	cl, err := readNetworkConflist(netName)
	if err != nil {
		return true // unknown: take the conservative teardown order
	}
	for _, p := range cl.Plugins {
		if p.Type == "bridge" {
			return p.IPMasq
		}
	}
	return false
}

func networkSubnets(cl *networkConflist) []string {
	var out []string
	for _, p := range cl.Plugins {
		for _, rs := range p.IPAM.Ranges {
			for _, r := range rs {
				if r.Subnet != "" {
					out = append(out, r.Subnet)
				}
			}
		}
	}
	return out
}

// ensureNetworkMasquerade installs the network's outbound masquerade rules,
// one per subnet in its family's table (idempotent: one -C when present).
// A dual-stack network first gets IPv6 forwarding; an --internal one gets
// isolation instead of NAT, in both families.
func ensureNetworkMasquerade(netName string) error {
	cl, err := readNetworkConflist(netName)
	if err != nil {
		return err
	}
	subnets := networkSubnets(cl)
	// A forwarding failure is reported, but must not cost the network
	// its IPv4 NAT or its isolation.
	var fwdErr error
	if anyIPv6(subnets) {
		fwdErr = ensureIPv6Forwarding()
	}
	if cl.Internal {
		if err := ensureInternalIsolation(cl); err != nil {
			return err
		}
		return fwdErr
	}
	for _, p := range cl.Plugins {
		if p.Type == "bridge" && p.IPMasq {
			return fwdErr // the plugin masquerades itself
		}
	}
	for _, subnet := range subnets {
		if err := ensureXtablesRule(xtablesFor(subnet), masqueradeRule(netName, subnet)); err != nil {
			return err
		}
	}
	return fwdErr
}

// removeNetworkMasquerade drops the rules when the network goes away.
func removeNetworkMasquerade(netName string) {
	cl, err := readNetworkConflist(netName)
	if err != nil {
		return
	}
	subnets := networkSubnets(cl)
	for _, subnet := range subnets {
		rule := masqueradeRule(netName, subnet)
		args := append([]string{rule[0], rule[1], "-D"}, rule[2:]...)
		exec.Command(xtablesFor(subnet), args...).Run() //nolint:errcheck — best effort
	}
	if cl.Internal {
		isolationMu.Lock()
		defer isolationMu.Unlock()
		bridge := networkBridge(cl)
		for _, bin := range isolationTables(subnets) {
			for _, rule := range append(isolationRules(bridge), internalInputRule(bridge)) {
				deleteXtablesRuleAll(bin, rule)
			}
		}
	}
}

// deleteXtablesRuleAll removes every copy of rule (best effort).
func deleteXtablesRuleAll(bin string, rule []string) {
	args := append([]string{rule[0], rule[1], "-D"}, rule[2:]...)
	for i := 0; i < 8; i++ {
		if exec.Command(bin, args...).Run() != nil {
			return
		}
	}
}

// isolationTables are the tables an --internal network's isolation goes
// into: ip6tables too once it is dual-stack (IPv6 forwarding is on then).
func isolationTables(subnets []string) []string {
	if anyIPv6(subnets) {
		return []string{"iptables", "ip6tables"}
	}
	return []string{"iptables"}
}

// ipv6Forwarding guards the one-time switch to IPv6 forwarding (a resumed
// snapshot keeps it, a cold boot starts over with the flag).
var ipv6Forwarding struct {
	sync.Mutex
	on bool
}

// ensureIPv6Forwarding turns IPv6 forwarding on for dual-stack networks.
// With forwarding on, the kernel ignores router advertisements and purges
// the routes it learned from them — eth0 would lose the default route (and,
// on expiry, the SLAAC address) the macOS NAT advertises — unless the
// interface's accept_ra is 2, so that is set first. The bridge plugin
// turns forwarding on by itself for an IPv6 gateway, which is why this
// runs before every CNI ADD on a dual-stack network.
func ensureIPv6Forwarding() error {
	ipv6Forwarding.Lock()
	defer ipv6Forwarding.Unlock()
	if ipv6Forwarding.on {
		return nil
	}
	for _, s := range []struct{ path, value string }{
		{"/proc/sys/net/ipv6/conf/eth0/accept_ra", "2"},
		{"/proc/sys/net/ipv6/conf/all/forwarding", "1"},
	} {
		if err := os.WriteFile(s.path, []byte(s.value), 0o644); err != nil {
			return fmt.Errorf("ipv6 forwarding: %w", err)
		}
	}
	ipv6Forwarding.on = true
	return nil
}

// isolationMu serializes isolation rule changes: two starts reordering at
// once could leave duplicates that outlive the network.
var isolationMu sync.Mutex

// internalInputRule keeps an --internal network away from the Mac's
// localhost (host.docker.internal is redirected to a guest-local proxy,
// which FORWARD rules never see).
func internalInputRule(bridge string) []string {
	return []string{"-t", "filter", "INPUT", "-i", bridge, "-p", "tcp", "--dport", hostLoopbackProxyPt,
		"-m", "comment", "--comment", "anvil-internal " + bridge, "-j", "DROP"}
}

func networkBridge(cl *networkConflist) string {
	for _, p := range cl.Plugins {
		if p.Type == "bridge" {
			return p.Bridge
		}
	}
	return ""
}

// isolationRules keep an --internal network's traffic on its bridge, as
// Docker's DOCKER-ISOLATION chains do: nothing is forwarded in or out.
// Containers still reach each other and the VM (DNS, the port proxy).
func isolationRules(bridge string) [][]string {
	comment := "anvil-internal " + bridge
	return [][]string{
		{"-t", "filter", "FORWARD", "-i", bridge, "!", "-o", bridge,
			"-m", "comment", "--comment", comment, "-j", "DROP"},
		{"-t", "filter", "FORWARD", "-o", bridge, "!", "-i", bridge,
			"-m", "comment", "--comment", comment, "-j", "DROP"},
	}
}

// ensureInternalIsolation keeps the isolation rules at the top of FORWARD,
// ahead of the CNI plugins' ACCEPT rules (no change when they already are),
// in ip6tables as well for a dual-stack network: an internal network must
// not leak out over IPv6 either.
func ensureInternalIsolation(cl *networkConflist) error {
	bridge := networkBridge(cl)
	if bridge == "" {
		return fmt.Errorf("internal network without a bridge")
	}
	isolationMu.Lock()
	defer isolationMu.Unlock()
	for _, bin := range isolationTables(networkSubnets(cl)) {
		if err := ensureIsolationRules(bin, bridge); err != nil {
			return err
		}
	}
	return nil
}

// ensureIsolationRules installs one table's isolation rules; the caller
// holds isolationMu.
func ensureIsolationRules(bin, bridge string) error {
	if err := ensureXtablesRule(bin, internalInputRule(bridge)); err != nil {
		return err
	}
	out, err := exec.Command(bin, "-t", "filter", "-S", "FORWARD").Output()
	if err != nil {
		return fmt.Errorf("%s -S FORWARD: %w", bin, err)
	}
	if isolationOnTop(string(out), "anvil-internal "+bridge+`"`) {
		return nil
	}
	rules := isolationRules(bridge)
	for _, rule := range rules {
		deleteXtablesRuleAll(bin, rule)
	}
	for i := len(rules) - 1; i >= 0; i-- {
		rule := rules[i]
		insert := append([]string{rule[0], rule[1], "-I", rule[2], "1"}, rule[3:]...)
		if out, err := exec.Command(bin, insert...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s %s: %v: %s", bin, strings.Join(insert, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// fitrim is FITRIM, _IOWR('X', 121, struct fstrim_range).
const fitrim = 0xc0185879

// trimFilesystem discards the unused blocks of the file system at path
// (FITRIM, what fstrim does) and returns how many bytes it trimmed.
func trimFilesystem(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := struct{ start, length, minLen uint64 }{0, ^uint64(0), 0}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fitrim, uintptr(unsafe.Pointer(&r))); errno != 0 {
		return 0, errno
	}
	return r.length, nil
}

// ipInNetworkSubnet reports whether ip lies in one of network's subnets.
func ipInNetworkSubnet(network string, ip net.IP) bool {
	cl, err := readNetworkConflist(network)
	if err != nil {
		return false
	}
	for _, s := range networkSubnets(cl) {
		if _, n, err := net.ParseCIDR(s); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
