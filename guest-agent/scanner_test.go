package main

import (
	"fmt"
	"strings"
	"testing"
)

// Per-project CNI subnets must be deterministic so a recreated compose
// project gets the same network — and stay inside 10.10.1.0/24..10.10.250.0/24.
func TestProjectSubnetOctetDeterministic(t *testing.T) {
	if got := projectSubnetOctet("web"); got != 76 { // pinned FNV-1a reference value
		t.Errorf("projectSubnetOctet(web) = %d, want 76", got)
	}
	if projectSubnetOctet("web") != projectSubnetOctet("web") {
		t.Error("projectSubnetOctet not deterministic")
	}
}

func TestProjectSubnetOctetRange(t *testing.T) {
	for i := 0; i < 1000; i++ {
		name := fmt.Sprintf("proj-%d", i)
		octet := projectSubnetOctet(name)
		if octet < 1 || octet > 250 {
			t.Fatalf("projectSubnetOctet(%q) = %d, out of [1,250]", name, octet)
		}
	}
	// Distinct projects should not collide on a small sample (not a hard
	// guarantee of the hash, but a sanity check against wiring bugs).
	seen := map[int]string{}
	for _, name := range []string{"alpha", "beta", "gamma", "delta"} {
		octet := projectSubnetOctet(name)
		if prev, dup := seen[octet]; dup {
			t.Errorf("projects %q and %q collide on subnet %d", prev, name, octet)
		}
		seen[octet] = name
	}
}

func TestNetworkIDDeterministic(t *testing.T) {
	if got := networkID("web"); got != "48d347bac606187dcfd0e4f13de7663e" { // pinned FNV-1a 128 reference value
		t.Errorf("networkID(web) = %s", got)
	}
	if networkID("web") != networkID("web") {
		t.Error("networkID not deterministic")
	}
	if len(networkID("x")) != 32 {
		t.Errorf("networkID length = %d, want 32 hex chars", len(networkID("x")))
	}
}

func TestSanitizeCNIName(t *testing.T) {
	cases := map[string]string{
		"simple":          "simple",
		"with-dash":       "with-dash",
		"under_score":     "under_score",
		"UPPER":           "upper",
		"dots.and:colons": "dots-and-colons",
		"":                "default",
	}
	for in, want := range cases {
		if got := sanitizeCNIName(in); got != want {
			t.Errorf("sanitizeCNIName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeCNIName("long-name-" + strings.Repeat("x", 80)); len(got) != 64 {
		t.Errorf("sanitizeCNIName length = %d, want capped at 64", len(got))
	}
}
