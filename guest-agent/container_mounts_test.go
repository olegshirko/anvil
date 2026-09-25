package main

import (
	"path/filepath"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestMountPointsFor(t *testing.T) {
	vol := filepath.Join(anvilStoreRoot, "volumes", "proj", "pgdata")
	got := mountPointsFor([]specs.Mount{
		{Type: "bind", Source: vol, Destination: "/var/lib/postgresql/data", Options: []string{"rbind"}},
		{Type: "bind", Source: "/Users/me/src", Destination: "/src", Options: []string{"rbind", "ro"}},
		{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
		{Type: "bind", Source: "/x/hosts", Destination: "/etc/hosts", Options: []string{"rbind", "ro"}},
		{Type: "bind", Source: "/sock", Destination: rosettaCacheSocket},
	})
	if len(got) != 3 {
		t.Fatalf("mounts = %+v", got)
	}
	if v := got[0]; v.Type != "volume" || v.Name != "pgdata" || !v.RW || v.Driver != "local" {
		t.Errorf("volume = %+v", v)
	}
	if b := got[1]; b.Type != "bind" || b.Source != "/Users/me/src" || b.RW {
		t.Errorf("bind = %+v", b)
	}
	if tm := got[2]; tm.Type != "tmpfs" || tm.Destination != "/tmp" {
		t.Errorf("tmpfs = %+v", tm)
	}
	if _, _, ok := volumeRefFromDir("/var/lib/other/x"); ok {
		t.Error("non-volume dir classified as a volume")
	}
}
