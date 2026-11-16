package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The IPv4-only conflist the generator writes for the default bridge.
const nativeDefaultConflist = `{
  "cniVersion": "0.4.0",
  "name": "bridge",
  "anvilID": "id",
  "anvilInternal": false,
  "plugins": [
    {"type": "loopback"},
    {"type": "bridge", "bridge": "br-bridge", "isGateway": true, "ipMasq": false, "hairpinMode": true,
     "ipam": {"type": "host-local", "ranges": [[{"subnet": "10.10.7.0/24", "gateway": "10.10.7.1",
       "rangeStart": "10.10.7.2", "rangeEnd": "10.10.7.254"}]],
       "routes": [{"dst": "0.0.0.0/0"}]}},
    {"type": "portmap", "capabilities": {"portMappings": true}},
    {"type": "firewall", "ingressPolicy": "same-bridge"},
    {"type": "tuning"}
  ]
}`

func TestParseNativeSpec(t *testing.T) {
	spec, err := parseNativeSpec([]byte(nativeDefaultConflist))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if spec.Network != "bridge" || spec.Bridge != "br-bridge" {
		t.Fatalf("network/bridge = %q/%q", spec.Network, spec.Bridge)
	}
	if spec.IPv4.Subnet != "10.10.7.0/24" || spec.IPv4.Gateway != "10.10.7.1" ||
		spec.IPv4.RangeStart != "10.10.7.2" || spec.IPv4.RangeEnd != "10.10.7.254" {
		t.Fatalf("range = %+v", spec.IPv4)
	}

	cases := map[string]string{
		"--internal": strings.Replace(nativeDefaultConflist, `"anvilInternal": false`, `"anvilInternal": true`, 1),
		"ipv6":       strings.Replace(nativeDefaultConflist, `"rangeEnd": "10.10.7.254"`, `"rangeEnd": "10.10.7.254"}], [{"subnet": "fd61:6e76:696c:7::/64"`, 1),
		"ipMasq":     strings.Replace(nativeDefaultConflist, `"ipMasq": false`, `"ipMasq": true`, 1),
		"two ranges": strings.Replace(nativeDefaultConflist, `"rangeEnd": "10.10.7.254"`, `"rangeEnd": "10.10.7.254"}], [{"subnet": "10.10.8.0/24"`, 1),
		"no bridge":  strings.Replace(nativeDefaultConflist, `"type": "bridge", "bridge": "br-bridge"`, `"type": "bridge"`, 1),
		"not a json": `{`,
		"no name":    strings.Replace(nativeDefaultConflist, `"name": "bridge",`, ``, 1),
	}
	for name, conf := range cases {
		if _, err := parseNativeSpec([]byte(conf)); !errors.Is(err, errNativeUnsupported) {
			t.Errorf("%s: err = %v, want errNativeUnsupported", name, err)
		}
	}
}

