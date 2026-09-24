package main

import (
	"encoding/json"
	"fmt"
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
	got := pickNetAlloc("web", 76, existing)
	if got.octet != 7 || got.bridge != "br-web" {
		t.Fatalf("existing allocation not kept: %+v", got)
	}
}

// Two projects hashing to the same slot must get different subnets.
func TestPickNetAllocProbesPastTakenSubnet(t *testing.T) {
	existing := map[string]cniConflist{"a": conflistWith(t, "a", 250, "br-a")}
	got := pickNetAlloc("b", 250, existing)
	if got.octet != 1 {
		t.Fatalf("octet = %d, want the next free slot 1 (wrapping)", got.octet)
	}
}

// Long names used to be truncated to the same 15-character bridge.
func TestPickBridgeNameLongNamesDiffer(t *testing.T) {
	a := pickNetAlloc("compose-test-a_default", 10, nil)
	existing := map[string]cniConflist{"compose-test-a_default": conflistWith(t, "compose-test-a_default", a.octet, a.bridge)}
	b := pickNetAlloc("compose-test-b_default", 11, existing)
	if a.bridge == b.bridge {
		t.Fatalf("both networks got bridge %q", a.bridge)
	}
	for _, br := range []string{a.bridge, b.bridge} {
		if len(br) > 15 {
			t.Fatalf("bridge %q exceeds IFNAMSIZ", br)
		}
	}
	if got := pickNetAlloc("web", 1, nil).bridge; got != "br-web" {
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
