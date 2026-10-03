package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	types100 "github.com/containernetworking/cni/pkg/types/100"
)

// Dual-stack (IPv6) user networks: allocation, conflists, NAT rules, CNI
// results and the inspect/hosts views of the IPv6 side.

func TestULASubnetFollowsIPv4Slot(t *testing.T) {
	if got := ulaSubnet(23); got != "fd61:6e76:696c:17::/64" {
		t.Fatalf("ulaSubnet(23) = %s", got)
	}
	if got := pickIPv6Subnet("web", 23, nil, &ipv6Pool{}); got != "fd61:6e76:696c:17::/64" {
		t.Fatalf("slot 23 got %s", got)
	}
	// A requested subnet wins.
	if got := pickIPv6Subnet("web", 23, nil, &ipv6Pool{Subnet: "2001:db8:1::/64"}); got != "2001:db8:1::/64" {
		t.Fatalf("requested subnet ignored: %s", got)
	}
}

func TestPickIPv6SubnetKeepsAndProbes(t *testing.T) {
	existing := map[string]cniConflist{
		"web":  dualStackConflist(t, "web", 7, "fd61:6e76:696c:99::/64"),
		"user": dualStackConflist(t, "user", 8, "fd61:6e76:696c:5::/64"), // a requested subnet on slot 5's ID
	}
	// The existing allocation is kept even though slot 7 would give ::7.
	if got := pickIPv6Subnet("web", 7, existing, &ipv6Pool{}); got != "fd61:6e76:696c:99::/64" {
		t.Fatalf("existing allocation not kept: %s", got)
	}
	// Slot 5's ID is taken: the network moves above every slot ID.
	got := pickIPv6Subnet("api", 5, existing, &ipv6Pool{})
	if got == ulaSubnet(5) || !strings.HasPrefix(got, ulaPrefix+":") {
		t.Fatalf("taken ID reused or outside the ULA prefix: %s", got)
	}
	_, n, _ := net.ParseCIDR(got)
	if id := int(n.IP[6])<<8 | int(n.IP[7]); id < 0x100 {
		t.Fatalf("probed ID %#x collides with the slot IDs", id)
	}
	// A user IPv4 pool has no slot: deterministic hashed ID, 0x100 and up.
	a, b := pickIPv6Subnet("pooled", 0, nil, &ipv6Pool{}), pickIPv6Subnet("pooled", 0, nil, &ipv6Pool{})
	if a != b {
		t.Fatalf("hashed ID not deterministic: %s vs %s", a, b)
	}
	_, n, _ = net.ParseCIDR(a)
	if id := int(n.IP[6])<<8 | int(n.IP[7]); id < 0x100 {
		t.Fatalf("pool network got slot-range ID %#x", id)
	}
}

func TestIPv6PoolFromIPAM(t *testing.T) {
	cfgs := []dockerIPAMConfig{{Subnet: "172.28.0.0/16"}, {Subnet: "fd00:1::/64"}}
	// Docker 27/28: without EnableIPv6 an IPv6 entry is ignored, not refused.
	if p, err := ipv6PoolFromIPAM(cfgs, false); p != nil || err != nil {
		t.Fatalf("disabled: pool = %+v, err = %v", p, err)
	}
	if !hasIPv6Subnet(cfgs) || hasIPv6Subnet(cfgs[:1]) {
		t.Fatal("hasIPv6Subnet")
	}
	// Enabled without an IPv6 subnet: allocate.
	if p, err := ipv6PoolFromIPAM(cfgs[:1], true); err != nil || p == nil || *p != (ipv6Pool{}) {
		t.Fatalf("enabled, no subnet: pool = %+v, err = %v", p, err)
	}
	p, err := ipv6PoolFromIPAM(cfgs, true)
	if err != nil || *p != (ipv6Pool{Subnet: "fd00:1::/64", Gateway: "fd00:1::1"}) {
		t.Fatalf("pool = %+v, err = %v", p, err)
	}
	p, err = ipv6PoolFromIPAM([]dockerIPAMConfig{{Subnet: "fd00:2::/64", Gateway: "fd00:2::fe", IPRange: "fd00:2::/112"}}, true)
	want := ipv6Pool{Subnet: "fd00:2::/64", Gateway: "fd00:2::fe", RangeStart: "fd00:2::1", RangeEnd: "fd00:2::ffff"}
	if err != nil || *p != want {
		t.Fatalf("pool = %+v, err = %v, want %+v", p, err, want)
	}
	for _, bad := range []dockerIPAMConfig{
		{Subnet: "fd00::/127"},
		{Subnet: "fd00::/64", Gateway: "fd01::1"},
		{Subnet: "fd00::/64", Gateway: "10.0.0.1"},
		{Subnet: "fd00::/64", IPRange: "fd01::/112"},
		{Subnet: "fd00::/64", IPRange: "10.0.0.0/24"},
	} {
		if _, err := ipv6PoolFromIPAM([]dockerIPAMConfig{bad}, true); err == nil {
			t.Errorf("%+v must be rejected", bad)
		}
	}
}

