package main

import (
	cniclient "github.com/containerd/go-cni"
	types100 "github.com/containernetworking/cni/pkg/types/100"
)

// CNI result parsing, kept free of linux-only code so it is unit-testable
// on the development host. The attaches themselves live in runtime_linux.go.

// resultAddresses extracts the primary endpoint's addresses from a go-cni
// result: the IPv4 address and MAC (the address arrives as a plain net.IP)
// and, on a dual-stack network, the IPv6 side from the raw results, the
// only place that keeps the prefix length.
func resultAddresses(res *cniclient.Result) cniAddrs {
	var a cniAddrs
	if res == nil {
		return a
	}
	a.IP, a.Mac = primaryIPv4(res)
	for _, raw := range res.Raw() {
		if raw == nil {
			continue
		}
		// go-cni names the single network's interface eth0.
		if v6 := ipv6FromResult(raw, "eth0"); v6.IPv6 != "" {
			a.ipv6Addr = v6
			break
		}
	}
	return a
}

// primaryIPv4 picks the IPv4 address and a MAC from a go-cni result.
func primaryIPv4(res *cniclient.Result) (ip, mac string) {
	for _, iface := range res.Interfaces {
		if iface == nil {
			continue
		}
		if mac == "" {
			mac = iface.Mac
		}
		for _, cfg := range iface.IPConfigs {
			if cfg.IP != nil && cfg.IP.To4() != nil {
				return cfg.IP.String(), mac
			}
			if ip == "" && cfg.IP != nil {
				ip = cfg.IP.String()
			}
		}
	}
	return ip, mac
}

// sandboxIndex is the index of ifName inside the container's netns in a
// result's interface list (-1 when it is not listed).
func sandboxIndex(res *types100.Result, ifName string) int {
	idx := -1
	for i, iface := range res.Interfaces {
		if iface != nil && iface.Name == ifName && iface.Sandbox != "" {
			idx = i
		}
	}
	return idx
}

// extraResultAddresses picks the addresses and MAC of ifName: the IPv4
// address (any address when there is none) and the IPv6 side.
func extraResultAddresses(res *types100.Result, ifName string) cniAddrs {
	var a cniAddrs
	idx := sandboxIndex(res, ifName)
	if idx >= 0 {
		a.Mac = res.Interfaces[idx].Mac
	}
	a.ipv6Addr = ipv6FromResult(res, ifName)
	for _, cfg := range res.IPs {
		if cfg == nil || (cfg.Interface != nil && idx >= 0 && *cfg.Interface != idx) {
			continue
		}
		if v4 := cfg.Address.IP.To4(); v4 != nil {
			a.IP = v4.String()
			return a
		}
		if a.IP == "" {
			a.IP = cfg.Address.IP.String()
		}
	}
	return a
}

// ipv6FromResult returns ifName's IPv6 address, prefix length and gateway
// (empty on an IPv4-only network).
func ipv6FromResult(res *types100.Result, ifName string) ipv6Addr {
	idx := sandboxIndex(res, ifName)
	for _, cfg := range res.IPs {
		if cfg == nil || (cfg.Interface != nil && idx >= 0 && *cfg.Interface != idx) {
			continue
		}
		ip := cfg.Address.IP
		if ip == nil || ip.To4() != nil {
			continue
		}
		ones, _ := cfg.Address.Mask.Size()
		a := ipv6Addr{IPv6: ip.String(), IPv6PrefixLen: ones}
		if cfg.Gateway != nil {
			a.IPv6Gateway = cfg.Gateway.String()
		}
		return a
	}
	return ipv6Addr{}
}
