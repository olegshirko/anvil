//go:build linux

package main

// The kernel half of rung 6a.1: lo, the bridge, the veth pair and the
// nftables NAT rules for the default bridge network, all in-process. The
// plugin path in runtime_linux.go stays the fallback and keeps the rest of
// the network features.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/containerd/log"
	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	unix "golang.org/x/sys/unix"
)

// --- eligibility ------------------------------------------------------------

// nativeEligible reports whether the endpoint should be created in-process,
// and hands back the metadata it already read (the caller needs LastIPs for
// the preferred address, and a second read would cost the hot path a file).
// Anything the 6a.1 subset does not cover — a user network, a static address,
// an address range, IPv6, --internal, secondary networks, or a forced plugin
// mode — keeps the plugin path.
func nativeEligible(netName, ns, id string) (*nativeSpec, *containerMeta, bool) {
	if cniMode() == "plugin" {
		return nil, nil, false
	}
	if netName != defaultBridgeNetwork {
		return nil, nil, false
	}
	if networkIsInternal(netName) || loadNetworkPool(netName) != nil {
		return nil, nil, false
	}
	// Secondary networks, --network host/none and container network modes are
	// all outside the subset; the primary endpoint of a multi-network
	// container goes through the plugins together with its secondaries.
	meta, err := loadContainerMeta(ns, id)
	if err != nil || len(meta.Networks) != 1 || meta.Networks[0] != netName {
		return nil, nil, false
	}
	if meta.NetworkIPs[netName] != "" {
		return nil, nil, false // a static address: host-local takes it via CNI_ARGS
	}
	// The generator writes the canonical file name for every network; the
	// plugin path still has findConflistForNetwork for legacy files.
	data, err := os.ReadFile(cniConflistPath(netName))
	if err != nil {
		return nil, nil, false
	}
	spec, err := parseNativeSpec(data)
	if err != nil {
		return nil, nil, false
	}
	return &spec, meta, true
}

// --- attach / detach --------------------------------------------------------

// attachNetworkNative creates the endpoint in-process. On any failure it
// undoes what it did, so the caller can fall back to the plugin path without
// leaving a veth, a lease or a NAT rule behind.
func attachNetworkNative(ctx context.Context, spec *nativeSpec, meta *containerMeta, ns, id, netnsPath string, ports []cniPortMapping) (addrs cniAddrs, err error) {
	r, err := newIPRange(spec.IPv4)
	if err != nil {
		return cniAddrs{}, err
	}
	hostNS, err := netnsHandle()
	if err != nil {
		return cniAddrs{}, fmt.Errorf("host netns: %w", err)
	}
	defer closeNetnsHandle(hostNS)

	// The address the container had before is only a preference: another
	// container may hold it now, and the allocator skips taken leases.
	preferred := v4(net.ParseIP(meta.LastIPs[spec.Network]))
	ip, err := hostLocalAllocate(spec.Network, r, id, "eth0", preferred)
	if err != nil {
		return cniAddrs{}, err
	}
	ok := false
	defer func() {
		if !ok {
			deleteNativeDNAT(id)
			deleteEndpointLink(netnsPath)
			hostLocalRelease(spec.Network, id, "eth0") //nolint:errcheck — rollback
			removeNativeMarker(ns, id)
		}
	}()

	if err := ensureIPv4Forwarding(); err != nil {
		return cniAddrs{}, err
	}
	if err := ensureNativeBridge(spec.Bridge, r); err != nil {
		return cniAddrs{}, err
	}
	if err := ensureNativeMasq(spec.Network, r); err != nil {
		return cniAddrs{}, err
	}
	mac, err := createEndpoint(spec.Bridge, r, ip, netnsPath, hostNS)
	if err != nil {
		return cniAddrs{}, err
	}
	if err := ensureNativeDNAT(id, ip, ports); err != nil {
		return cniAddrs{}, err
	}
	if err := writeNativeMarker(ns, id, nativeEndpoint{Network: spec.Network, IP: ip.String()}); err != nil {
		return cniAddrs{}, fmt.Errorf("native endpoint marker: %w", err)
	}
	ok = true
	log.G(ctx).WithField("network", spec.Network).Debugf("[cni] %s attached natively ip=%s", truncateID(id), ip)
	return cniAddrs{IP: ip.String(), Mac: mac}, nil
}

