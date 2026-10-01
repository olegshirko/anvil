package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveInRootStaysInside(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "real", "dir"), 0o755) //nolint:errcheck
	os.Symlink("/etc", filepath.Join(root, "abs"))         //nolint:errcheck
	os.Symlink("../../..", filepath.Join(root, "up"))      //nolint:errcheck
	os.Symlink("real", filepath.Join(root, "rel"))         //nolint:errcheck
	cases := map[string]string{
		"/real/dir":      "/real/dir",
		"/abs/passwd":    "/etc/passwd",
		"/up/etc":        "/etc",
		"/rel/dir/x":     "/real/dir/x",
		"/../../outside": "/outside",
		"/new/a/b":       "/new/a/b",
	}
	for in, want := range cases {
		got, err := resolveInRoot(root, in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if !strings.HasPrefix(got, root) || strings.TrimPrefix(got, root) != want {
			t.Errorf("%s -> %s, want %s inside the root", in, got, want)
		}
	}
	os.Symlink("loop", filepath.Join(root, "loop")) //nolint:errcheck
	if _, err := resolveInRoot(root, "/loop/x"); err == nil {
		t.Error("symlink loop accepted")
	}
}
