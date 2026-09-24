package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"path/filepath"
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

// allocFromConflist extracts the subnet and bridge a conflist uses.
func allocFromConflist(cl cniConflist) (netAlloc, bool) {
	for _, p := range cl.Plugins {
		if p.Type != "bridge" {
			continue
		}
		for _, ranges := range p.IPAM.Ranges {
			for _, r := range ranges {
				if _, _, err := net.ParseCIDR(r.Subnet); err != nil {
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
// ignored: anvil networks are IPv4-only.
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
}
