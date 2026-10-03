package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Subnet and bridge allocation for anvil CNI networks.
//
// A network's subnet is 10.10.<octet>.0/24 with the octet derived from the
// project name, so a recreated project gets the same network. The hash has
// only 250 slots, though, and the bridge name is capped at 15 characters
// (IFNAMSIZ): without a check two projects could share a subnet (broken
// routing) or a bridge ("br-compose-test-" for every compose-test-*
// project — no isolation at all). The existing allocation of a network is
// kept; a new one takes its hashed slot, or the next free one.

// netAllocMu serializes allocation with the conflist write, so two
// concurrent creates cannot pick the same free slot.
var netAllocMu sync.Mutex

type netAlloc struct {
	subnet string // CIDR
	octet  int    // 10.10.<octet>.0/24 for hashed subnets, 0 for a user pool
	bridge string
}

// hashedSubnet is the subnet of a hashed slot.
func hashedSubnet(octet int) string {
	return fmt.Sprintf("10.10.%d.0/24", octet)
}

// allocFromConflist extracts the IPv4 subnet and bridge a conflist uses.
func allocFromConflist(cl cniConflist) (netAlloc, bool) {
	for _, p := range cl.Plugins {
		if p.Type != "bridge" {
			continue
		}
		for _, ranges := range p.IPAM.Ranges {
			for _, r := range ranges {
				if ip, _, err := net.ParseCIDR(r.Subnet); err != nil || ip.To4() == nil {
					continue
				}
				a := netAlloc{subnet: r.Subnet, bridge: p.Bridge}
				var o int
				if n, _ := fmt.Sscanf(r.Subnet, "10.10.%d.0/24", &o); n == 1 && hashedSubnet(o) == r.Subnet {
					a.octet = o
				}
				return a, true
			}
		}
	}
	return netAlloc{}, false
}

// cidrsOverlap reports whether two CIDRs share any address.
func cidrsOverlap(a, b string) bool {
	_, na, errA := net.ParseCIDR(a)
	_, nb, errB := net.ParseCIDR(b)
	if errA != nil || errB != nil {
		return false
	}
	return na.Contains(nb.IP) || nb.Contains(na.IP)
}

// pickNetAlloc returns the allocation for netName given every existing
// conflist (keyed by network name). preferred is the hashed octet; pool is
// the user-requested address pool, if any.
func pickNetAlloc(netName string, preferred int, existing map[string]cniConflist, pool *ipamPool) netAlloc {
	cur, have := allocFromConflist(existing[netName])
	usedOctets := map[int]bool{}
	usedBridges := map[string]bool{}
	for name, cl := range existing {
		if name == netName {
			continue
		}
		a, ok := allocFromConflist(cl)
		if !ok {
			continue
		}
		usedBridges[a.bridge] = true
		// A user pool inside 10.10.0.0/16 takes every slot it overlaps.
		for o := 1; o <= 250; o++ {
			if cidrsOverlap(a.subnet, hashedSubnet(o)) {
				usedOctets[o] = true
			}
		}
	}

	bridge := cur.bridge
	if !have {
		bridge = pickBridgeName(netName, usedBridges)
	}
	if pool != nil {
		return netAlloc{subnet: pool.Subnet, bridge: bridge}
	}
	if have && cur.octet != 0 {
		return cur
	}

	octet := preferred
	for i := 0; i < 250; i++ {
		o := (preferred-1+i)%250 + 1
		if !usedOctets[o] {
			octet = o
			break
		}
	}
	return netAlloc{subnet: hashedSubnet(octet), octet: octet, bridge: bridge}
}

// pickBridgeName keeps the readable "br-<name>" when it fits IFNAMSIZ and is
// free; otherwise it shortens the name and appends a hash of the full name.
func pickBridgeName(netName string, used map[string]bool) string {
	base := sanitizeCNIName(netName)
	if name := "br-" + base; len(name) <= 15 && !used[name] {
		return name
	}
	h := fnv.New32a()
	h.Write([]byte(netName))
	sum := h.Sum32()
	prefix := base
	if len(prefix) > 5 {
		prefix = prefix[:5]
	}
	for i := uint32(0); ; i++ {
		// br- + 5 + - + 6 hex = 15 characters.
		name := fmt.Sprintf("br-%s-%06x", prefix, (sum+i)&0xffffff)
		if !used[name] {
			return name
		}
	}
}

