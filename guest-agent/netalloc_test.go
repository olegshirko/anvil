package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// conflistWith builds the parsed form of a conflist holding one allocation.
func conflistWith(t *testing.T, name string, octet int, bridge string) cniConflist {
	t.Helper()
	raw := fmt.Sprintf(`{"name":%q,"plugins":[{"type":"loopback"},{"type":"bridge","bridge":%q,
		"ipam":{"ranges":[[{"subnet":"10.10.%d.0/24","gateway":"10.10.%d.1"}]]}}]}`, name, bridge, octet, octet)
	var cl cniConflist
	if err := json.Unmarshal([]byte(raw), &cl); err != nil {
		t.Fatal(err)
	}
	return cl
}

// A network keeps whatever it was allocated, even if its hashed slot moved.
func TestPickNetAllocKeepsExisting(t *testing.T) {
	existing := map[string]cniConflist{"web": conflistWith(t, "web", 7, "br-web")}
	got := pickNetAlloc("web", 76, existing, nil)
	if got.octet != 7 || got.bridge != "br-web" {
		t.Fatalf("existing allocation not kept: %+v", got)
	}
}

// Two projects hashing to the same slot must get different subnets.
func TestPickNetAllocProbesPastTakenSubnet(t *testing.T) {
	existing := map[string]cniConflist{"a": conflistWith(t, "a", 250, "br-a")}
	got := pickNetAlloc("b", 250, existing, nil)
	if got.octet != 1 {
		t.Fatalf("octet = %d, want the next free slot 1 (wrapping)", got.octet)
	}
}

// Long names used to be truncated to the same 15-character bridge.
func TestPickBridgeNameLongNamesDiffer(t *testing.T) {
	a := pickNetAlloc("compose-test-a_default", 10, nil, nil)
	existing := map[string]cniConflist{"compose-test-a_default": conflistWith(t, "compose-test-a_default", a.octet, a.bridge)}
	b := pickNetAlloc("compose-test-b_default", 11, existing, nil)
	if a.bridge == b.bridge {
		t.Fatalf("both networks got bridge %q", a.bridge)
	}
	for _, br := range []string{a.bridge, b.bridge} {
		if len(br) > 15 {
			t.Fatalf("bridge %q exceeds IFNAMSIZ", br)
		}
	}
	if got := pickNetAlloc("web", 1, nil, nil).bridge; got != "br-web" {
		t.Fatalf("short names keep the readable form, got %q", got)
	}
}

// End to end through the config writer: a second network on the same hashed
// slot lands on another subnet, and rewriting a config keeps its allocation.
func TestGenerateCNIConfigAvoidsCollisions(t *testing.T) {
	dir := t.TempDir()
	old := cniConfDir
	cniConfDir = dir
	t.Cleanup(func() { cniConfDir = old })

	// Find two names that hash to the same octet.
	byOctet := map[int]string{}
	var first, second string
	for i := 0; second == ""; i++ {
		n := fmt.Sprintf("proj%d", i)
		o := projectSubnetOctet(n)
		if prev, ok := byOctet[o]; ok {
			first, second = prev, n
		}
		byOctet[o] = n
	}
	for _, n := range []string{first, second, first} {
		if err := generateCNIConfig(n); err != nil {
			t.Fatal(err)
		}
	}
	confs, err := loadCNIConflists()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := allocFromConflist(confs[first])
	b, _ := allocFromConflist(confs[second])
	if a.octet == b.octet {
		t.Fatalf("%s and %s share 10.10.%d.0/24", first, second, a.octet)
	}
	if a.octet != projectSubnetOctet(first) {
		t.Fatalf("first network lost its hashed slot: %d", a.octet)
	}
	if got := networkSubnet(second); got != fmt.Sprintf("10.10.%d.0/24", b.octet) {
		t.Fatalf("networkSubnet(%s) = %s", second, got)
	}
	if _, err := os.Stat(filepath.Join(dir, "anvil-"+sanitizeCNIName(second)+".conflist")); err != nil {
		t.Fatal(err)
	}
}

