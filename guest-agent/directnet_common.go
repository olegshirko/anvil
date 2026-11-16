package main

// Rung 6a.1: the default bridge network's attach done in-process instead of
// by five CNI plugin processes. This file holds the parts that do not touch
// the kernel — the conflist subset check, the host-local IPAM store, the
// published-port plan and the per-container marker — so they stay testable
// on the development host (the netlink/nftables side is directnet_linux.go).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	unix "golang.org/x/sys/unix"
)

// defaultBridgeNetwork is the logical name of Docker's default network. It is
// the only network the native path supports in 6a.1: user-created networks
// keep every IPAM feature (static addresses, ranges, IPv6, --internal) and
// stay on the plugin path.
const defaultBridgeNetwork = "bridge"

// cniMode is the ANVIL_CNI setting: "plugin" forces the old path, "native"
// forces the in-process path for the supported subset, anything else is
// native-with-fallback.
func cniMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ANVIL_CNI"))) {
	case "plugin":
		return "plugin"
	case "native":
		return "native"
	default:
		return "auto"
	}
}

// --- conflist subset --------------------------------------------------------

// nativeIPRange is one IPv4 range of a conflist, host-local's shape.
type nativeIPRange struct {
	Subnet     string
	Gateway    string
	RangeStart string
	RangeEnd   string
}

// nativeSpec is what the native path needs from a network: the bridge to
// attach to and the IPv4 range to allocate from.
type nativeSpec struct {
	Network string
	Bridge  string
	IPv4    nativeIPRange
}

// errNativeUnsupported marks a conflist that needs the plugin path; the
// caller falls back silently.
var errNativeUnsupported = errors.New("network outside the native CNI subset")

func unsupportedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errNativeUnsupported, fmt.Sprintf(format, args...))
}

