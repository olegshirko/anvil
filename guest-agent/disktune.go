package main

import (
	"os"
	"path/filepath"
)

// tuneBlockDevices applies the virtio-blk queue settings that keep
// metadata-heavy container I/O low-latency. The host-side APFS/virtio stack
// already batches requests, so guest-level merging and scheduler overhead are
// disabled and the queues kept short.
//
// Runs shortly after the control listener is up, never before it: the queue
// reconfiguration costs ~100 ms of guest uptime (the stage2 markers showed
// exactly that between varlib_mounted and the agent exec), and the settings
// only shape I/O for the containers that run later. Tuning in a background
// stage2 subshell still competed with the agent's startup and delayed the
// listener, so it moved here.
func tuneBlockDevices() {
	settings := []struct{ file, value string }{
		{"scheduler", "none"},
		{"read_ahead_kb", "256"},
		{"nr_requests", "256"},
		{"nomerges", "2"},
	}
	for _, dev := range []string{"vda", "vdb", "vdc"} {
		queue := filepath.Join("/sys/block", dev, "queue")
		if _, err := os.Stat(queue); err != nil {
			continue
		}
		for _, s := range settings {
			if err := os.WriteFile(filepath.Join(queue, s.file), []byte(s.value+"\n"), 0o644); err != nil {
				debugLog("[disk] tune /dev/%s/%s: %v", dev, s.file, err)
			}
		}
		debugLog("[disk] tuned /dev/%s", dev)
	}
	bootMark("disk_tuned")
}
