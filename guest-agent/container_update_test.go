package main

import (
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestApplyResourceUpdate(t *testing.T) {
	limit := int64(256 << 20)
	res := &specs.LinuxResources{Memory: &specs.LinuxMemory{Limit: &limit}}
	pids := int64(100)
	err := applyResourceUpdate(res, dockerUpdateRequest{
		Memory:     512 << 20,
		MemorySwap: 1 << 30,
		NanoCpus:   1_500_000_000,
		CpuShares:  512,
		CpusetCpus: "0",
		PidsLimit:  &pids,
	})
	if err != nil {
		t.Fatal(err)
	}
	if *res.Memory.Limit != 512<<20 || *res.Memory.Swap != 1<<30 {
		t.Errorf("memory = %d swap = %d", *res.Memory.Limit, *res.Memory.Swap)
	}
	if *res.CPU.Quota != 150000 || *res.CPU.Period != cpuPeriod || *res.CPU.Shares != 512 || res.CPU.Cpus != "0" {
		t.Errorf("cpu = %+v", res.CPU)
	}
	if *res.Pids.Limit != 100 {
		t.Errorf("pids = %d", *res.Pids.Limit)
	}

	// Zero fields leave the rest alone.
	if err := applyResourceUpdate(res, dockerUpdateRequest{CpuShares: 1024}); err != nil {
		t.Fatal(err)
	}
	if *res.Memory.Limit != 512<<20 || *res.CPU.Shares != 1024 {
		t.Errorf("partial update clobbered: memory %d shares %d", *res.Memory.Limit, *res.CPU.Shares)
	}

	unlimited := int64(-1)
	if err := applyResourceUpdate(res, dockerUpdateRequest{PidsLimit: &unlimited}); err != nil || *res.Pids.Limit != -1 {
		t.Errorf("pids -1: %v %d", err, *res.Pids.Limit)
	}
}

func TestApplyResourceUpdateRejects(t *testing.T) {
	limit, swap := int64(256<<20), int64(512<<20)
	cases := map[string]dockerUpdateRequest{
		"memory above existing swap": {Memory: 1 << 30},
		"swap below memory":          {Memory: 512 << 20, MemorySwap: 256 << 20},
		"tiny memory":                {Memory: 1 << 20},
		"nanocpus with quota":        {NanoCpus: 1e9, CpuQuota: 50000},
	}
	for name, u := range cases {
		res := &specs.LinuxResources{Memory: &specs.LinuxMemory{Limit: &limit, Swap: &swap}}
		if err := applyResourceUpdate(res, u); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRestartMonitorUpdate(t *testing.T) {
	m := &restartMonitor{
		policies: make(map[string]restartPolicy),
		backoff:  make(map[string]time.Duration),
		nextAt:   make(map[string]time.Time),
		specs:    make(map[string]restartPolicy),
		counts:   make(map[string]int),
		stopped:  make(map[string]bool),
		where:    make(map[string]containerRef),
	}
	did := dockerID("default", "c1")
	m.registerAt("default", "c1", "always", -1)
	m.counts[did] = 3

	// A stopped container gets the new spec but no armed policy.
	m.update("default", "c1", "on-failure", 5, false)
	if _, armed := m.policies[did]; armed {
		t.Error("policy armed on a stopped container")
	}
	if got := m.policySpecFor(did); got.Name != "on-failure" || got.MaximumRetryCount != 5 {
		t.Errorf("spec = %+v", got)
	}
	if m.countFor(did) != 3 {
		t.Error("restart count reset by update")
	}

	m.update("default", "c1", "unless-stopped", -1, true)
	if p, armed := m.policies[did]; !armed || p.name != "unless-stopped" {
		t.Errorf("running container: policy %+v armed=%v", p, armed)
	}
	m.update("default", "c1", "no", -1, true)
	if _, armed := m.policies[did]; armed {
		t.Error("--restart=no left the policy armed")
	}
}

func TestRestartMonitorRearm(t *testing.T) {
	m := &restartMonitor{
		policies: make(map[string]restartPolicy),
		backoff:  make(map[string]time.Duration),
		nextAt:   make(map[string]time.Time),
		specs:    make(map[string]restartPolicy),
		counts:   make(map[string]int),
		stopped:  make(map[string]bool),
		where:    make(map[string]containerRef),
	}
	did := dockerID("default", "c1")
	m.registerAt("default", "c1", "on-failure", 3)
	m.counts[did] = 2
	m.clear(did) // docker stop
	if _, armed := m.policies[did]; armed {
		t.Fatal("stop did not disarm")
	}
	m.rearm(did) // docker start
	if p, armed := m.policies[did]; !armed || p.name != "on-failure" || p.max != 3 {
		t.Errorf("start did not re-arm: %+v %v", p, armed)
	}
	if m.countFor(did) != 0 {
		t.Error("restart count not reset by a user start")
	}

	none := dockerID("default", "c2")
	m.registerAt("default", "c2", "no", -1)
	m.rearm(none)
	if _, armed := m.policies[none]; armed {
		t.Error("--restart=no armed by start")
	}
	m.rearm(dockerID("default", "unknown")) // no spec: no-op
}
