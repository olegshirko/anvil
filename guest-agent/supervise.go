package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// containerd is started by stage2 and nothing restarted it: an OOM kill or a
// crash left every Docker call failing until the VM was cold-booted. The
// supervisor protects it (and the agent) from the OOM killer, starts it
// again when it is gone, and keeps the logs that live in RAM or on the
// share bounded.

const (
	containerdBin     = "/opt/containerd/bin/containerd"
	containerdLogPath = "/tmp/containerd.log"
	// The guest rootfs is RAM: a chatty containerd log is memory.
	maxDaemonLogBytes = 16 << 20
)

// protectFromOOM makes the kernel pick a container before pid.
func protectFromOOM(pid int) {
	path := "/proc/self/oom_score_adj"
	if pid > 0 {
		path = "/proc/" + strconv.Itoa(pid) + "/oom_score_adj"
	}
	if err := os.WriteFile(path, []byte("-999"), 0o644); err != nil {
		log.Printf("[supervise] oom_score_adj %s: %v", path, err)
	}
}

// vmContainerd returns the pid of the VM's own containerd: a child of PID 1
// running containerdBin. DinD and kind node containers run processes named
// containerd too; those must neither stand in for it nor get its score.
func vmContainerd() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(data)
		open, end := strings.IndexByte(s, '('), strings.LastIndex(s, ")")
		if open < 0 || end < open || end+2 >= len(s) {
			continue
		}
		fields := strings.Fields(s[end+1:])
		if s[open+1:end] != "containerd" || len(fields) < 2 || fields[0] == "Z" || fields[1] != "1" {
			continue
		}
		if exe, err := os.Readlink("/proc/" + e.Name() + "/exe"); err == nil && exe == containerdBin {
			return pid
		}
	}
	return 0
}

func startContainerd() (int, error) {
	logFile, err := os.OpenFile(containerdLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	cmd := exec.Command(containerdBin)
	cmd.Env = []string{"PATH=/bin:/sbin:/usr/bin:/usr/sbin:/opt/containerd/bin", "HOME=/root"}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// No Wait(): PID 1's orphan reaper collects it.
	return cmd.Process.Pid, nil
}

// truncateIfLarge empties a log opened in append mode by its writer once
// it grows past max (writers keep appending at the new end).
func truncateIfLarge(path string, max int64) {
	if st, err := os.Stat(path); err == nil && st.Size() > max {
		if err := os.Truncate(path, 0); err == nil {
			log.Printf("[supervise] truncated %s (%d bytes)", path, st.Size())
		}
	}
}

// superviseContainerd runs for the agent's lifetime.
func superviseContainerd() {
	protectFromOOM(0)
	<-bootFinalized
	protected := 0
	misses := 0
	for {
		if pid := vmContainerd(); pid > 0 {
			misses = 0
			if pid != protected {
				protectFromOOM(pid)
				protected = pid
			}
		} else if misses++; misses >= 2 {
			// Two consecutive misses: not a restart racing the scan.
			misses = 0
			log.Printf("[supervise] containerd is not running; starting it")
			if pid, err := startContainerd(); err != nil {
				log.Printf("[supervise] start containerd: %v", err)
			} else {
				protectFromOOM(pid)
				protected = pid
			}
		}
		truncateIfLarge(containerdLogPath, maxDaemonLogBytes)
		truncateIfLarge(filepath.Join(anvilRunDir, "buildkitd.log"), maxDaemonLogBytes)
		time.Sleep(2 * time.Second)
	}
}