// detachNetworkNative releases a native endpoint: its port rules, the veth
// (while the netns still exists; otherwise it died with the netns) and the
// address lease. Every step is idempotent.
func detachNetworkNative(ctx context.Context, netName, ns, id, netnsPath string, ports []cniPortMapping) error {
	deleteNativeDNAT(id)
	deleteEndpointLink(netnsPath)
	err := hostLocalRelease(netName, id, "eth0")
	removeNativeMarker(ns, id)
	if err != nil {
		return fmt.Errorf("release %s: %w", truncateID(id), err)
	}
	log.G(ctx).WithField("network", netName).Debugf("[cni] %s detached natively", truncateID(id))
	return nil
}

// --- netns ------------------------------------------------------------------

// netnsHandle opens the agent's network namespace: the host end of a new veth
// pair is created into it while the container side is being made. The path
// is the process's main-thread namespace: the agent never leaves its own.
func netnsHandle() (int, error) {
	return unix.Open("/proc/self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
}

func closeNetnsHandle(fd int) {
	if fd >= 0 {
		_ = unix.Close(fd)
	}
}

// inNetNS runs fn on a thread switched into netnsPath. The thread is locked
// for the whole call; if the switch back fails the thread is discarded with
// the goroutine rather than returned to the pool in the wrong namespace.
func inNetNS(netnsPath string, fn func() error) error {
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		inOrig := true
		defer func() {
			if inOrig {
				runtime.UnlockOSThread()
			}
		}()
		self := fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid())
		orig, err := os.Open(self)
		if err != nil {
			errCh <- err
			return
		}
		defer orig.Close()
		target, err := os.Open(netnsPath)
		if err != nil {
			errCh <- err
			return
		}
		defer target.Close()
		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			errCh <- fmt.Errorf("enter netns %s: %w", netnsPath, err)
			return
		}
		inOrig = false
		ferr := func() (err error) {
			// A panic must not leave this thread in the container's netns.
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("network setup panicked: %v", r)
				}
			}()
			return fn()
		}()
		if err := unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET); err != nil {
			errCh <- fmt.Errorf("restore netns: %w", err)
			return
		}
		inOrig = true
		errCh <- ferr
	}()
	return <-errCh
}

// --- interfaces -------------------------------------------------------------

var ipv4Forwarding struct {
	sync.Once
	err error
}

// ensureIPv4Forwarding turns IPv4 forwarding on once per boot. The bridge
// plugin does it on every ADD; containers cannot talk to eth0 without it.
func ensureIPv4Forwarding() error {
	ipv4Forwarding.Do(func() {
		if data, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err == nil && strings.TrimSpace(string(data)) == "1" {
			return
		}
		ipv4Forwarding.err = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644)
	})
	return ipv4Forwarding.err
}

// ensureNativeBridge creates the network's bridge if needed, gives it the
// gateway address and brings it up, as the bridge plugin's setupBridge does.
func ensureNativeBridge(name string, r ipRange) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("bridge %s: %w", name, err)
		}
		br := &netlink.Bridge{LinkAttrs: netlink.NewLinkAttrs()}
		br.Name = name
		addErr := netlink.LinkAdd(br)
		if link, err = netlink.LinkByName(name); err != nil {
			if addErr != nil {
				return fmt.Errorf("add bridge %s: %w", name, addErr)
			}
			return fmt.Errorf("bridge %s: %w", name, err)
		}
	}
	if _, isBridge := link.(*netlink.Bridge); !isBridge {
		return fmt.Errorf("%s exists but is not a bridge", name)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("up bridge %s: %w", name, err)
	}
	gw := &net.IPNet{IP: r.Gateway, Mask: r.Net.Mask}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("bridge %s addresses: %w", name, err)
	}
	for _, a := range addrs {
		if a.IPNet != nil && a.IPNet.IP.Equal(gw.IP) {
			return nil
		}
	}
	if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: gw}); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("bridge %s address %s: %w", name, gw, err)
	}
	return nil
}

// randomVethName is the plugin's host-side naming: "veth" plus 4 random
// bytes, which NetworkManager leaves alone.
func randomVethName() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("veth%x", b), nil
}