func TestPoolFromIPAM(t *testing.T) {
	pool, err := poolFromIPAM([]dockerIPAMConfig{
		{Subnet: "fd00::/64"}, // IPv6 is skipped, not an error
		{Subnet: "172.28.0.0/16", IPRange: "172.28.5.0/24"},
	})
	if err != nil || pool == nil {
		t.Fatalf("pool = %+v, err = %v", pool, err)
	}
	want := ipamPool{Subnet: "172.28.0.0/16", Gateway: "172.28.0.1", RangeStart: "172.28.5.0", RangeEnd: "172.28.5.255"}
	if *pool != want {
		t.Fatalf("pool = %+v, want %+v", *pool, want)
	}

	// A range equal to the subnet excludes network and broadcast addresses.
	pool, _ = poolFromIPAM([]dockerIPAMConfig{{Subnet: "192.168.90.0/24", Gateway: "192.168.90.254", IPRange: "192.168.90.0/24"}})
	if pool.Gateway != "192.168.90.254" || pool.RangeStart != "192.168.90.1" || pool.RangeEnd != "192.168.90.254" {
		t.Fatalf("pool = %+v", *pool)
	}

	if p, err := poolFromIPAM(nil); p != nil || err != nil {
		t.Fatal("no config means the hashed slot")
	}
	for _, bad := range []dockerIPAMConfig{
		{Subnet: "nonsense"},
		{Subnet: "10.0.0.0/31"},
		{Subnet: "10.0.0.0/24", Gateway: "10.0.1.1"},
		{Subnet: "10.0.0.0/24", IPRange: "10.0.1.0/28"},
	} {
		if _, err := poolFromIPAM([]dockerIPAMConfig{bad}); err == nil {
			t.Errorf("%+v must be rejected", bad)
		}
	}
}

// End to end: the requested subnet reaches the conflist, survives a rewrite
// (every container create regenerates it), blocks hashed slots it overlaps
// and makes an overlapping second pool fail like Docker.
func TestCreateNetworkWithPool(t *testing.T) {
	cniDir, runDir := t.TempDir(), t.TempDir()
	oldCNI, oldRun := cniConfDir, anvilRunDir
	cniConfDir, anvilRunDir = cniDir, runDir
	t.Cleanup(func() { cniConfDir, anvilRunDir = oldCNI, oldRun })

	req := dockerNetworkCreateRequest{Name: "proj_backend", IPAM: dockerIPAM{Config: []dockerIPAMConfig{{Subnet: "10.10.0.0/20", Gateway: "10.10.0.254"}}}}
	nw, err := createDockerNetwork(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := nw.IPAM.Config[0]; got.Subnet != "10.10.0.0/20" || got.Gateway != "10.10.0.254" {
		t.Fatalf("create response IPAM = %+v", got)
	}
	if err := generateCNIConfig("proj_backend"); err != nil { // container create path
		t.Fatal(err)
	}
	confs, _ := loadCNIConflists()
	cl := confs["proj_backend"]
	a, _ := allocFromConflist(cl)
	if a.subnet != "10.10.0.0/20" {
		t.Fatalf("conflist subnet = %s", a.subnet)
	}
	var gw string
	for _, p := range cl.Plugins {
		for _, rs := range p.IPAM.Ranges {
			gw = rs[0].Gateway
		}
	}
	if gw != "10.10.0.254" {
		t.Fatalf("conflist gateway = %s", gw)
	}

	// Hashed networks must stay out of 10.10.0.0/20 (octets 0..15).
	for i := 0; i < 40; i++ {
		n := fmt.Sprintf("hashed%d", i)
		if err := generateCNIConfig(n); err != nil {
			t.Fatal(err)
		}
	}
	confs, _ = loadCNIConflists()
	for i := 0; i < 40; i++ {
		if a, _ := allocFromConflist(confs[fmt.Sprintf("hashed%d", i)]); a.octet <= 15 {
			t.Fatalf("hashed%d got 10.10.%d.0/24 inside the user pool", i, a.octet)
		}
	}

	_, err = createDockerNetwork(context.Background(), dockerNetworkCreateRequest{
		Name: "other", IPAM: dockerIPAM{Config: []dockerIPAMConfig{{Subnet: "10.10.8.0/24"}}}})
	if errorStatus(err, 0) != http.StatusForbidden {
		t.Fatalf("overlapping pool: err = %v", err)
	}
	if _, err := os.Stat(networkPoolPath("other")); err == nil {
		t.Fatal("a refused pool must not be persisted")
	}
}
