package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNetResetCommandsBringTheLinkDownThenUp(t *testing.T) {
	cmds := netResetCommands("eth0")
	if len(cmds) != 2 {
		t.Fatalf("want down and up, got %v", cmds)
	}
	if !strings.HasSuffix(cmds[0], "eth0 down") || !strings.HasSuffix(cmds[1], "eth0 up") {
		t.Fatalf("want eth0 down then up, got %v", cmds)
	}
}

func TestLeaseFileChangedAfter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lease")

	if leaseFileChangedAfter(path, time.Now().Add(-time.Hour)) {
		t.Fatal("a missing lease file must not count as changed")
	}
	if err := os.WriteFile(path, []byte("192.168.64.2 255.255.255.0 192.168.64.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	written := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, written, written); err != nil {
		t.Fatal(err)
	}
	if leaseFileChangedAfter(path, time.Now()) {
		t.Fatal("a lease written before the reset must not count as changed")
	}
	if !leaseFileChangedAfter(path, written.Add(-time.Second)) {
		t.Fatal("a lease written after the reset must count as changed")
	}
}

func TestWaitLeaseApplied(t *testing.T) {
	dir := t.TempDir()
	old := leaseFilePath
	leaseFilePath = filepath.Join(dir, "lease")
	defer func() { leaseFilePath = old }()

	if waitLeaseApplied(time.Now().Add(-time.Hour), 50*time.Millisecond) {
		t.Fatal("no lease file: wait must time out")
	}
	if err := os.WriteFile(leaseFilePath, []byte("192.168.64.2 255.255.255.0 192.168.64.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitLeaseApplied(time.Now().Add(-time.Minute), time.Second) {
		t.Fatal("a freshly written lease must be seen")
	}
}
