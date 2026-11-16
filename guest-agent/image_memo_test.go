package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/errdefs"
	digest "github.com/opencontainers/go-digest"
)

// withTempAnvilStore points the persistent state root at a temp dir for the
// duration of the test.
func withTempAnvilStore(t *testing.T) {
	t.Helper()
	old := anvilStoreRoot
	anvilStoreRoot = t.TempDir()
	t.Cleanup(func() { anvilStoreRoot = old })
}

func TestImageMemoPersistsAcrossRestart(t *testing.T) {
	withTempAnvilStore(t)
	const ref = "docker.io/library/alpine:latest"
	d := digest.FromString("alpine")
	other := digest.FromString("busybox")

	m := newImageMemoStore()
	m.markUnpacked(d, "")
	m.markPresent("default", ref, "", d)

	// The file lands on the persistent store.
	if _, err := os.Stat(filepath.Join(anvilStoreRoot, imageMemoFileName)); err != nil {
		t.Fatalf("memo file not written: %v", err)
	}

	// A fresh store (the next boot) reads back what the previous process wrote.
	m2 := newImageMemoStore()
	if !m2.isUnpacked(d, "") {
		t.Error("unpacked memo lost across restart")
	}
	if !m2.isPresent("default", ref, "", d) {
		t.Error("present memo lost across restart")
	}
	// The digest is part of the answer: a moved tag must not hit.
	if m2.isPresent("default", ref, "", other) {
		t.Error("present memo matched a different digest")
	}
	// The platform is part of the answer: another platform unpacked nothing.
	if m2.isUnpacked(d, "linux/amd64") {
		t.Error("unpacked memo matched a different platform")
	}
	// Namespace is part of the answer.
	if m2.isPresent("project", ref, "", d) {
		t.Error("present memo matched a different namespace")
	}
}

func TestImageMemoForgetImage(t *testing.T) {
	withTempAnvilStore(t)
	const refA = "docker.io/library/alpine:latest"
	const refB = "docker.io/library/busybox:latest"
	a, b := digest.FromString("alpine"), digest.FromString("busybox")

	m := newImageMemoStore()
	m.markUnpacked(a, "")
	m.markUnpacked(a, "linux/amd64")
	m.markUnpacked(b, "")
	m.markPresent("default", refA, "", a)
	m.markPresent("project", refA, "", a)
	m.markPresent("default", refB, "", b)
	storeImageInfo(a, "", &imageInfo{resolved: "linux/amd64"})
	storeImageInfo(b, "", &imageInfo{})

	// rmi of alpine: every entry about its digest goes, nothing else does.
	m.forgetImage(a)
	for _, platform := range []string{"", "linux/amd64"} {
		if m.isUnpacked(a, platform) {
			t.Errorf("unpacked memo for %q survived forgetImage", platform)
		}
	}
	if m.isPresent("default", refA, "", a) || m.isPresent("project", refA, "", a) {
		t.Error("present memo survived forgetImage")
	}
	if lookupImageInfo(a, "") != nil {
		t.Error("image info survived forgetImage")
	}
	if !m.isUnpacked(b, "") || !m.isPresent("default", refB, "", b) {
		t.Error("forgetImage dropped an unrelated image")
	}
	if lookupImageInfo(b, "") == nil {
		t.Error("forgetImage dropped unrelated image info")
	}
}

func TestImageMemoForgetRef(t *testing.T) {
	withTempAnvilStore(t)
	const ref = "docker.io/library/alpine:latest"
	d := digest.FromString("alpine")

	m := newImageMemoStore()
	m.markUnpacked(d, "")
	m.markPresent("default", ref, "", d)
	m.markPresent("default", ref, "linux/amd64", d)
	m.markPresent("project", ref, "", d)

	// A pull rewrites one namespace's record: every platform there goes.
	m.forgetRef("default", ref)
	if m.isPresent("default", ref, "", d) || m.isPresent("default", ref, "linux/amd64", d) {
		t.Error("forgetRef kept a present entry in the target namespace")
	}
	if !m.isPresent("project", ref, "", d) {
		t.Error("forgetRef dropped another namespace's entry")
	}
	// The unpacked answer is about the digest, not the record: it survives.
	if !m.isUnpacked(d, "") {
		t.Error("forgetRef dropped the unpacked answer")
	}
}

func TestImageMemoCorruptFileIsColdCache(t *testing.T) {
	withTempAnvilStore(t)
	if err := os.WriteFile(filepath.Join(anvilStoreRoot, imageMemoFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newImageMemoStore()
	if m.isUnpacked(digest.FromString("alpine"), "") {
		t.Error("corrupt memo produced a hit")
	}
	// Writes repair the file.
	d := digest.FromString("alpine")
	m.markUnpacked(d, "")
	if !newImageMemoStore().isUnpacked(d, "") {
		t.Error("memo did not recover from a corrupt file")
	}
}

func TestMissingUnpackedLayers(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"not found", fmt.Errorf("prepare snapshot: %w", errdefs.ErrNotFound), true},
		{"parent snapshot", errors.New("failed to prepare snapshot: parent snapshot sha256:abc does not exist"), true},
		{"unpacked", errors.New("no unpacked rootfs for image"), true},
		{"snapshot not found", errors.New("snapshot snapshotter not found"), true},
		{"other", errors.New("invalid spec: unknown capability"), false},
	}
	for _, tc := range cases {
		if got := missingUnpackedLayers(tc.err); got != tc.want {
			t.Errorf("%s: missingUnpackedLayers = %v, want %v", tc.name, got, tc.want)
		}
	}
}