// createEndpoint builds eth0 inside the container netns (lo up, veth with the
// address and the default route) and attaches the host end to the bridge
// with hairpin mode on. It returns the container MAC.
func createEndpoint(bridge string, r ipRange, ip net.IP, netnsPath string, hostNSFD int) (string, error) {
	hostVeth, err := randomVethName()
	if err != nil {
		return "", err
	}
	var mac string
	err = inNetNS(netnsPath, func() error {
		lo, err := netlink.LinkByName("lo")
		if err != nil {
			return fmt.Errorf("lo: %w", err)
		}
		if err := netlink.LinkSetUp(lo); err != nil {
			return fmt.Errorf("up lo: %w", err)
		}
		// A leftover endpoint from a crashed run must not block this attach.
		if old, lerr := netlink.LinkByName("eth0"); lerr == nil {
			_ = netlink.LinkDel(old)
		}
		veth := &netlink.Veth{
			LinkAttrs:     netlink.LinkAttrs{Name: "eth0"},
			PeerName:      hostVeth,
			PeerNamespace: netlink.NsFd(hostNSFD),
		}
		if err := netlink.LinkAdd(veth); err != nil {
			return fmt.Errorf("add veth: %w", err)
		}
		link, err := netlink.LinkByName("eth0")
		if err != nil {
			return fmt.Errorf("eth0: %w", err)
		}
		addr := &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: r.Net.Mask}}
		if err := netlink.AddrAdd(link, addr); err != nil {
			return fmt.Errorf("add %s: %w", addr, err)
		}
		// The interface must be up before the default route: a route via a
		// gateway on a down interface is refused with ENETUNREACH.
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("up eth0: %w", err)
		}
		route := netlink.Route{
			Dst:       &net.IPNet{IP: net.IP{0, 0, 0, 0}, Mask: net.CIDRMask(0, 32)},
			Gw:        r.Gateway,
			LinkIndex: link.Attrs().Index,
		}
		if err := netlink.RouteAdd(&route); err != nil {
			return fmt.Errorf("default route via %s: %w", r.Gateway, err)
		}
		mac = link.Attrs().HardwareAddr.String()
		return nil
	})
	if err != nil {
		return "", err
	}
	hostLink, err := netlink.LinkByName(hostVeth)
	if err != nil {
		return "", fmt.Errorf("host veth %s: %w", hostVeth, err)
	}
	br, err := netlink.LinkByName(bridge)
	if err != nil {
		return "", fmt.Errorf("bridge %s: %w", bridge, err)
	}
	if err := netlink.LinkSetMaster(hostLink, br); err != nil {
		return "", fmt.Errorf("attach %s to %s: %w", hostVeth, bridge, err)
	}
	if err := netlink.LinkSetHairpin(hostLink, true); err != nil {
		return "", fmt.Errorf("hairpin %s: %w", hostVeth, err)
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		return "", fmt.Errorf("up %s: %w", hostVeth, err)
	}
	return mac, nil
}

// deleteEndpointLink removes eth0 while the container netns still exists.
// Once the netns is gone the kernel has already deleted both veth ends.
func deleteEndpointLink(netnsPath string) {
	if netnsPath == "" {
		return
	}
	if _, err := os.Stat(netnsPath); err != nil {
		return
	}
	_ = inNetNS(netnsPath, func() error {
		link, err := netlink.LinkByName("eth0")
		if err != nil {
			return nil // already gone
		}
		if _, isVeth := link.(*netlink.Veth); !isVeth {
			return nil // not ours
		}
		_ = netlink.LinkDel(link)
		return nil
	})
}

// --- nftables ---------------------------------------------------------------

const (
	nativeNftTable = "anvil"
	nftChainPre    = "pre"
	nftChainOut    = "out"
	nftChainPost   = "post"
	nftChainDNAT   = "dnat"
)

var (
	nftMu       sync.Mutex
	nftBaseOK   bool
	nftMasqDone = map[string]bool{}
)

func masqTag(network string) string { return "anvil-masq " + network }
func dnatTag(id string) string      { return "anvil-dnat " + id }

func nftConn() (*nftables.Conn, error) {
	return nftables.New()
}

