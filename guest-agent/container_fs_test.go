package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
)

func TestOverlayLayers(t *testing.T) {
	upper, lowers, err := overlayLayers([]mount.Mount{{Type: "overlay", Source: "overlay",
		Options: []string{"index=off", "workdir=/s/9/work", "upperdir=/s/9/fs", "lowerdir=/s/2/fs:/s/1/fs"}}})
	if err != nil || upper != "/s/9/fs" || !reflect.DeepEqual(lowers, []string{"/s/2/fs", "/s/1/fs"}) {
		t.Errorf("overlay: %q %v %v", upper, lowers, err)
	}
	upper, lowers, err = overlayLayers([]mount.Mount{{Type: "bind", Source: "/s/1/fs", Options: []string{"rbind"}}})
	if err != nil || upper != "/s/1/fs" || lowers != nil {
		t.Errorf("bind: %q %v %v", upper, lowers, err)
	}
	if _, _, err = overlayLayers([]mount.Mount{{Type: "overlay", Options: []string{"lowerdir=/a:/b"}}}); err == nil {
		t.Error("view snapshot (no upperdir) accepted")
	}
}

func TestOverlayChanges(t *testing.T) {
	root := t.TempDir()
	upper, lower := filepath.Join(root, "upper"), filepath.Join(root, "lower")
	mk := func(base string, files ...string) {
		for _, f := range files {
			p := filepath.Join(base, f)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk(lower, "etc/passwd", "etc/os-release", "usr/bin/sh")
	// /etc/passwd edited, /tmp/new added, /etc/hosts is a mountpoint runc
	// created, /run holds only a mountpoint, /var/cache is a whole new tree.
	mk(upper, "etc/passwd", "tmp/new", "etc/hosts", "run/secret", "var/cache/a/b")
	if err := os.MkdirAll(filepath.Join(lower, "run"), 0o755); err != nil {
		t.Fatal(err)
	}

	inLower, done, lerr := lowerLookup([]string{lower})
	if lerr != nil {
		t.Fatal(lerr)
	}
	defer done()
	got, err := overlayChanges(upper, inLower, map[string]bool{"/etc/hosts": true, "/run/secret": true})
	if err != nil {
		t.Fatal(err)
	}
	want := []containerChange{
		{"/etc", changeModified},
		{"/etc/passwd", changeModified},
		{"/tmp", changeAdded},
		{"/tmp/new", changeAdded},
		{"/var", changeAdded},
		{"/var/cache", changeAdded},
		{"/var/cache/a", changeAdded},
		{"/var/cache/a/b", changeAdded},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changes:\n got %v\nwant %v", got, want)
	}

	// Nothing changed: an empty list, not null.
	empty := filepath.Join(root, "empty")
	os.MkdirAll(empty, 0o755)
	if got, _ := overlayChanges(empty, inLower, nil); got == nil || len(got) != 0 {
		t.Errorf("empty upper: %v", got)
	}
}