// parseNativeSpec accepts a conflist only when it is inside the 6a.1 subset:
// not --internal, IPv4-only, exactly one IPv4 range, and a bridge plugin that
// does not masquerade per container.
func parseNativeSpec(data []byte) (nativeSpec, error) {
	var conf struct {
		Name     string `json:"name"`
		Internal bool   `json:"anvilInternal"`
		Plugins  []struct {
			Type   string `json:"type"`
			Bridge string `json:"bridge"`
			IPMasq bool   `json:"ipMasq"`
			IPAM   struct {
				Ranges [][]struct {
					Subnet     string `json:"subnet"`
					Gateway    string `json:"gateway"`
					RangeStart string `json:"rangeStart"`
					RangeEnd   string `json:"rangeEnd"`
				} `json:"ranges"`
			} `json:"ipam"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &conf); err != nil {
		return nativeSpec{}, unsupportedf("conflist: %v", err)
	}
	if conf.Name == "" {
		return nativeSpec{}, unsupportedf("conflist without a name")
	}
	if conf.Internal {
		return nativeSpec{}, unsupportedf("--internal network")
	}
	var (
		spec   nativeSpec
		ranges []nativeIPRange
	)
	spec.Network = conf.Name
	for _, p := range conf.Plugins {
		if p.Type != "bridge" {
			continue
		}
		if p.Bridge == "" {
			return nativeSpec{}, unsupportedf("bridge plugin without a bridge name")
		}
		if p.IPMasq {
			return nativeSpec{}, unsupportedf("bridge plugin masquerading per container")
		}
		if spec.Bridge != "" {
			return nativeSpec{}, unsupportedf("more than one bridge plugin")
		}
		spec.Bridge = p.Bridge
		for _, set := range p.IPAM.Ranges {
			for _, r := range set {
				ip, _, err := net.ParseCIDR(r.Subnet)
				if err != nil {
					return nativeSpec{}, unsupportedf("subnet %q: %v", r.Subnet, err)
				}
				if ip.To4() == nil {
					return nativeSpec{}, unsupportedf("IPv6 range %s", r.Subnet)
				}
				ranges = append(ranges, nativeIPRange{
					Subnet: r.Subnet, Gateway: r.Gateway,
					RangeStart: r.RangeStart, RangeEnd: r.RangeEnd,
				})
			}
		}
	}
	if spec.Bridge == "" {
		return nativeSpec{}, unsupportedf("no bridge plugin")
	}
	if len(ranges) != 1 {
		return nativeSpec{}, unsupportedf("%d IPv4 ranges", len(ranges))
	}
	spec.IPv4 = ranges[0]
	return spec, nil
}

// --- range arithmetic -------------------------------------------------------

// ipRange is a canonicalized host-local range. Addresses are 4-byte.
type ipRange struct {
	Net     *net.IPNet
	Start   net.IP
	End     net.IP
	Gateway net.IP
}

func v4(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	out := ip.To4()
	if out == nil {
		return nil
	}
	// To4 may return a slice of the original; copy so callers cannot alias.
	cp := make(net.IP, 4)
	copy(cp, out)
	return cp
}

func nextIPv4(ip net.IP) net.IP {
	n := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	n++
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)).To4()
}

func prevIPv4(ip net.IP) net.IP {
	n := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	n--
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)).To4()
}

func lastIPv4(n *net.IPNet) net.IP {
	ip := n.IP.To4()
	out := make(net.IP, 4)
	for i := range out {
		out[i] = ip[i] | ^n.Mask[len(n.Mask)-4+i]
	}
	return out
}

// newIPRange canonicalizes a conflist range the way host-local's
// Range.Canonicalize does: gateway defaults to the first address, the range
// to everything but the broadcast address.
func newIPRange(r nativeIPRange) (ipRange, error) {
	ip, n, err := net.ParseCIDR(r.Subnet)
	if err != nil {
		return ipRange{}, err
	}
	n.IP = v4(ip)
	if n.IP == nil {
		return ipRange{}, fmt.Errorf("subnet %s is not IPv4", r.Subnet)
	}
	if ones, bits := n.Mask.Size(); ones > bits-2 {
		return ipRange{}, fmt.Errorf("subnet %s is too small", r.Subnet)
	}
	gw := v4(net.ParseIP(r.Gateway))
	if gw == nil {
		gw = nextIPv4(n.IP)
	}
	start := v4(net.ParseIP(r.RangeStart))
	if start == nil {
		start = nextIPv4(n.IP)
	}
	end := v4(net.ParseIP(r.RangeEnd))
	if end == nil {
		end = prevIPv4(lastIPv4(n))
	}
	if !n.Contains(start) || !n.Contains(end) || !n.Contains(gw) {
		return ipRange{}, fmt.Errorf("range %s-%s or gateway %s outside %s", start, end, gw, r.Subnet)
	}
	if bytesCompareIPv4(start, end) > 0 {
		return ipRange{}, fmt.Errorf("range starts after it ends: %s-%s", start, end)
	}
	return ipRange{Net: n, Start: start, End: end, Gateway: gw}, nil
}

func bytesCompareIPv4(a, b net.IP) int {
	for i := 0; i < 4; i++ {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

// contains reports whether ip is inside the range's start..end window, the
// same check host-local's Range.Contains makes (the gateway is inside it; the
// candidate order skips it).
func (r ipRange) contains(ip net.IP) bool {
	ip = v4(ip)
	if ip == nil || !r.Net.Contains(ip) {
		return false
	}
	return bytesCompareIPv4(ip, r.Start) >= 0 && bytesCompareIPv4(ip, r.End) <= 0
}

// candidates is the allocation order host-local uses: the previous address
// first when the container had one, then round-robin from the last reserved
// address + 1. The gateway is never offered.
func (r ipRange) candidates(last, preferred net.IP) []net.IP {
	// The supported network is a /24; cap the enumeration so a pathological
	// range can never turn an attach into a long walk.
	const maxRange = 1 << 16
	var all []net.IP
	for ip := r.Start; bytesCompareIPv4(ip, r.End) <= 0 && len(all) < maxRange; ip = nextIPv4(ip) {
		if ip.Equal(r.Gateway) {
			continue
		}
		all = append(all, ip)
	}
	var out []net.IP
	seen := map[string]bool{}
	add := func(ip net.IP) {
		if ip == nil || seen[ip.String()] {
			return
		}
		seen[ip.String()] = true
		out = append(out, ip)
	}
	add(v4(preferred))
	start := 0
	if l := v4(last); l != nil && r.contains(l) && len(all) > 0 {
		for i, ip := range all {
			if ip.Equal(l) {
				start = (i + 1) % len(all)
				break
			}
		}
	}
	for i := 0; i < len(all); i++ {
		add(all[(start+i)%len(all)])
	}
	return out
}

// --- host-local IPAM store --------------------------------------------------

// cniNetworksDir is the host-local plugin's state root. A variable so tests
// can point it at a temporary directory.
var cniNetworksDir = hostLocalDir

const (
	lastReservedPrefix = "last_reserved_ip."
	hostLocalRangeID   = 0
)

func hostLocalNetworkDir(network string) string {
	return filepath.Join(cniNetworksDir, network)
}

// hostLocalLock takes the same flock the host-local plugin takes, so a
// fallback ADD or DEL can never interleave with a native allocation.
type hostLocalLock struct{ f *os.File }

func lockHostLocal(network string) (*hostLocalLock, error) {
	dir := hostLocalNetworkDir(network)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return &hostLocalLock{f: f}, nil
}

func (l *hostLocalLock) unlock() {
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	_ = l.f.Close()
}

// hostLocalLookup returns the address the store already records for
// (id, ifName). The plugin writes "<id>\r\n<ifname>"; older files hold just
// the id, and both are accepted exactly as the plugin's GetByID does.
func hostLocalLookup(dir, id, ifName string) []net.IP {
	var out []net.IP
	match := strings.TrimSpace(id) + "\r\n" + ifName
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		ip := net.ParseIP(filepath.Base(path))
		if ip == nil || ip.To4() == nil {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if body := strings.TrimSpace(string(data)); body == match || body == strings.TrimSpace(id) {
			out = append(out, ip.To4())
		}
		return nil
	})
	return out
}

func hostLocalLastReserved(dir string) net.IP {
	data, err := os.ReadFile(filepath.Join(dir, lastReservedPrefix+strconv.Itoa(hostLocalRangeID)))
	if err != nil {
		return nil
	}
	return v4(net.ParseIP(strings.TrimSpace(string(data))))
}

func hostLocalSetLastReserved(dir string, ip net.IP) {
	_ = os.WriteFile(filepath.Join(dir, lastReservedPrefix+strconv.Itoa(hostLocalRangeID)), []byte(ip.String()), 0o600)
}

// hostLocalReserve creates the per-address file. false means another
// container took the address between the scan and the create.
func hostLocalReserve(dir string, ip net.IP, id, ifName string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(dir, ip.String()), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(strings.TrimSpace(id) + "\r\n" + ifName); err != nil {
		f.Close()
		os.Remove(f.Name())
		return false, err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return false, err
	}
	return true, nil
}

// hostLocalAllocate reserves an address for (id, ifName) in the network's
// store. An allocation the container already holds is reused (a retried or
// crashed attach must not leak it); otherwise the previous address is taken
// when free, then the round-robin scan starts after the last reserved one.
func hostLocalAllocate(network string, r ipRange, id, ifName string, preferred net.IP) (net.IP, error) {
	lk, err := lockHostLocal(network)
	if err != nil {
		return nil, err
	}
	defer lk.unlock()
	dir := hostLocalNetworkDir(network)

	for _, ip := range hostLocalLookup(dir, id, ifName) {
		if r.contains(ip) {
			hostLocalSetLastReserved(dir, ip)
			return ip, nil
		}
	}
	for _, ip := range r.candidates(hostLocalLastReserved(dir), preferred) {
		ok, rerr := hostLocalReserve(dir, ip, id, ifName)
		if rerr != nil {
			return nil, rerr
		}
		if ok {
			hostLocalSetLastReserved(dir, ip)
			return ip, nil
		}
	}
	return nil, fmt.Errorf("no IP addresses available in range set: %s", r.Net.String())
}

// hostLocalRelease drops every address the store records for the container,
// matching the plugin's ReleaseByID.
func hostLocalRelease(network, id, ifName string) error {
	lk, err := lockHostLocal(network)
	if err != nil {
		return err
	}
	defer lk.unlock()
	return releaseHostLocal(hostLocalNetworkDir(network), id, ifName)
}

func releaseHostLocal(dir, id, ifName string) error {
	match := strings.TrimSpace(id) + "\r\n" + ifName
	var firstErr error
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		ip := net.ParseIP(filepath.Base(path))
		if ip == nil || ip.To4() == nil {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		body := strings.TrimSpace(string(data))
		if body == match || body == strings.TrimSpace(id) {
			if rerr := os.Remove(path); rerr != nil && firstErr == nil {
				firstErr = rerr
			}
		}
		return nil
	})
	return firstErr
}

// --- published ports --------------------------------------------------------

// nativeDNAT is one published port the guest translates itself. The host-side
// publisher dials the container directly (TCP) or relays to the guest IP
// (UDP); this rule covers traffic that reaches the guest's own address.
type nativeDNAT struct {
	HostIP        string
	Protocol      string
	HostPort      int
	ContainerPort int
}

func nativeDNATs(ports []cniPortMapping) []nativeDNAT {
	var out []nativeDNAT
	for _, p := range ports {
		if p.HostPort <= 0 || p.ContainerPort <= 0 {
			continue // ephemeral binding not assigned yet
		}
		proto := strings.ToLower(strings.TrimSpace(p.Protocol))
		if proto == "" {
			proto = "tcp"
		}
		switch proto {
		case "tcp", "udp", "sctp":
		default:
			continue
		}
		out = append(out, nativeDNAT{HostIP: p.HostIP, Protocol: proto, HostPort: p.HostPort, ContainerPort: p.ContainerPort})
	}
	return out
}

// --- per-container marker ---------------------------------------------------

// nativeEndpoint records that the container's endpoint was created by the
// native path. net.json stays untouched: teardown reads this marker to pick
// the matching DEL, and a plugin attach clears it.
type nativeEndpoint struct {
	Network string `json:"network"`
	IP      string `json:"ip"`
}

func nativeMarkerPath(ns, id string) string {
	return filepath.Join(containerMetaDir(ns, id), "net.native")
}

func writeNativeMarker(ns, id string, ep nativeEndpoint) error {
	data, err := json.Marshal(ep)
	if err != nil {
		return err
	}
	path := nativeMarkerPath(ns, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func loadNativeMarker(ns, id string) (nativeEndpoint, bool) {
	data, err := os.ReadFile(nativeMarkerPath(ns, id))
	if err != nil {
		return nativeEndpoint{}, false
	}
	var ep nativeEndpoint
	if json.Unmarshal(data, &ep) != nil || ep.Network == "" {
		return nativeEndpoint{}, false
	}
	return ep, true
}

func removeNativeMarker(ns, id string) {
	_ = os.Remove(nativeMarkerPath(ns, id))
}
