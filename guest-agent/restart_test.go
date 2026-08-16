package main

import (
	"testing"
	"time"
)

func TestParseRestartPolicy(t *testing.T) {
	cases := []struct {
		spec string
		name string
		max  int
	}{
		{"", "", 0},
		{"no", "no", -1},
		{"always", "always", -1},
		{"unless-stopped", "unless-stopped", -1},
		{"on-failure", "on-failure", -1},
		{"on-failure:3", "on-failure", 3},
		{"on-failure:0", "on-failure", 0},
		{"on-failure:10", "on-failure", 10},
		{"on-failure:notanumber", "on-failure", -1},
		{"on-failure:-2", "on-failure", -2},
		// A malformed trailing segment ("3:extra") fails strconv.Atoi, so
		// the whole count is dropped: unlimited. Docker-side the CLI never
		// sends such specs; this pins our behavior.
		{"on-failure:3:extra", "on-failure", -1},
	}
	for _, tc := range cases {
		p := parseRestartPolicy(tc.spec)
		if p.name != tc.name || p.max != tc.max {
			t.Errorf("parseRestartPolicy(%q) = {%s %d}, want {%s %d}",
				tc.spec, p.name, p.max, tc.name, tc.max)
		}
	}
}

// The create hook merges the CLI's separate MaximumRetryCount into the
// parsed spec ("on-failure" + retry 5 -> max 5); a spec-embedded count
// must win over a stray MaximumRetryCount (the CLI sends 0 when the user
// did not set one).
func TestRestartPolicyMergeWithRetryCount(t *testing.T) {
	// Spec carries the count: max from spec wins.
	p := parseRestartPolicy("on-failure:3")
	if p.max < 0 {
		t.Fatalf("spec count not parsed: %+v", p)
	}
	merged := p.max
	if merged != 3 {
		t.Errorf("spec count merge: got %d want 3", merged)
	}

	// Spec without a count and MaximumRetryCount=0 (the common CLI form
	// for plain --restart on-failure): unlimited retries.
	p = parseRestartPolicy("on-failure")
	if p.max != -1 {
		t.Errorf("plain on-failure should be unlimited, got max=%d", p.max)
	}

	// Name-only policies ignore the count (docker rejects
	// maximum-retry-count for always/unless-stopped; we must not arm a
	// retry cap for them either).
	p = parseRestartPolicy("always")
	if p.max != -1 {
		t.Errorf("always should have no retry cap, got max=%d", p.max)
	}
}

func TestRestartMonitorRegistry(t *testing.T) {
	m := &restartMonitor{
		policies: make(map[string]restartPolicy),
		retries:  make(map[string]int),
		backoff:  make(map[string]time.Duration),
		nextAt:   make(map[string]time.Time),
		specs:    make(map[string]restartPolicy),
		counts:   make(map[string]int),
		stopped:  make(map[string]bool),
	}

	// register arms the policy and keeps the spec for inspect.
	m.register("id1", "on-failure", 3)
	if _, ok := m.policies["id1"]; !ok {
		t.Fatal("active policy not armed")
	}
	if got := m.policySpecFor("id1"); got.Name != "on-failure" || got.MaximumRetryCount != 3 {
		t.Errorf("spec for inspect: %+v", got)
	}

	// "no" is recorded for inspect but never armed.
	m.register("id2", "no", 0)
	if _, ok := m.policies["id2"]; ok {
		t.Error("policy 'no' must not be armed")
	}
	if got := m.policySpecFor("id2"); got.Name != "no" {
		t.Errorf("spec for 'no': %+v", got)
	}
	// Unknown containers report the docker default "no".
	if got := m.policySpecFor("missing"); got.Name != "no" {
		t.Errorf("missing spec: %+v", got)
	}

	// clear disarms but keeps the spec (inspect fidelity after a user stop).
	m.clear("id1")
	if _, ok := m.policies["id1"]; ok {
		t.Error("clear must disarm the policy")
	}
	if got := m.policySpecFor("id1"); got.Name != "on-failure" {
		t.Errorf("clear must keep the requested spec for inspect, got %+v", got)
	}

	// countFor tracks performed restarts.
	if got := m.countFor("id1"); got != 0 {
		t.Errorf("initial count: %d", got)
	}
}