// networkSubnet returns the subnet recorded for a network, falling back to
// the hashed slot when it has no conflist.
func networkSubnet(netName string) string {
	if confs, err := loadCNIConflists(); err == nil {
		if a, ok := allocFromConflist(confs[netName]); ok {
			return a.subnet
		}
	}
	return hashedSubnet(projectSubnetOctet(netName))
}

// --- user-requested address pools (compose ipam.config) --------------------

// ipamPool is a validated IPv4 pool from POST /networks/create.
type ipamPool struct {
	Subnet     string `json:"Subnet"`
	Gateway    string `json:"Gateway"`
	RangeStart string `json:"RangeStart,omitempty"`
	RangeEnd   string `json:"RangeEnd,omitempty"`
}

// poolFromIPAM validates the request's IPAM config. It returns nil when no
// IPv4 subnet was requested (the hashed slot is used). IPv6 pools are
// ipv6PoolFromIPAM's.
func poolFromIPAM(cfgs []dockerIPAMConfig) (*ipamPool, error) {
	for _, c := range cfgs {
		if c.Subnet == "" {
			continue
		}
		ip, subnet, err := net.ParseCIDR(c.Subnet)
		if err != nil {
			return nil, fmt.Errorf("invalid subnet %q: %v", c.Subnet, err)
		}
		if ip.To4() == nil {
			continue
		}
		if ones, _ := subnet.Mask.Size(); ones > 30 {
			return nil, fmt.Errorf("subnet %s is too small", c.Subnet)
		}
		pool := &ipamPool{Subnet: subnet.String()}
		if c.Gateway != "" {
			gw := net.ParseIP(c.Gateway).To4()
			if gw == nil || !subnet.Contains(gw) {
				return nil, fmt.Errorf("gateway %s is not in subnet %s", c.Gateway, pool.Subnet)
			}
			pool.Gateway = gw.String()
		} else {
			pool.Gateway = offsetIP(subnet.IP, 1).String()
		}
		if c.IPRange != "" {
			_, rng, err := net.ParseCIDR(c.IPRange)
			if err != nil || rng.IP.To4() == nil || !subnet.Contains(rng.IP) || !subnet.Contains(lastIP(rng)) {
				return nil, fmt.Errorf("ip range %s is not in subnet %s", c.IPRange, pool.Subnet)
			}
			start, end := rng.IP.To4(), lastIP(rng)
			if start.Equal(subnet.IP.To4()) {
				start = offsetIP(start, 1)
			}
			if end.Equal(lastIP(subnet)) {
				end = offsetIP(end, -1)
			}
			pool.RangeStart, pool.RangeEnd = start.String(), end.String()
		}
		return pool, nil
	}
	return nil, nil
}

func offsetIP(ip net.IP, delta int) net.IP {
	v4 := ip.To4()
	n := uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3])
	n = uint32(int64(n) + int64(delta))
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)).To4()
}

// lastIP is the broadcast address of an IPv4 network.
func lastIP(n *net.IPNet) net.IP {
	ip := n.IP.To4()
	out := make(net.IP, 4)
	for i := range out {
		out[i] = ip[i] | ^n.Mask[len(n.Mask)-4+i]
	}
	return out
}

// poolOverlap returns the name of another network whose subnet overlaps the
// pool, if any — Docker refuses such a create the same way.
func poolOverlap(netName string, pool *ipamPool, existing map[string]cniConflist) string {
	for name, cl := range existing {
		if name == netName {
			continue
		}
		if a, ok := allocFromConflist(cl); ok && cidrsOverlap(a.subnet, pool.Subnet) {
			return name
		}
	}
	return ""
}

