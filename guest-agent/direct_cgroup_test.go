package main

import (
	"os"
	"path/filepath"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// TestUpdateDirectCgroup pins the docker-update -> cgroup v2 translation for a
// direct task: absolute limits, the memory+swap -> swap-only conversion and
// pids -1 -> max (the creation path skips -1, an update must write it).
func TestUpdateDirectCgroup(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"memory.max", "memory.swap.max", "memory.low",
		"cpu.max", "cpu.weight", "cpuset.cpus", "cpuset.mems", "pids.max",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	limit, swap := int64(512<<20), int64(1<<30)
	quota, period, shares := int64(150000), uint64(100000), uint64(1024)
	pids := int64(-1)
	res := &specs.LinuxResources{
		Memory: &specs.LinuxMemory{Limit: &limit, Swap: &swap},
		CPU:    &specs.LinuxCPU{Quota: &quota, Period: &period, Shares: &shares, Cpus: "0-1"},
		Pids:   &specs.LinuxPids{Limit: &pids},
	}
	if err := updateDirectCgroup(dir, res); err != nil {
		t.Fatalf("updateDirectCgroup: %v", err)
	}

	want := map[string]string{
		"memory.max":      "536870912",
		"memory.swap.max": "536870912", // 1g combined - 512m memory
		"cpu.max":         "150000 100000",
		"cpuset.cpus":     "0-1",
		"pids.max":        "max",
	}
	for f, w := range want {
		got, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if string(got) != w {
			t.Errorf("%s = %q, want %q", f, got, w)
		}
	}
	// A controller the cgroup does not carry is skipped, not an error.
	if err := os.Remove(filepath.Join(dir, "cpu.weight")); err != nil {
		t.Fatal(err)
	}
	if err := updateDirectCgroup(dir, res); err != nil {
		t.Fatalf("updateDirectCgroup with a missing controller file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cpu.weight")); !os.IsNotExist(err) {
		t.Fatalf("cpu.weight was recreated: %v", err)
	}
}