// ensureNftBase makes sure our table exists with the two DNAT entry chains,
// the masquerade chain and the shared DNAT chain. Everything is cached after
// the first success: the table is process-independent kernel state.
func ensureNftBase(c *nftables.Conn) (*nftables.Table, error) {
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: nativeNftTable}
	if nftBaseOK {
		return table, nil
	}
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, fmt.Errorf("list nft tables: %w", err)
	}
	exists := false
	for _, t := range tables {
		if t.Name == nativeNftTable {
			exists = true
			break
		}
	}
	if !exists {
		c.AddTable(table)
		if err := c.Flush(); err != nil {
			return nil, fmt.Errorf("add nft table: %w", err)
		}
	}
	chains, err := c.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, fmt.Errorf("list nft chains: %w", err)
	}
	have := map[string]bool{}
	for _, ch := range chains {
		if ch.Table != nil && ch.Table.Name == nativeNftTable {
			have[ch.Name] = true
		}
	}
	if !have[nftChainPre] {
		c.AddChain(&nftables.Chain{Name: nftChainPre, Table: table, Type: nftables.ChainTypeNAT,
			Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest})
	}
	if !have[nftChainOut] {
		c.AddChain(&nftables.Chain{Name: nftChainOut, Table: table, Type: nftables.ChainTypeNAT,
			Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityNATDest})
	}
	if !have[nftChainPost] {
		c.AddChain(&nftables.Chain{Name: nftChainPost, Table: table, Type: nftables.ChainTypeNAT,
			Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource})
	}
	if !have[nftChainDNAT] {
		c.AddChain(&nftables.Chain{Name: nftChainDNAT, Table: table})
	}
	if err := c.Flush(); err != nil {
		return nil, fmt.Errorf("add nft chains: %w", err)
	}
	// The two local-destination guards are static; the per-container rules
	// live in the shared chain behind them.
	for _, name := range []string{nftChainPre, nftChainOut} {
		if err := ensureJumpToDNAT(c, table, name); err != nil {
			return nil, err
		}
	}
	if err := c.Flush(); err != nil {
		return nil, fmt.Errorf("add nft jumps: %w", err)
	}
	nftBaseOK = true
	return table, nil
}

// ensureJumpToDNAT keeps one "local destination -> dnat chain" rule per entry
// chain, adding it only when a previous run did not.
func ensureJumpToDNAT(c *nftables.Conn, table *nftables.Table, chain string) error {
	rules, err := c.GetRules(table, &nftables.Chain{Name: chain})
	if err != nil {
		return fmt.Errorf("list nft %s rules: %w", chain, err)
	}
	for _, r := range rules {
		for _, e := range r.Exprs {
			if v, ok := e.(*expr.Verdict); ok && v.Kind == expr.VerdictJump && v.Chain == nftChainDNAT {
				return nil
			}
		}
	}
	c.AddRule(&nftables.Rule{
		Table: table,
		Chain: &nftables.Chain{Name: chain, Table: table},
		Exprs: []expr.Any{
			&expr.Fib{Register: 1, FlagDADDR: true, ResultADDRTYPE: true},
			// nft's fib address type is a host-endian u32 (fib_addr_type
			// in userspace, a plain u32 store in the kernel).
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(uint32(unix.RTN_LOCAL))},
			&expr.Verdict{Kind: expr.VerdictJump, Chain: nftChainDNAT},
		},
	})
	return nil
}

// ensureNativeMasq installs the network's outbound masquerade once per
// network: source inside the subnet, destination outside it.
func ensureNativeMasq(network string, r ipRange) error {
	nftMu.Lock()
	defer nftMu.Unlock()
	if nftMasqDone[network] {
		return nil
	}
	c, err := nftConn()
	if err != nil {
		return err
	}
	table, err := ensureNftBase(c)
	if err != nil {
		nftBaseOK = false
		return err
	}
	rules, err := c.GetRules(table, &nftables.Chain{Name: nftChainPost, Table: table})
	if err != nil {
		nftBaseOK = false
		return fmt.Errorf("list nft post rules: %w", err)
	}
	for _, rule := range rules {
		if string(rule.UserData) == masqTag(network) {
			nftMasqDone[network] = true
			return nil
		}
	}
	c.AddRule(&nftables.Rule{
		Table:    table,
		Chain:    &nftables.Chain{Name: nftChainPost, Table: table},
		UserData: []byte(masqTag(network)),
		Exprs:    masqExprs(r),
	})
	if err := c.Flush(); err != nil {
		nftBaseOK = false
		return fmt.Errorf("add nft masquerade for %s: %w", network, err)
	}
	nftMasqDone[network] = true
	return nil
}