// IPv4-only networks must keep the exact conflist bytes they had before
// IPv6 support: a changed file means churn on every existing network. The
// digests pin the pre-IPv6 output of these four shapes (default bridge,
// compose default, --internal, user pool with ip_range).
func TestConflistIPv4OnlyUnchanged(t *testing.T) {
	overrideNetworkDirs(t)
	if err := markNetworkInternal("isolated"); err != nil {
		t.Fatal(err)
	}
	if err := saveNetworkPool("pooled", &ipamPool{Subnet: "172.28.0.0/16", Gateway: "172.28.0.1",
		RangeStart: "172.28.5.0", RangeEnd: "172.28.5.255"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ ns, file, digest string }{
		{"default", "bridge", "9918e3c4f1eb79f4af95e764cbe098ad8a64deb3c9732e03a60498d9a8a239bf"},
		{"web_default", "web_default", "5d25bf6edf324feba712b23a4578db0f4f3af8fc2c74b4145874273fde0663fb"},
		{"isolated", "isolated", "579620c0b9c15b977c96a3a14e9133ad7cccdadbac0133faad683254e37e6836"},
		{"pooled", "pooled", "55fd270331f0ef6c3a59af72799e7ed6bc95af33798f06269a910353a2c27e1f"},
	} {
		if err := generateCNIConfigWithLabels(c.ns, map[string]string{"x": "y"}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(cniConflistPath(c.file))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != c.digest {
			t.Errorf("%s conflist changed (sha256 %s):\n%s", c.ns, got, data)
		}
	}
}

// ipamOf returns the bridge plugin's IPAM section of a written conflist.
func ipamOf(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(cniConflistPath(name))
	if err != nil {
		t.Fatal(err)
	}
	var conf struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal(data, &conf); err != nil {
		t.Fatal(err)
	}
	for _, p := range conf.Plugins {
		if p["type"] == "bridge" {
			return p["ipam"].(map[string]any)
		}
	}
	t.Fatalf("%s: no bridge plugin", name)
	return nil
}

func TestCreateDualStackNetwork(t *testing.T) {
	overrideNetworkDirs(t)
	ctx := context.Background()

	nw, err := createDockerNetwork(ctx, dockerNetworkCreateRequest{Name: "ds", EnableIPv6: true})
	if err != nil {
		t.Fatal(err)
	}
	octet := projectSubnetOctet("ds")
	want6 := ulaSubnet(octet)
	if !nw.EnableIPv6 || len(nw.IPAM.Config) != 2 || nw.IPAM.Config[1].Subnet != want6 ||
		nw.IPAM.Config[1].Gateway != strings.TrimSuffix(want6, "/64")+"1" {
		t.Fatalf("create response = %+v", nw)
	}
	if err := generateCNIConfig("ds"); err != nil { // container create path
		t.Fatal(err)
	}
	ipam := ipamOf(t, "ds")
	ranges := ipam["ranges"].([]any)
	if len(ranges) != 2 {
		t.Fatalf("ranges = %v", ranges)
	}
	r6 := ranges[1].([]any)[0].(map[string]any)
	if r6["subnet"] != want6 || r6["gateway"] != strings.TrimSuffix(want6, "/64")+"1" {
		t.Fatalf("IPv6 range = %v", r6)
	}
	if got := fmt.Sprint(ipam["routes"]); got != "[map[dst:0.0.0.0/0] map[dst:::/0]]" {
		t.Fatalf("routes = %s", got)
	}
	dn, err := inspectDockerNetwork(ctx, "ds")
	if err != nil || !dn.EnableIPv6 || len(dn.IPAM.Config) != 2 || networkPrefixLen(dn.IPAM) != 24 {
		t.Fatalf("inspect = %+v, err = %v", dn, err)
	}

	// A requested subnet, on an --internal network: no default routes.
	_, err = createDockerNetwork(ctx, dockerNetworkCreateRequest{Name: "inner", EnableIPv6: true, Internal: true,
		IPAM: dockerIPAM{Config: []dockerIPAMConfig{{Subnet: "fd12:3456::/64", Gateway: "fd12:3456::fe"}}}})
	if err != nil {
		t.Fatal(err)
	}
	ipam = ipamOf(t, "inner")
	r6 = ipam["ranges"].([]any)[1].([]any)[0].(map[string]any)
	if r6["subnet"] != "fd12:3456::/64" || r6["gateway"] != "fd12:3456::fe" || ipam["routes"] != nil {
		t.Fatalf("internal dual-stack ipam = %v", ipam)
	}

	// The same IPv6 subnet again is refused like an overlapping IPv4 pool.
	_, err = createDockerNetwork(ctx, dockerNetworkCreateRequest{Name: "clash", EnableIPv6: true,
		IPAM: dockerIPAM{Config: []dockerIPAMConfig{{Subnet: "fd12:3456::/48"}}}})
	if errorStatus(err, 0) != http.StatusForbidden {
		t.Fatalf("overlapping IPv6 pool: err = %v", err)
	}

	// The driver option enables IPv6 as well.
	nw, err = createDockerNetwork(ctx, dockerNetworkCreateRequest{Name: "opt",
		Options: map[string]string{"com.docker.network.enable_ipv6": "true"}})
	if err != nil || !nw.EnableIPv6 {
		t.Fatalf("enable_ipv6 option: %+v, %v", nw, err)
	}

	// Without EnableIPv6 an IPv6 subnet is ignored: an IPv4-only network.
	nw, err = createDockerNetwork(ctx, dockerNetworkCreateRequest{Name: "v4",
		IPAM: dockerIPAM{Config: []dockerIPAMConfig{{Subnet: "fd99::/64"}}}})
	if err != nil || nw.EnableIPv6 || len(nw.IPAM.Config) != 1 {
		t.Fatalf("IPv6 subnet without EnableIPv6: %+v, %v", nw, err)
	}
	if _, err := os.Stat(networkIPv6Path("v4")); err == nil {
		t.Fatal("an IPv4-only network must not be marked dual-stack")
	}

	// Removal forgets the IPv6 side.
	if err := removeDockerNetwork(ctx, "ds"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(networkIPv6Path("ds")); err == nil {
		t.Fatal("IPv6 marker survived network removal")
	}
}

// dualStackConflist builds the parsed form of a dual-stack conflist.
func dualStackConflist(t *testing.T, name string, octet int, subnet6 string) cniConflist {
	t.Helper()
	raw := fmt.Sprintf(`{"name":%q,"plugins":[{"type":"bridge","bridge":"br-%s","ipam":{"ranges":[
		[{"subnet":"10.10.%d.0/24","gateway":"10.10.%d.1"}],[{"subnet":%q}]]}}]}`, name, name, octet, octet, subnet6)
	var cl cniConflist
	if err := json.Unmarshal([]byte(raw), &cl); err != nil {
		t.Fatal(err)
	}
	// The IPv4 allocation is still read from the first range set.
	if a, ok := allocFromConflist(cl); !ok || a.octet != octet {
		t.Fatalf("allocFromConflist = %+v", a)
	}
	return cl
}

func TestMasqueradeRuleIPv6(t *testing.T) {
	if xtablesFor("10.10.5.0/24") != "iptables" || xtablesFor("fd61:6e76:696c:5::/64") != "ip6tables" {
		t.Fatal("xtablesFor picks the wrong family")
	}
	want := []string{"-t", "nat", "POSTROUTING", "-s", "fd61:6e76:696c:5::/64", "!", "-d", "fd61:6e76:696c:5::/64",
		"-m", "comment", "--comment", "anvil-masq web", "-j", "MASQUERADE"}
	if got := masqueradeRule("web", "fd61:6e76:696c:5::/64"); !reflect.DeepEqual(got, want) {
		t.Fatalf("rule = %v", got)
	}
	if anyIPv6([]string{"10.10.5.0/24"}) || !anyIPv6([]string{"10.10.5.0/24", "fd00::/64"}) {
		t.Fatal("anyIPv6")
	}
}

func TestCNIResultIPv6(t *testing.T) {
	two := 2
	res := &types100.Result{
		Interfaces: []*types100.Interface{
			{Name: "br-web", Mac: "br"},
			{Name: "veth1", Mac: "host"},
			{Name: "eth1", Mac: "aa:bb", Sandbox: "/var/run/netns/x"},
		},
		IPs: []*types100.IPConfig{
			{Interface: &two, Address: mustCIDR(t, "10.10.5.2/24"), Gateway: net.ParseIP("10.10.5.1")},
			{Interface: &two, Address: mustCIDR(t, "fd61:6e76:696c:5::2/64"), Gateway: net.ParseIP("fd61:6e76:696c:5::1")},
		},
	}
	got := extraResultAddresses(res, "eth1")
	want := cniAddrs{IP: "10.10.5.2", Mac: "aa:bb", ipv6Addr: ipv6Addr{
		IPv6: "fd61:6e76:696c:5::2", IPv6PrefixLen: 64, IPv6Gateway: "fd61:6e76:696c:5::1"}}
	if got != want {
		t.Fatalf("addrs = %+v, want %+v", got, want)
	}
	// IPv4-only: no IPv6 side.
	res.IPs = res.IPs[:1]
	if got := extraResultAddresses(res, "eth1"); got.IPv6 != "" || got.IP != "10.10.5.2" {
		t.Fatalf("IPv4-only addrs = %+v", got)
	}
	if got := resultAddresses(nil); got != (cniAddrs{}) {
		t.Fatalf("nil result = %+v", got)
	}
}

func mustCIDR(t *testing.T, s string) net.IPNet {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	n.IP = ip
	return *n
}

// net.json written before IPv6 still loads, and an IPv4-only record keeps
// its exact shape.
func TestNetInfoIPv6JSONCompatible(t *testing.T) {
	v4 := containerNetInfo{IP: "10.10.1.2", Mac: "aa", Network: "front",
		Extra: []netEndpoint{{Network: "back", IfName: "eth1", IP: "10.10.2.3"}}}
	data, _ := json.Marshal(v4)
	if string(data) != `{"IP":"10.10.1.2","Mac":"aa","Network":"front","Extra":[{"Network":"back","IfName":"eth1","IP":"10.10.2.3"}]}` {
		t.Fatalf("IPv4-only net.json = %s", data)
	}
	ds := v4
	ds.ipv6Addr = ipv6Addr{IPv6: "fd00::2", IPv6PrefixLen: 64, IPv6Gateway: "fd00::1"}
	ds.Extra = []netEndpoint{{Network: "back", IfName: "eth1", IP: "10.10.2.3", ipv6Addr: ipv6Addr{IPv6: "fd01::3", IPv6PrefixLen: 64}}}
	data, _ = json.Marshal(ds)
	if !strings.Contains(string(data), `"IPv6":"fd00::2","IPv6PrefixLen":64,"IPv6Gateway":"fd00::1"`) {
		t.Fatalf("dual-stack net.json = %s", data)
	}
	var back containerNetInfo
	if err := json.Unmarshal(data, &back); err != nil || !reflect.DeepEqual(back, ds) {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
}

func TestIPv6EndpointViews(t *testing.T) {
	api := &containerMeta{Namespace: "p", ID: "api", Name: "/api", Networks: []string{"front", "back"}}
	infos := map[string]containerNetInfo{
		"api": {Network: "front", IP: "10.1.0.2",
			ipv6Addr: ipv6Addr{IPv6: "fd00:1::2", IPv6PrefixLen: 64, IPv6Gateway: "fd00:1::1"},
			Extra:    []netEndpoint{{Network: "back", IfName: "eth1", IP: "10.2.0.2"}}},
	}
	lookup := func(_, id string) (containerNetInfo, bool) {
		ni, ok := infos[id]
		return ni, ok
	}

	ep, _ := infos["api"].endpointOn("front")
	if ep.GlobalIPv6Address != "fd00:1::2" || ep.GlobalIPv6PrefixLen != 64 || ep.IPv6Gateway != "fd00:1::1" {
		t.Fatalf("endpoint = %+v", ep)
	}
	if ep, _ := infos["api"].endpointOn("back"); ep.GlobalIPv6Address != "" {
		t.Fatalf("IPv4-only endpoint got IPv6: %+v", ep)
	}

	eps := endpointsOn("front", 24, []*containerMeta{api}, lookup)
	if e := eps[dockerID("p", "api")]; e.IPv4Address != "10.1.0.2/24" || e.IPv6Address != "fd00:1::2/64" {
		t.Fatalf("network endpoint = %+v", e)
	}
	if e := endpointsOn("back", 24, []*containerMeta{api}, lookup)[dockerID("p", "api")]; e.IPv6Address != "" {
		t.Fatalf("IPv4-only network endpoint = %+v", e)
	}

	entries := networkHostsEntries([]*containerMeta{api}, lookup)
	if got := entries["front"]; !reflect.DeepEqual(got, []string{"10.1.0.2\t/api", "fd00:1::2\t/api"}) {
		t.Fatalf("front hosts = %q", got)
	}
	if got := entries["back"]; len(got) != 1 {
		t.Fatalf("back hosts = %q", got)
	}
}
