package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
)

// OOM reporting parity with the runc shim: the runc shim watches the
// container cgroup's memory.events and publishes TaskOOM whenever its
// oom_kill counter increases; containerd forwards the event and the agent's
// docker-events stream maps /tasks/oom to the Docker "oom" action. The fast
// shim polls the same file instead of epolling it — the event is not on any
// hot path, and the poll keeps the watcher a dozen lines of plain file
// reads. The watcher runs from create (the cgroup exists by then) until the
// container's delete (the stop channel) or the cgroup's own removal, and is
// container-level only, like the runc shim's.

// oomPollInterval is how often the watcher re-reads memory.events.
const oomPollInterval = 250 * time.Millisecond

// parseMemoryEventsOOMKill extracts the oom_kill counter from a cgroup v2
// memory.events file. found is false when the kernel reports no oom_kill
// line (controllers disabled, old kernel) or the value is unreadable.
func parseMemoryEventsOOMKill(content string) (count uint64, found bool) {
	for _, line := range strings.Split(content, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || key != "oom_kill" {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// watchOOM publishes TaskOOM for the container whenever the cgroup's
// oom_kill counter rises. Exits when oomStop is closed (Delete) or the
// cgroup disappears (its removal also closes the watcher on the runc side).
func (s *taskService) watchOOM(c *container) {
	if c.cgPath == "" {
		return
	}
	events := filepath.Join(c.cgPath, "memory.events")
	var last uint64
	for {
		select {
		case <-c.oomStop:
			return
		case <-time.After(oomPollInterval):
		}
		data, err := os.ReadFile(events)
		if err != nil {
			// The cgroup is gone (delete, or the task cleanup path):
			// nothing left to watch.
			return
		}
		if cur, ok := parseMemoryEventsOOMKill(string(data)); ok && cur > last {
			s.send(&eventstypes.TaskOOM{ContainerID: c.id})
			last = cur
		}
	}
}

// stopOOMWatcher ends the container's OOM watcher before its cgroup goes
// away (safe to call more than once).
func (c *container) stopOOMWatcher() {
	if c.oomStop != nil {
		select {
		case <-c.oomStop:
		default:
			close(c.oomStop)
		}
	}
}
