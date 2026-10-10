package main

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"

	containerspb "github.com/containerd/containerd/api/services/containers/v1"
	"google.golang.org/protobuf/types/known/anypb"
)

// useVolumeStore points the volume store at a scratch directory.
func useVolumeStore(t *testing.T) {
	t.Helper()
	old := anvilStoreRoot
	anvilStoreRoot = t.TempDir()
	t.Cleanup(func() { anvilStoreRoot = old })
}

func makeVolumeCopy(t *testing.T, ns, name string, labels map[string]string, file string) {
	t.Helper()
	if err := os.MkdirAll(volumeDataDir(ns, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if labels != nil {
		saveVolumeLabels(ns, name, labels) //nolint:errcheck
	}
	if file != "" {
		os.WriteFile(volumeDataDir(ns, name)+"/"+file, []byte("x"), 0o644) //nolint:errcheck
	}
}

// A named volume is the daemon's: a container in a compose project's
// namespace mounts the volume compose created in "default" instead of an
// empty twin of its own (regression: `compose down -v` kept the data).
func TestNamedVolumeIsDaemonWide(t *testing.T) {
	useVolumeStore(t)
	if got := volumeNamespace("proj", "proj_pg"); got != "default" {
		t.Fatalf("unknown volume resolves to %q, want default (created there)", got)
	}
	createDockerVolume(context.Background(), dockerVolumeCreateRequest{Name: "proj_pg", //nolint:errcheck
		Labels: map[string]string{"com.docker.compose.project": "proj"}})
	if got := volumeNamespace("proj", "proj_pg"); got != "default" {
		t.Fatalf("container in proj mounts the volume from %q, want default", got)
	}
	// An older anvil's copy in the project namespace holds the data: keep
	// mounting it.
	makeVolumeCopy(t, "legacy", "legacy_pg", nil, "PG_VERSION")
	makeVolumeCopy(t, "default", "legacy_pg", map[string]string{"com.docker.compose.project": "legacy"}, "")
	if got := volumeNamespace("legacy", "legacy_pg"); got != "legacy" {
		t.Fatalf("legacy copy not preferred: %q", got)
	}
	if got := volumeNamespace("other", "legacy_pg"); got != "legacy" {
		t.Fatalf("another namespace mounts the %q copy, want the one with data", got)
	}

	vols, err := listDockerVolumes(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, v := range vols {
		names = append(names, v.Name)
		if v.Name == "legacy_pg" && (v.Labels["com.docker.compose.project"] != "legacy" || v.Mountpoint != volumeDataDir("legacy", "legacy_pg")) {
			t.Errorf("merged legacy volume: %+v", v)
		}
	}
	if !slices.Equal(names, []string{"legacy_pg", "proj_pg"}) {
		t.Fatalf("volume ls = %v, want each name once", names)
	}
}

func TestCreateExistingVolumeAdoptsLabels(t *testing.T) {
	useVolumeStore(t)
	makeVolumeCopy(t, "proj", "proj_data", nil, "f")
	v, err := createDockerVolume(context.Background(), dockerVolumeCreateRequest{Name: "proj_data",
		Labels: map[string]string{"com.docker.compose.project": "proj"}})
	if err != nil || v.Mountpoint != volumeDataDir("proj", "proj_data") || v.Labels["com.docker.compose.project"] != "proj" {
		t.Fatalf("create of an existing volume: %+v %v", v, err)
	}
	if _, err := os.Stat(volumeDataDir("default", "proj_data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("create made a second copy")
	}
}

func TestRemoveVolumeTakesEveryCopyUnlessMounted(t *testing.T) {
	useVolumeStore(t)
	makeVolumeCopy(t, "proj", "proj_pg", nil, "PG_VERSION")
	makeVolumeCopy(t, "default", "proj_pg", map[string]string{"a": "b"}, "")
	makeVolumeCopy(t, "proj", "busy", nil, "")
	spec := `{"process":{"args":["sleep"]},"mounts":[{"type":"bind","source":"` +
		volumeDataDir("proj", "busy") + `","destination":"/d"}]}`
	startFakeContainerd(t, "proj", withContainer(&containerspb.Container{
		ID: "c1", Image: "docker.io/library/alpine:latest",
		Spec: &anypb.Any{TypeUrl: "json", Value: []byte(spec)},
	}, nil))

	if err := removeDockerVolume(context.Background(), "proj_pg"); err != nil {
		t.Fatal(err)
	}
	if nss := volumeCopies("proj_pg"); len(nss) != 0 {
		t.Fatalf("copies left in %v", nss)
	}
	err := removeDockerVolume(context.Background(), "busy")
	var ae *apiError
	if !errors.As(err, &ae) || ae.status != 409 {
		t.Fatalf("removing a mounted volume: %v, want 409", err)
	}
	if len(volumeCopies("busy")) != 1 {
		t.Fatal("mounted volume removed")
	}
}