func TestNewIPRangeDefaults(t *testing.T) {
	r, err := newIPRange(nativeIPRange{Subnet: "10.10.7.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Gateway.String(); got != "10.10.7.1" {
		t.Fatalf("gateway = %s", got)
	}
	if got := r.Start.String(); got != "10.10.7.1" {
		t.Fatalf("start = %s", got)
	}
	if got := r.End.String(); got != "10.10.7.254" {
		t.Fatalf("end = %s", got)
	}
	if got := r.candidates(nil, nil); len(got) == 0 || got[0].String() != "10.10.7.2" {
		t.Fatalf("the gateway must not be offered first: %v", got)
	}
	if !r.contains(net.ParseIP("10.10.7.254")) || r.contains(net.ParseIP("10.10.7.255")) {
		t.Fatal("range end mishandled")
	}

	if _, err := newIPRange(nativeIPRange{Subnet: "10.10.7.0/31"}); err == nil {
		t.Fatal("a /31 was accepted")
	}
	if _, err := newIPRange(nativeIPRange{Subnet: "10.10.7.0/24", Gateway: "10.10.8.1"}); err == nil {
		t.Fatal("a gateway outside the subnet was accepted")
	}
}

func TestCandidatesRoundRobin(t *testing.T) {
	r, err := newIPRange(nativeIPRange{Subnet: "10.10.7.0/24", Gateway: "10.10.7.1",
		RangeStart: "10.10.7.2", RangeEnd: "10.10.7.5"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.candidates(nil, nil); !wantIPs(got, "10.10.7.2", "10.10.7.3", "10.10.7.4", "10.10.7.5") {
		t.Fatalf("from start: %v", got)
	}
	// The plugin continues after the last reserved address.
	if got := r.candidates(net.ParseIP("10.10.7.3"), nil); !wantIPs(got, "10.10.7.4", "10.10.7.5", "10.10.7.2", "10.10.7.3") {
		t.Fatalf("from last reserved: %v", got)
	}
	if got := r.candidates(net.ParseIP("10.10.7.5"), nil); !wantIPs(got, "10.10.7.2", "10.10.7.3", "10.10.7.4", "10.10.7.5") {
		t.Fatalf("wrap: %v", got)
	}
	// The previous address of a restarted container comes first.
	if got := r.candidates(net.ParseIP("10.10.7.5"), net.ParseIP("10.10.7.3")); got[0].String() != "10.10.7.3" {
		t.Fatalf("preferred first: %v", got)
	}
}

func wantIPs(got []net.IP, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].String() != want[i] {
			return false
		}
	}
	return true
}

func TestHostLocalAllocateAndRelease(t *testing.T) {
	old := cniNetworksDir
	cniNetworksDir = t.TempDir()
	defer func() { cniNetworksDir = old }()

	r, err := newIPRange(nativeIPRange{Subnet: "10.10.7.0/24", Gateway: "10.10.7.1",
		RangeStart: "10.10.7.2", RangeEnd: "10.10.7.4"})
	if err != nil {
		t.Fatal(err)
	}

	first, err := hostLocalAllocate("bridge", r, "id-one", "eth0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.String() != "10.10.7.2" {
		t.Fatalf("first allocation = %s", first)
	}
	// The same on-disk shape host-local writes: "<id>\r\n<ifname>".
	data, err := os.ReadFile(filepath.Join(cniNetworksDir, "bridge", "10.10.7.2"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "id-one\r\neth0" {
		t.Fatalf("lease content = %q", data)
	}
	if got := hostLocalLastReserved(filepath.Join(cniNetworksDir, "bridge")); got.String() != "10.10.7.2" {
		t.Fatalf("last_reserved_ip.0 = %v", got)
	}

	second, err := hostLocalAllocate("bridge", r, "id-two", "eth0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.String() != "10.10.7.3" {
		t.Fatalf("second allocation = %s", second)
	}
	// A retried attach reuses the container's own lease instead of failing
	// the way the plugin's duplicate-allocation check would.
	again, err := hostLocalAllocate("bridge", r, "id-one", "eth0", nil)
	if err != nil || !again.Equal(first) {
		t.Fatalf("reuse = %v, %v", again, err)
	}
	// An existing lease found by id alone (the plugin's legacy shape).
	legacy := filepath.Join(cniNetworksDir, "bridge", "10.10.7.4")
	if err := os.WriteFile(legacy, []byte("id-three\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := hostLocalAllocate("bridge", r, "id-three", "eth0", nil)
	if err != nil || third.String() != "10.10.7.4" {
		t.Fatalf("legacy lease = %v, %v", third, err)
	}

	if err := hostLocalRelease("bridge", "id-one", "eth0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cniNetworksDir, "bridge", "10.10.7.2")); !os.IsNotExist(err) {
		t.Fatalf("lease survived release: %v", err)
	}
	// The store is full now: the freed address is the only candidate.
	next, err := hostLocalAllocate("bridge", r, "id-four", "eth0", net.ParseIP("10.10.7.2"))
	if err != nil || next.String() != "10.10.7.2" {
		t.Fatalf("preferred reuse = %v, %v", next, err)
	}
	if _, err := hostLocalAllocate("bridge", r, "id-five", "eth0", nil); err == nil {
		t.Fatal("allocation succeeded in a full range")
	}
}

func TestNativeDNATs(t *testing.T) {
	got := nativeDNATs([]cniPortMapping{
		{HostPort: 18080, ContainerPort: 80, Protocol: "tcp"},
		{HostPort: 25354, ContainerPort: 15354, Protocol: "UDP", HostIP: "127.0.0.1"},
		{HostPort: 0, ContainerPort: 443, Protocol: "tcp", Ephemeral: true},
		{ContainerPort: 80, Ephemeral: true},
		{HostPort: 9000, ContainerPort: 9000, Protocol: "icmp"},
	})
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Protocol != "tcp" || got[0].HostPort != 18080 || got[0].ContainerPort != 80 {
		t.Fatalf("tcp rule = %+v", got[0])
	}
	if got[1].Protocol != "udp" || got[1].HostIP != "127.0.0.1" || got[1].ContainerPort != 15354 {
		t.Fatalf("udp rule = %+v", got[1])
	}
	if nativeDNATs([]cniPortMapping{{HostPort: 8080, ContainerPort: 80}})[0].Protocol != "tcp" {
		t.Fatal("an empty protocol must default to tcp")
	}
}

func TestNativeMarkerRoundTrip(t *testing.T) {
	old := anvilStoreRoot
	anvilStoreRoot = t.TempDir()
	defer func() { anvilStoreRoot = old }()

	if _, ok := loadNativeMarker("default", "abc"); ok {
		t.Fatal("marker exists before it was written")
	}
	if err := writeNativeMarker("default", "abc", nativeEndpoint{Network: "bridge", IP: "10.10.7.2"}); err != nil {
		t.Fatal(err)
	}
	ep, ok := loadNativeMarker("default", "abc")
	if !ok || ep.Network != "bridge" || ep.IP != "10.10.7.2" {
		t.Fatalf("marker = %+v, %v", ep, ok)
	}
	removeNativeMarker("default", "abc")
	if _, ok := loadNativeMarker("default", "abc"); ok {
		t.Fatal("marker survived removal")
	}
}

func TestCNIMode(t *testing.T) {
	for env, want := range map[string]string{"": "auto", "plugin": "plugin", "native": "native", " NATIVE ": "native", "other": "auto"} {
		t.Setenv("ANVIL_CNI", env)
		if got := cniMode(); got != want {
			t.Errorf("ANVIL_CNI=%q: %q, want %q", env, got, want)
		}
	}
}
