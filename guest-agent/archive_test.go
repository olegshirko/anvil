package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveInRoot(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "run", "app"), 0o755))
	must(os.MkdirAll(filepath.Join(root, "etc"), 0o755))
	must(os.WriteFile(filepath.Join(root, "etc", "conf"), nil, 0o644))
	must(os.Symlink("/run", filepath.Join(root, "var-run")))           // absolute: stays inside root
	must(os.Symlink("../../../etc", filepath.Join(root, "run", "up"))) // climbs above root
	must(os.Symlink("conf", filepath.Join(root, "etc", "link")))       // relative final
	must(os.Symlink("/loop2", filepath.Join(root, "loop1")))
	must(os.Symlink("/loop1", filepath.Join(root, "loop2")))

	cases := []struct {
		path        string
		followFinal bool
		want        string // relative to root
	}{
		{"/var-run/app", false, "run/app"},
		{"var-run/app", false, "run/app"},
		{"/run/up/conf", false, "etc/conf"},
		{"/../../etc/conf", false, "etc/conf"},
		{"/etc/link", false, "etc/link"},          // final symlink reported as is
		{"/etc/link", true, "etc/conf"},           // or followed for a destination
		{"/var-run/new/dir", true, "run/new/dir"}, // missing parts kept lexically
		{"/", false, ""},
	}
	for _, tc := range cases {
		got, err := resolveInRoot(root, tc.path, tc.followFinal)
		if err != nil {
			t.Errorf("%q: %v", tc.path, err)
			continue
		}
		if want := filepath.Join(root, tc.want); got != want {
			t.Errorf("resolveInRoot(%q, %v) = %q, want %q", tc.path, tc.followFinal, got, want)
		}
	}
	if _, err := resolveInRoot(root, "/loop1/x", false); err == nil {
		t.Error("symlink loop not detected")
	}
}
