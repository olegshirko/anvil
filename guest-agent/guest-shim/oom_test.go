//go:build shimserver

package main

import "testing"

// TestParseMemoryEventsOOMKill covers the cgroup v2 memory.events parsing
// the fast shim's OOM watcher is built on (kernel format: "<counter> <n>"
// per line, oom_kill among them).
func TestParseMemoryEventsOOMKill(t *testing.T) {
	cases := []struct {
		name    string
		content string
		count   uint64
		found   bool
	}{
		{"plain", "anon 12\nfile 34\noom_kill 0\n", 0, true},
		{"killed", "anon 12\nfile 34\noom_kill 1\n", 1, true},
		{"grown", "oom_kill 7\n", 7, true},
		{"no counter yet", "anon 12\nfile 34\n", 0, false},
		{"empty", "", 0, false},
		{"no trailing newline", "anon 1\noom_kill 3", 3, true},
		{"garbage counter", "oom_kill x\n", 0, false},
		{"windows line ends", "oom_kill 2\r\n", 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, found := parseMemoryEventsOOMKill(tc.content)
			if count != tc.count || found != tc.found {
				t.Fatalf("parseMemoryEventsOOMKill(%q) = (%d, %v), want (%d, %v)",
					tc.content, count, found, tc.count, tc.found)
			}
		})
	}
}
