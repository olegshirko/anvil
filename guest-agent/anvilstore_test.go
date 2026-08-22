package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContainerMetaRoundtrip(t *testing.T) {
	dir := t.TempDir()
	oldRoot := anvilStoreRoot
	anvilStoreRoot = dir
	defer func() { anvilStoreRoot = oldRoot }()

	m := &containerMeta{
		ID:        "abc123",
		Name:      "web",
		Namespace: "proj",
		ImageRef:  "docker.io/library/nginx:latest",
		Ports: []cniPortMapping{
			{HostPort: 8080, ContainerPort: 80, Protocol: "tcp", HostIP: "0.0.0.0"},
		},
		Aliases:    []string{"web"},
		TTY:        true,
		AutoRemove: true,
		StopSignal: "SIGTERM",
		Mounts:     []dockerMount{{Type: "bind", Source: "/Users/x", Target: "/x", ReadOnly: true}},
	}
	if err := saveContainerMeta(m); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := loadContainerMeta("proj", "abc123")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Name != "web" || got.Namespace != "proj" || got.ID != "abc123" {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if len(got.Ports) != 1 || got.Ports[0].HostPort != 8080 || got.Ports[0].Protocol != "tcp" {
		t.Fatalf("ports mismatch: %+v", got.Ports)
	}
	if !got.TTY || !got.AutoRemove || got.StopSignal != "SIGTERM" {
		t.Fatalf("flags mismatch: %+v", got)
	}

	metas, err := containerMetas()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(metas) != 1 || metas[0].Name != "web" {
		t.Fatalf("list mismatch: %+v", metas)
	}

	deleteContainerMeta("proj", "abc123")
	if _, err := loadContainerMeta("proj", "abc123"); err == nil {
		t.Fatal("expected load error after delete")
	}
	if metas, _ := containerMetas(); len(metas) != 0 {
		t.Fatalf("expected empty list after delete, got %d", len(metas))
	}
}

func TestAnvilPaths(t *testing.T) {
	dir := t.TempDir()
	oldRoot := anvilStoreRoot
	anvilStoreRoot = dir
	defer func() { anvilStoreRoot = oldRoot }()

	want := filepath.Join(dir, "containers", "ns1", "cid")
	if got := containerMetaDir("ns1", "cid"); got != want {
		t.Fatalf("meta dir: got %s want %s", got, want)
	}
	if got := containerLogPath("ns1", "cid"); got != filepath.Join(want, "cid.json") {
		t.Fatalf("log path: %s", got)
	}
	if got := containerHostsPath("ns1", "cid"); got != filepath.Join(want, "hosts") {
		t.Fatalf("hosts path: %s", got)
	}
	if got := containerResolvPath("ns1", "cid"); got != filepath.Join(want, "resolv.conf") {
		t.Fatalf("resolv path: %s", got)
	}
	if got := volumeDataDir("default", "data"); got != filepath.Join(dir, "volumes", "default", "data") {
		t.Fatalf("volume path: %s", got)
	}
	if err := os.MkdirAll(volumeDataDir("default", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
}