func masqExprs(r ipRange) []expr.Any {
	subnet := r.Net.IP
	xor := []byte{0, 0, 0, 0}
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: r.Net.Mask, Xor: xor},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: subnet},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: r.Net.Mask, Xor: xor},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: subnet},
		&expr.Masq{},
	}
}

// removeNativeMasq drops the network's masquerade rule when the network goes
// away. It never creates the table: a network that never used the native path
// has nothing here.
func removeNativeMasq(network string) {
	nftMu.Lock()
	defer nftMu.Unlock()
	delete(nftMasqDone, network)
	c, err := nftConn()
	if err != nil {
		return
	}
	table, exists, err := lookupNftBase(c)
	if err != nil || !exists {
		nftBaseOK = false
		return
	}
	_ = deleteTaggedRules(c, table, nftChainPost, masqTag(network))
}

// lookupNftBase returns our table when it exists, without creating anything.
func lookupNftBase(c *nftables.Conn) (*nftables.Table, bool, error) {
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, false, err
	}
	for _, t := range tables {
		if t.Name == nativeNftTable {
			return t, true, nil
		}
	}
	return nil, false, nil
}

// ensureNativeDNAT replaces the container's published-port rules with the
// current mapping. Only traffic to a local address reaches the dnat chain.
func ensureNativeDNAT(id string, ip net.IP, ports []cniPortMapping) error {
	nftMu.Lock()
	defer nftMu.Unlock()
	c, err := nftConn()
	if err != nil {
		return err
	}
	table, err := ensureNftBase(c)
	if err != nil {
		nftBaseOK = false
		return err
	}
	if err := deleteTaggedRules(c, table, nftChainDNAT, dnatTag(id)); err != nil {
		nftBaseOK = false
		return err
	}
	rules := nativeDNATs(ports)
	for _, m := range rules {
		c.AddRule(&nftables.Rule{
			Table:    table,
			Chain:    &nftables.Chain{Name: nftChainDNAT, Table: table},
			UserData: []byte(dnatTag(id)),
			Exprs:    dnatExprs(m, ip),
		})
	}
	if len(rules) == 0 {
		return nil
	}
	if err := c.Flush(); err != nil {
		nftBaseOK = false
		return fmt.Errorf("add nft port rules for %s: %w", id[:12], err)
	}
	return nil
}

// deleteNativeDNAT drops a container's port rules; best effort because it
// runs on teardown paths that must not stop half-way.
func deleteNativeDNAT(id string) {
	nftMu.Lock()
	defer nftMu.Unlock()
	c, err := nftConn()
	if err != nil {
		return
	}
	table, err := ensureNftBase(c)
	if err != nil {
		nftBaseOK = false
		return
	}
	if err := deleteTaggedRules(c, table, nftChainDNAT, dnatTag(id)); err != nil {
		nftBaseOK = false
	}
}

// deleteTaggedRules flushes every rule of chain whose user data is tag.
func deleteTaggedRules(c *nftables.Conn, table *nftables.Table, chain, tag string) error {
	rules, err := c.GetRules(table, &nftables.Chain{Name: chain, Table: table})
	if err != nil {
		return fmt.Errorf("list nft %s rules: %w", chain, err)
	}
	deleted := false
	for _, rule := range rules {
		if string(rule.UserData) != tag {
			continue
		}
		if err := c.DelRule(rule); err != nil {
			return err
		}
		deleted = true
	}
	if !deleted {
		return nil
	}
	return c.Flush()
}

var nftProtos = map[string]byte{
	"tcp":  unix.IPPROTO_TCP,
	"udp":  unix.IPPROTO_UDP,
	"sctp": unix.IPPROTO_SCTP,
}

func dnatExprs(m nativeDNAT, ip net.IP) []expr.Any {
	var out []expr.Any
	if host := v4(net.ParseIP(m.HostIP)); host != nil && !host.Equal(net.IPv4zero) {
		out = append(out,
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: host},
		)
	}
	return append(out,
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{nftProtos[m.Protocol]}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(uint16(m.HostPort))},
		&expr.Immediate{Register: 1, Data: v4(ip)},
		&expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(uint16(m.ContainerPort))},
		&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegProtoMin: 2},
	)
}
