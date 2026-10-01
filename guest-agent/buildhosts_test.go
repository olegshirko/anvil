package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildExtraHosts(t *testing.T) {
	if got := buildExtraHosts("db:10.0.0.5, cache:10.0.0.6"); got != "db=10.0.0.5,cache=10.0.0.6" {
		t.Errorf("got %q", got)
	}
	if got := buildExtraHosts(""); got != "" {
		t.Errorf("empty: %q", got)
	}
}

func TestVolumeSubpath(t *testing.T) {
	vol := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vol, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := volumeSubpath(vol, "a/b"); err != nil || !strings.HasSuffix(got, "/a/b") {
		t.Fatalf("a/b -> %q %v", got, err)
	}
	if _, err := volumeSubpath(vol, "missing"); err == nil {
		t.Fatal("missing subpath accepted")
	}
	if err := os.Symlink("/etc", filepath.Join(vol, "out")); err != nil {
		t.Fatal(err)
	}
	if _, err := volumeSubpath(vol, "out"); err == nil {
		t.Fatal("symlink escaping the volume accepted")
	}
}
