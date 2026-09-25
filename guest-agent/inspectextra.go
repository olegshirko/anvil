package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"

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
	if serr != nil || st.Status != "running" {
		return 0, false
	}
	return int(task.Pid()), true
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
		return fmt.Errorf("container is not running")
	}
	if pause {
		return task.Pause(nsCtx)
	}
	return task.Resume(nsCtx)
}

// handleContainerTop implements GET /containers/{id}/top: process list of
// the container, read from the task's cgroup procs via the guest.
func handleContainerTop(ctx context.Context, w http.ResponseWriter, id string) {
	ns, containerdID, _, err := resolveDockerID(ctx, id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	pid, ok := containerTaskPid(ctx, ns, containerdID)
	if !ok || pid <= 0 {
		writeJSONError(w, http.StatusConflict, "container not running")
		return
	}
	// Container processes are descendants of the task pid. /proc/<pid>/task/<pid>/children
	// gives direct children (runc init + the app); include the app itself.
	cmdline := fmt.Sprintf(
		`echo "%d $(cat /proc/%d/comm 2>/dev/null)"; kids=$(cat /proc/%d/task/%d/children 2>/dev/null); for k in $kids; do echo "$k $(cat /proc/$k/comm 2>/dev/null)"; ck=$(cat /proc/$k/task/$k/children 2>/dev/null); for c in $ck; do echo "$c $(cat /proc/$c/comm 2>/dev/null)"; done; done`,
		pid, pid, pid, pid)
	out, _, execCode, _ := runGuestShell(cmdline)
	if execCode != 0 {
		out = ""
	}
	titles := []string{"PID", "COMMAND"}
	var processes [][]string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 {
			processes = append(processes, fields)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"Titles":    titles,
		"Processes": processes,
	})
}

// runGuestShell runs a shell one-liner in the guest root (host namespace).
func runGuestShell(script string) (string, string, int, error) {
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(cmd.Env, "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			code = 1
		}
	}
	return outBuf.String(), errBuf.String(), code, err
}

// handleContainerStats implements GET /containers/{id}/stats. With
// stream=false a single reading is returned (what `docker stats
// --no-stream` needs); streaming mode sends one reading per second.

// handleSystemDF implements GET /system/df: reclaimable-space overview.
func handleSystemDF(ctx context.Context, w http.ResponseWriter) {
	images, err := listDockerImages(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	containers, _ := listDockerContainers(ctx, nil)
	volumes, _ := listDockerVolumes(ctx, nil)

	// Mark images referenced by any container as in-use.
	inUse := map[string]bool{}
	for _, c := range containers {
		if c.ImageID != "" {
			inUse[c.ImageID] = true
		}
		if c.Image != "" {
			inUse[c.Image] = true
		}
	}
	var imagesOut []map[string]interface{}
	var imgTotal, imgReclaimable int64
	for _, img := range images {
		size := img.Size
		imgTotal += size
		tag := ""
		if len(img.RepoTags) > 0 {
			tag = img.RepoTags[0]
		}
		used := inUse[tag] || inUse[img.Id]
		if !used {
			imgReclaimable += size
		}
		imagesOut = append(imagesOut, map[string]interface{}{
			"Containers":  -1,
			"CreatedTime": 0,
			"Id":          img.Id,
			"RepoTags":    img.RepoTags,
			"Size":        size,
			"VirtualSize": size,
			"Labels":      nil,
		})
	}
	var contOut []map[string]interface{}
	for _, c := range containers {
		contOut = append(contOut, map[string]interface{}{
			"Id":              c.Id,
			"Names":           c.Names,
			"Image":           c.Image,
			"ImageID":         c.ImageID,
			"Command":         c.Command,
			"Created":         c.Created,
			"Ports":           c.Ports,
			"Labels":          c.Labels,
			"State":           c.State,
			"Status":          c.Status,
			"HostConfig":      map[string]string{"NetworkMode": "default"},
			"NetworkSettings": map[string]interface{}{},
			"Mounts":          []interface{}{},
			"SizeRw":          0,
			"SizeRootFs":      0,
		})
	}
	var volOut []map[string]interface{}
	for _, v := range volumes {
		volOut = append(volOut, map[string]interface{}{
			"Name":       v.Name,
			"Driver":     "local",
			"Mountpoint": volumeDataDir("default", v.Name),
			"Labels":     nil,
			"Scope":      "local",
			"Options":    nil,
			"UsageData":  nil,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"LayersSize":  imgTotal,
		"Images":      imagesOut,
		"Containers":  contOut,
		"Volumes":     volOut,
		"BuildCache":  []interface{}{},
		"BuilderSize": 0,
	})
}
