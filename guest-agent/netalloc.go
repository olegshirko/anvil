package main

import (
	"fmt"
	"hash/fnv"
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
	octet  int
	bridge string
}

// allocFromConflist extracts the subnet octet and bridge a conflist uses.
func allocFromConflist(cl cniConflist) (netAlloc, bool) {
	for _, p := range cl.Plugins {
		if p.Type != "bridge" {
			continue
		}
		for _, ranges := range p.IPAM.Ranges {
			for _, r := range ranges {
				var a, b, c, d, bits int
				if n, _ := fmt.Sscanf(r.Subnet, "%d.%d.%d.%d/%d", &a, &b, &c, &d, &bits); n == 5 && a == 10 && b == 10 {
					return netAlloc{octet: c, bridge: p.Bridge}, true
				}
			}
		}
	}
	return netAlloc{}, false
}

// pickNetAlloc returns the allocation for netName given every existing
// conflist (keyed by network name). preferred is the hashed octet.
func pickNetAlloc(netName string, preferred int, existing map[string]cniConflist) netAlloc {
	if cl, ok := existing[netName]; ok {
		if a, ok := allocFromConflist(cl); ok {
			return a
		}
	}
	usedOctets := map[int]bool{}
	usedBridges := map[string]bool{}
	for name, cl := range existing {
		if name == netName {
			continue
		}
		if a, ok := allocFromConflist(cl); ok {
			usedOctets[a.octet] = true
			usedBridges[a.bridge] = true
		}
	}

	octet := preferred
	for i := 0; i < 250; i++ {
		o := (preferred-1+i)%250 + 1
		if !usedOctets[o] {
			octet = o
			break
		}
	}
	return netAlloc{octet: octet, bridge: pickBridgeName(netName, usedBridges)}
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
			return fmt.Sprintf("10.10.%d.0/24", a.octet)
		}
	}
	return fmt.Sprintf("10.10.%d.0/24", projectSubnetOctet(netName))
}
