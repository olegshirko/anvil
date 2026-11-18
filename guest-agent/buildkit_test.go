package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitForFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "buildkitd")
	if waitForFile(missing, 250*time.Millisecond) {
		t.Fatal("waitForFile reported a missing file as present")
	}

	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitForFile(present, time.Second) {
		t.Fatal("waitForFile missed an existing file")
	}
}

// A missing buildkit asset must fail with a message naming the asset and
// the consequence, not a bare "no such file or directory" from exec.
func TestBuildkitMissingErrorIsActionable(t *testing.T) {
	msg := buildkitMissingError("/opt/containerd/bin/buildkitd").Error()
	for _, want := range []string{
		"/opt/containerd/bin/buildkitd",
		"buildkit.tar.zst",
		"Containers are unaffected",
		"docker build is unavailable",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}
