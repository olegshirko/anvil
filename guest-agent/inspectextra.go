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

// handleContainerTop implements GET /containers/{id}/top as `ps` over
// every process in the container's cgroup (execs and deep descendants
// included), honoring the ps_args query parameter (-ef default, aux,
// -o fields).
func handleContainerTop(ctx context.Context, w http.ResponseWriter, id, psArgs string) {
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
	now := time.Now()
	bootTime := bootTimeOf(now)
	titles, rows, err := psFormat(collectProcSnapshots(pids, bootTime), now, bootTime, memTotalKB(), psArgs)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"Titles":    titles,
		"Processes": rows,
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

// handleContainerStats implements GET /containers/{id}/stats. With
// stream=false a single reading is returned (what `docker stats
// --no-stream` needs); streaming mode sends one reading per second.
