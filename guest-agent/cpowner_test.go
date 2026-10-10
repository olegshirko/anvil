package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestResolveContainerUserNumeric(t *testing.T) {
	for in, want := range map[string][2]int{"": {0, 0}, "4242": {4242, 0}, "4242:4343": {4242, 4343}} {
		uid, gid, err := resolveContainerUser(in)
		if err != nil || uid != want[0] || gid != want[1] {
			t.Errorf("%q: %d:%d %v, want %v", in, uid, gid, err, want)
		}
	}
	if _, _, err := resolveContainerUser("no-such-user-anvil"); err == nil {
		t.Error("unknown user name accepted")
	}
}

func TestLookupContainerUser(t *testing.T) {
	files := map[string]string{
		"etc/passwd": "root:x:0:0:root:/root:/bin/sh\npostgres:x:999:999::/var/lib/postgresql:/bin/bash\n",
		"etc/group":  "root:x:0:\npostgres:x:999:\nssl-cert:x:101:postgres\nadm:x:4:syslog,postgres\nstaff:x:50:other\n",
	}
	read := func(name string) ([]byte, error) {
		if s, ok := files[name]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	for _, tc := range []struct {
		in        string
		uid, gid  uint32
		additions []uint32
	}{
		{"postgres", 999, 999, []uint32{101, 4}},
		{"999", 999, 999, []uint32{101, 4}},
		{"postgres:staff", 999, 50, []uint32{101, 4}},
		{"postgres:0", 999, 0, []uint32{101, 4}},
		{"4242", 4242, 0, nil},
		{"4242:4343", 4242, 4343, nil},
	} {
		u, err := lookupContainerUser(read, tc.in)
		if err != nil || u.UID != tc.uid || u.GID != tc.gid || !slices.Equal(u.AdditionalGids, tc.additions) {
			t.Errorf("%q: %+v %v, want %d:%d %v", tc.in, u, err, tc.uid, tc.gid, tc.additions)
		}
	}
	for _, bad := range []string{"nobody", "postgres:nogroup"} {
		if _, err := lookupContainerUser(read, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The container's passwd is read through an os.Root: a symlink pointing
// out of the container does not reach the guest's own files.
func TestLookupContainerUserStaysInRoot(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "passwd"), []byte("evil:x:7:7::/:/bin/sh\n"), 0o644)
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "etc"), 0o755)
	if err := os.Symlink(filepath.Join(outside, "passwd"), filepath.Join(dir, "etc", "passwd")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if u, err := lookupContainerUser(root.ReadFile, "evil"); err == nil {
		t.Errorf("read a passwd outside the root: %+v", u)
	}
}
