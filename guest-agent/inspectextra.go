package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// containerTaskPid returns the host-side PID of the container's init process.
func containerTaskPid(ctx context.Context, ns, containerdID string) (int, bool) {
	cl, err := pc.get(ctx)
	if err != nil {
		return 0, false
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, lerr := cl.LoadContainer(nsCtx, containerdID)
	if lerr != nil {
		return 0, false
	}
	task, terr := c.Task(nsCtx, nil)
	if terr != nil {
		return 0, false
	}
	st, serr := task.Status(nsCtx)
	if serr != nil || (st.Status != "running" && st.Status != "paused") {
		return 0, false
	}
	return int(task.Pid()), true
}

// containerTaskPids lists every process of the container's task, from its
// own cgroup (nested cgroups included): DinD and systemd move their init
// into a child cgroup, so walking the init's cgroup missed their siblings.
func containerTaskPids(ctx context.Context, ns, containerdID string) []int {
	cl, err := pc.get(ctx)
	if err != nil {
		return nil
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, containerdID)
	if err != nil {
		return nil
	}
	task, err := c.Task(nsCtx, nil)
	if err != nil {
		return nil
	}
	procs, err := task.Pids(nsCtx)
	if err != nil {
		return nil
	}
	pids := make([]int, 0, len(procs))
	for _, p := range procs {
		pids = append(pids, int(p.Pid))
	}
	slices.Sort(pids)
	return pids
}

// pauseDockerContainer pauses (pause=true) or unpauses a container by
// signalling the containerd task directly.
func pauseDockerContainer(ctx context.Context, id string, pause bool) error {
	ns, containerdID, _, err := resolveDockerID(ctx, id)
	if err != nil {
		return err
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, lerr := cl.LoadContainer(nsCtx, containerdID)
	if lerr != nil {
		return lerr
	}
	task, terr := c.Task(nsCtx, nil)
	if terr != nil {
		return errConflict("container %s is not running", truncateID(containerdID))
	}
	st, serr := task.Status(nsCtx)
	if serr == nil {
		switch {
		case pause && st.Status == "paused":
			return errConflict("container %s is already paused", truncateID(containerdID))
		case !pause && st.Status != "paused":
			return errConflict("container %s is not paused", truncateID(containerdID))
		case st.Status != "running" && st.Status != "paused":
			return errConflict("container %s is not running", truncateID(containerdID))
		}
	}
	action, op := "unpause", task.Resume
	if pause {
		action, op = "pause", task.Pause
	}
	if err := op(nsCtx); err != nil {
		return err
	}
	publishContainerEvent(action, ns, containerdID, nil)
	return nil
}

// handleContainerTop implements GET /containers/{id}/top as `ps -ef` over
// every process in the container's cgroup (execs and deep descendants
// included).
func handleContainerTop(ctx context.Context, w http.ResponseWriter, id string) {
	ns, containerdID, _, err := resolveDockerID(ctx, id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	pid, ok := containerTaskPid(ctx, ns, containerdID)
	if !ok || pid <= 0 {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("container %s is not running", truncateID(containerdID)))
		return
	}
	pids := containerTaskPids(ctx, ns, containerdID)
	if len(pids) == 0 {
		pids = cgroupPids(cgroupDir(pid))
	}
	if len(pids) == 0 {
		pids = []int{pid}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"Titles":    []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"},
		"Processes": psRows(pids, time.Now()),
	})
}

// cgroupPids lists the processes of a cgroup and its descendants, sorted.
func cgroupPids(dir string) []int {
	var pids []int
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err != nil || d.IsDir() || d.Name() != "cgroup.procs" {
			return nil
		}
		data, _ := os.ReadFile(p)
		for _, f := range strings.Fields(string(data)) {
			if n, err := strconv.Atoi(f); err == nil {
				pids = append(pids, n)
			}
		}
		return nil
	})
	slices.Sort(pids)
	return slices.Compact(pids)
}

// psRows renders processes the way `ps -ef` does.
func psRows(pids []int, now time.Time) [][]string {
	const hz = 100 // USER_HZ
	var bootTime time.Time
	procStat, _ := os.ReadFile("/proc/stat")
	for _, line := range strings.Split(string(procStat), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				bootTime = time.Unix(sec, 0)
			}
		}
	}
	rows := [][]string{}
	for _, pid := range pids {
		stat := readProc(pid, "stat")
		end := strings.LastIndex(stat, ")")
		if end < 0 {
			continue // exited meanwhile
		}
		comm := stat[strings.IndexByte(stat, '(')+1 : end]
		f := strings.Fields(stat[end+1:])
		if len(f) < 20 {
			continue
		}
		// f[0]=state f[1]=ppid f[4]=tty_nr f[11]=utime f[12]=stime f[19]=starttime
		ppid := f[1]
		cpuTicks := parseUint(f[11]) + parseUint(f[12])
		started := bootTime.Add(time.Duration(parseUint(f[19])) * time.Second / hz)
		elapsed := now.Sub(started).Seconds()
		c := 0
		if elapsed > 0 {
			c = int(float64(cpuTicks) / hz / elapsed * 100)
		}
		stime := started.Format("15:04")
		if now.Sub(started) > 24*time.Hour {
			stime = started.Format("Jan02")
		}
		tty := "?"
		if nr := parseUint(f[4]); nr != 0 {
			major, minor := (nr>>8)&0xfff, (nr&0xff)|((nr>>12)&0xfff00)
			if major == 136 {
				tty = fmt.Sprintf("pts/%d", minor)
			}
		}
		secs := cpuTicks / hz
		cpuTime := fmt.Sprintf("%02d:%02d:%02d", secs/3600, secs/60%60, secs%60)
		uid := "?"
		for _, line := range strings.Split(readProc(pid, "status"), "\n") {
			if v, ok := strings.CutPrefix(line, "Uid:"); ok {
				if fs := strings.Fields(v); len(fs) > 0 {
					uid = fs[0]
				}
			}
		}
		if uid == "0" {
			uid = "root"
		}
		cmd := strings.TrimSpace(strings.ReplaceAll(readProc(pid, "cmdline"), "\x00", " "))
		if cmd == "" {
			cmd = "[" + comm + "]"
		}
		rows = append(rows, []string{uid, strconv.Itoa(pid), ppid, strconv.Itoa(c), stime, tty, cpuTime, cmd})
	}
	return rows
}

// handleContainerStats implements GET /containers/{id}/stats. With
// stream=false a single reading is returned (what `docker stats
// --no-stream` needs); streaming mode sends one reading per second.