// The pool is persisted next to the network's labels: conflists are
// rewritten on every container create and restored after a cold boot, and
// both must keep the requested subnet. ".ipam", not ".json": the restore
// loop treats every .json there as a network's labels.
func networkPoolPath(name string) string {
	return filepath.Join(anvilRunDir, "networks", sanitizeCNIName(name)+".ipam")
}

func saveNetworkPool(name string, pool *ipamPool) error {
	path := networkPoolPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(pool)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func loadNetworkPool(name string) *ipamPool {
	data, err := os.ReadFile(networkPoolPath(name))
	if err != nil {
		return nil
	}
	var pool ipamPool
	if json.Unmarshal(data, &pool) != nil || pool.Subnet == "" {
		return nil
	}
	return &pool
}

func deleteNetworkPool(name string) {
	_ = os.Remove(networkPoolPath(name))
	_ = os.Remove(networkInternalPath(name))
	_ = os.Remove(networkIPv6Path(name))
}

// --- IPv6 (dual-stack networks) ----------------------------------------------

// A dual-stack network (--ipv6, compose enable_ipv6) gets a second, IPv6
// range next to its IPv4 one. Without a requested subnet that range is a
// ULA /64 under fd61:6e76:696c::/48: the global ID spells "anvil" in ASCII
// instead of RFC 4193's random 40 bits — the prefix never leaves the VM
// (egress is masqueraded behind eth0's address), so it only has to be
// stable and recognizable. The subnet ID follows the IPv4 slot:
// 10.10.<n>.0/24 pairs with fd61:6e76:696c:<n in hex>::/64. A network on
// a user IPv4 pool has no slot; it takes an ID in 0x100-0xffff (above every
// slot) from a hash of its name, probing past the IDs in use.
const ulaPrefix = "fd61:6e76:696c"

// ulaSubnet is the anvil ULA /64 with the given subnet ID.
func ulaSubnet(id int) string {
	return fmt.Sprintf("%s:%x::/64", ulaPrefix, id)
}

// ipv6Pool is the IPv6 side of a dual-stack network, persisted as
// <name>.ipv6 — the file's presence is what makes the network dual-stack.
// An empty Subnet means "allocate one": the ULA /64 is derived whenever the
// conflist is written (kept from the existing conflist on rewrites).
type ipv6Pool struct {
	Subnet     string `json:"Subnet,omitempty"`
	Gateway    string `json:"Gateway,omitempty"`
	RangeStart string `json:"RangeStart,omitempty"`
	RangeEnd   string `json:"RangeEnd,omitempty"`
}

// ipv6PoolFromIPAM returns the IPv6 side of a network create: nil when
// IPv6 is not enabled, an empty pool (allocate) when no IPv6 subnet was
// requested. Like Docker 27/28, IPv6 entries in the IPAM config are ignored
// — not refused — without EnableIPv6: the network stays IPv4-only.
func ipv6PoolFromIPAM(cfgs []dockerIPAMConfig, enable bool) (*ipv6Pool, error) {
	if !enable {
		return nil, nil
	}
	for _, c := range cfgs {
		if c.Subnet == "" {
			continue
		}
		ip, subnet, err := net.ParseCIDR(c.Subnet)
		if err != nil {
			return nil, fmt.Errorf("invalid subnet %q: %v", c.Subnet, err)
		}
		if ip.To4() != nil {
			continue
		}
		if ones, _ := subnet.Mask.Size(); ones > 126 {
			return nil, fmt.Errorf("subnet %s is too small", c.Subnet)
		}
		pool := &ipv6Pool{Subnet: subnet.String()}
		if c.Gateway != "" {
			gw := net.ParseIP(c.Gateway)
			if gw == nil || gw.To4() != nil || !subnet.Contains(gw) {
				return nil, fmt.Errorf("gateway %s is not in subnet %s", c.Gateway, pool.Subnet)
			}
			pool.Gateway = gw.String()
		} else {
			pool.Gateway = offsetIP6(subnet.IP, 1).String()
		}
		if c.IPRange != "" {
			rip, rng, err := net.ParseCIDR(c.IPRange)
			if err != nil || rip.To4() != nil || !subnet.Contains(rng.IP) || !subnet.Contains(lastIP6(rng)) {
				return nil, fmt.Errorf("ip range %s is not in subnet %s", c.IPRange, pool.Subnet)
			}
			// IPv6 has no broadcast address: only the subnet's first
			// address (the subnet-router anycast) is left out.
			start := rng.IP
			if start.Equal(subnet.IP) {
				start = offsetIP6(start, 1)
			}
			pool.RangeStart, pool.RangeEnd = start.String(), lastIP6(rng).String()
		}
		return pool, nil
	}
	return &ipv6Pool{}, nil
}

// hasIPv6Subnet reports whether an IPAM config requests an IPv6 subnet.
func hasIPv6Subnet(cfgs []dockerIPAMConfig) bool {
	for _, c := range cfgs {
		if ip, _, err := net.ParseCIDR(c.Subnet); err == nil && ip.To4() == nil {
			return true
		}
	}
	return false
}

// offsetIP6 adds a small non-negative delta to an IPv6 address.
func offsetIP6(ip net.IP, delta int) net.IP {
	out := make(net.IP, net.IPv6len)
	copy(out, ip.To16())
	for i := len(out) - 1; i >= 0 && delta != 0; i-- {
		sum := int(out[i]) + delta
		out[i] = byte(sum)
		delta = sum >> 8
	}
	return out
}

// lastIP6 is the last address of an IPv6 network.
func lastIP6(n *net.IPNet) net.IP {
	ip := n.IP.To16()
	out := make(net.IP, net.IPv6len)
	for i := range out {
		out[i] = ip[i] | ^n.Mask[i]
	}
	return out
}

// ipv6RangeOf returns the IPv6 subnet and gateway of a conflist ("" for
// an IPv4-only network).
func ipv6RangeOf(cl cniConflist) (subnet, gateway string) {
	for _, p := range cl.Plugins {
		if p.Type != "bridge" {
			continue
		}
		for _, ranges := range p.IPAM.Ranges {
			for _, r := range ranges {
				if ip, _, err := net.ParseCIDR(r.Subnet); err == nil && ip.To4() == nil {
					return r.Subnet, r.Gateway
				}
			}
		}
	}
	return "", ""
}

// pickIPv6Subnet returns the IPv6 subnet of a dual-stack network: the
// requested one, else the one its conflist already has, else the ULA /64
// of its IPv4 slot (octet, 0 for a user IPv4 pool) when free, else a free
// ID from 0x100 up, picked by a hash of the name.
func pickIPv6Subnet(netName string, octet int, existing map[string]cniConflist, pool *ipv6Pool) string {
	if pool != nil && pool.Subnet != "" {
		return pool.Subnet
	}
	if cur, _ := ipv6RangeOf(existing[netName]); cur != "" {
		return cur
	}
	var used []string
	for name, cl := range existing {
		if name == netName {
			continue
		}
		if s, _ := ipv6RangeOf(cl); s != "" {
			used = append(used, s)
		}
	}
	free := func(subnet string) bool {
		for _, u := range used {
			if cidrsOverlap(u, subnet) {
				return false
			}
		}
		return true
	}
	if octet > 0 && free(ulaSubnet(octet)) {
		return ulaSubnet(octet)
	}
	h := fnv.New32a()
	h.Write([]byte(netName))
	const lo, span = 0x100, 0x10000 - 0x100
	start := int(h.Sum32() % span)
	for i := 0; i < span; i++ {
		if s := ulaSubnet(lo + (start+i)%span); free(s) {
			return s
		}
	}
	return ulaSubnet(lo + start) // 65280 dual-stack networks: not reached
}

// ipv6PoolOverlap returns the name of another network whose IPv6 subnet
// overlaps the requested one, if any.
func ipv6PoolOverlap(netName string, pool *ipv6Pool, existing map[string]cniConflist) string {
	if pool == nil || pool.Subnet == "" {
		return ""
	}
	for name, cl := range existing {
		if name == netName {
			continue
		}
		if s, _ := ipv6RangeOf(cl); s != "" && cidrsOverlap(s, pool.Subnet) {
			return name
		}
	}
	return ""
}

// networkSubnet6 returns the IPv6 subnet and gateway recorded for a
// network ("" when it is IPv4-only).
func networkSubnet6(netName string) (subnet, gateway string) {
	confs, err := loadCNIConflists()
	if err != nil {
		return "", ""
	}
	return ipv6RangeOf(confs[netName])
}

// The IPv6 side is persisted like the IPv4 pool, so conflist rewrites and
// the cold-boot restore keep the network dual-stack.
func networkIPv6Path(name string) string {
	return filepath.Join(anvilRunDir, "networks", sanitizeCNIName(name)+".ipv6")
}

func saveNetworkIPv6(name string, pool *ipv6Pool) error {
	path := networkIPv6Path(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(pool)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// loadNetworkIPv6 returns nil for an IPv4-only network.
func loadNetworkIPv6(name string) *ipv6Pool {
	data, err := os.ReadFile(networkIPv6Path(name))
	if err != nil {
		return nil
	}
	var pool ipv6Pool
	if json.Unmarshal(data, &pool) != nil {
		return &ipv6Pool{} // unreadable: still dual-stack, allocated
	}
	return &pool
}

// An --internal network is marked next to its pool, so the flag survives
// the cold boots that regenerate its conflist.
func networkInternalPath(name string) string {
	return filepath.Join(anvilRunDir, "networks", sanitizeCNIName(name)+".internal")
}

func networkIsInternal(name string) bool {
	_, err := os.Stat(networkInternalPath(name))
	return err == nil
}

func markNetworkInternal(name string) error {
	path := networkInternalPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o644)
}

// masqueradeRule is a network's outbound NAT rule for one subnet (without
// -A/-C/-D); the same rule serves iptables and ip6tables.
func masqueradeRule(netName, subnet string) []string {
	return []string{"-t", "nat", "POSTROUTING", "-s", subnet, "!", "-d", subnet,
		"-m", "comment", "--comment", "anvil-masq " + netName, "-j", "MASQUERADE"}
}

// xtablesFor is the tool managing a subnet's address family.
func xtablesFor(subnet string) string {
	if ip, _, err := net.ParseCIDR(subnet); err == nil && ip.To4() == nil {
		return "ip6tables"
	}
	return "iptables"
}

// anyIPv6 reports whether a network's subnets include an IPv6 one.
func anyIPv6(subnets []string) bool {
	for _, s := range subnets {
		if xtablesFor(s) == "ip6tables" {
			return true
		}
	}
	return false
}

// isolationOnTop reports whether, in `iptables -S FORWARD` output, both of
// a network's isolation rules come before any jump into another chain or
// ACCEPT.
func isolationOnTop(rules, comment string) bool {
	found := 0
	for _, line := range strings.Split(rules, "\n") {
		if !strings.HasPrefix(line, "-A FORWARD") {
			continue
		}
		if strings.Contains(line, comment) {
			found++
			if found == 2 {
				return true
			}
			continue
		}
		if !strings.Contains(line, "anvil-internal ") {
			return false
		}
	}
	return false
}

// staticConflistBytes widens a conflist's host-local ranges to the whole
// subnet: Docker lets --ip / ipv4_address take any address of the subnet,
// and keeping static ones outside ip_range is the usual compose layout.
// The network name, and with it host-local's allocation store, is
// unchanged, so dynamic and static leases still cannot collide.
func staticConflistBytes(data []byte) ([]byte, error) {
	var conf map[string]any
	if err := json.Unmarshal(data, &conf); err != nil {
		return nil, err
	}
	plugins, _ := conf["plugins"].([]any)
	for _, p := range plugins {
		plugin, _ := p.(map[string]any)
		ipam, _ := plugin["ipam"].(map[string]any)
		ranges, _ := ipam["ranges"].([]any)
		for _, set := range ranges {
			entries, _ := set.([]any)
			for _, e := range entries {
				if r, ok := e.(map[string]any); ok {
					delete(r, "rangeStart")
					delete(r, "rangeEnd")
				}
			}
		}
	}
	return json.Marshal(conf)
}
