package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// The Docker container ID must be deterministic (stable across guest-agent
// restarts and daemon resumes) and 64 hex chars — see ARCHITECTURE.md §4.3.
func TestDockerIDDeterministic(t *testing.T) {
	a := dockerID("myproj", "abc123")
	b := dockerID("myproj", "abc123")
	if a != b {
		t.Fatalf("dockerID not deterministic: %s != %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("dockerID length = %d, want 64", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("dockerID not hex: %v", err)
	}
}

func TestDockerIDDiffersByNamespace(t *testing.T) {
	// Same containerd ID in two compose projects must map to different
	// Docker IDs — this is what makes two-project isolation by namespace
	// visible to the Docker CLI.
	if dockerID("proj1", "abc") == dockerID("proj2", "abc") {
		t.Fatal("dockerID must depend on the namespace")
	}
}

func TestDockerIDMatchesSpec(t *testing.T) {
	// The spec is sha256(namespace + "/" + containerdID), hex, first 64
	// chars (sha256 hex is exactly 64, so the [:64] is a no-op guard).
	got := dockerID("ns", "id")
	want := sha256Hex("ns/id")
	if got != want {
		t.Fatalf("dockerID = %s, want %s", got, want)
	}
}

func TestNamespaceFromNetwork(t *testing.T) {
	cases := map[string]string{
		"":              "default",
		"default":       "default",
		"bridge":        "default",
		"myproj_default": "myproj",
		"custom":        "custom",
	}
	for in, want := range cases {
		if got := namespaceFromNetwork(in); got != want {
			t.Errorf("namespaceFromNetwork(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDockerStateMapping(t *testing.T) {
	cases := map[string]string{
		"running": "running",
		"stopped": "exited",
		"paused":  "paused",
		"created": "created",
		"":        "created",
		"weird":   "created",
	}
	for in, want := range cases {
		if got := dockerState(in); got != want {
			t.Errorf("dockerState(%q) = %q, want %q", in, got, want)
		}
		if got := dockerStatus(in); got != want {
			t.Errorf("dockerStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripANSI(t *testing.T) {
	in := "\x1b[1mbold\x1b[0m and \x1b[31;1mred\x1b[39;49m plain"
	want := "bold and red plain"
	if got := stripANSI(in); got != want {
		t.Fatalf("stripANSI = %q, want %q", got, want)
	}
}

func TestDefaultString(t *testing.T) {
	if got := defaultString("", "fallback"); got != "fallback" {
		t.Fatalf("defaultString(empty) = %q", got)
	}
	if got := defaultString("value", "fallback"); got != "value" {
		t.Fatalf("defaultString(value) = %q", got)
	}
}
