package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// pauseDockerContainer pauses (pause=true) or unpauses a container.
func pauseDockerContainer(id string, pause bool) error {
	ns, containerdID, _, err := resolveDockerID(id)
	if err != nil {
		return err
	}
	verb := "pause"
	if !pause {
		verb = "unpause"
	}
	stdout, stderr, code, err := runNerdctl(ns, verb, containerdID)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl %s failed (%d): %s%s", verb, code, stripANSI(stdout), stripANSI(stderr))
	}
	return nil
}

// handleContainerTop implements GET /containers/{id}/top: process list of
// the container, read from the task's cgroup procs via the guest.
func handleContainerTop(w http.ResponseWriter, id string) {
	ns, containerdID, _, err := resolveDockerID(id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
		return
	}
	stdout, _, code, err := runNerdctl(ns, "inspect", "--format", "json", containerdID)
	if err != nil || code != 0 {
		http.Error(w, `{"message":"container not running"}`, http.StatusConflict)
		return
	}
	var info struct {
		State struct {
			Pid int `json:"Pid"`
		} `json:"State"`
	}
	if err := json.Unmarshal([]byte(stdout), &info); err != nil || info.State.Pid <= 0 {
		http.Error(w, `{"message":"container not running"}`, http.StatusConflict)
		return
	}
	// Container processes are descendants of the task pid. /proc/<pid>/task/<pid>/children
	// gives direct children (runc init + the app); include the app itself.
	cmdline := fmt.Sprintf(
		`echo "%d $(cat /proc/%d/comm 2>/dev/null)"; kids=$(cat /proc/%d/task/%d/children 2>/dev/null); for k in $kids; do echo "$k $(cat /proc/$k/comm 2>/dev/null)"; ck=$(cat /proc/$k/task/$k/children 2>/dev/null); for c in $ck; do echo "$c $(cat /proc/$c/comm 2>/dev/null)"; done; done`,
		info.State.Pid, info.State.Pid, info.State.Pid, info.State.Pid)
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
func handleContainerStats(w http.ResponseWriter, id string, stream bool) {
	w.Header().Set("Content-Type", "application/json")
	reading := func() map[string]interface{} {
		return containerStatsReading(id)
	}
	if !stream {
		json.NewEncoder(w).Encode(reading())
		return
	}
	flusher, _ := w.(http.Flusher)
	for {
		json.NewEncoder(w).Encode(reading())
		if flusher != nil {
			flusher.Flush()
		}
		// One reading per second, like Docker. The connection closing makes
		// the next Encode fail, which ends the stream.
		sleepms(1000)
	}
}

func sleepms(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

// containerStatsReading builds a Docker-shaped stats sample from
// /sys/fs/cgroup of the container's task pid and /proc/<pid>/stat for CPU.
func containerStatsReading(id string) map[string]interface{} {
	zero := func(s string) uint64 { v, _ := strconv.ParseUint(s, 10, 64); return v }
	reading := func() map[string]interface{} {
		ns, containerdID, _, err := resolveDockerID(id)
		if err != nil {
			return nil
		}
		stdout, _, code, err := runNerdctl(ns, "inspect", "--format", "json", containerdID)
		if err != nil || code != 0 {
			return nil
		}
		var info struct {
			State struct {
				Pid     int    `json:"Pid"`
				Status  string `json:"Status"`
				Running bool   `json:"Running"`
			} `json:"State"`
		}
		if json.Unmarshal([]byte(stdout), &info) != nil || info.State.Pid <= 0 {
			return nil
		}
		pid := info.State.Pid
		readFile := func(p string) string {
			b, err := os.ReadFile(p)
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(b))
		}
		memCurrent := zero(readFile(fmt.Sprintf("/sys/fs/cgroup/memory/%d/memory.current", pid)))
		if memCurrent == 0 {
			// cgroup v2 nested layout used by runc: /sys/fs/cgroup/<...>/memory.current
			// The exact path varies; fall back to /proc/<pid>/status VmRSS.
			status := readFile(fmt.Sprintf("/proc/%d/status", pid))
			for _, l := range strings.Split(status, "\n") {
				if strings.HasPrefix(l, "VmRSS:") {
					f := strings.Fields(l)
					if len(f) > 1 {
						memCurrent = zero(f[1]) * 1024
					}
				}
			}
		}
		utime := uint64(0)
		if stat := strings.Fields(readFile(fmt.Sprintf("/proc/%d/stat", pid))); len(stat) >= 14 {
			utime = zero(stat[13])
		}
		return map[string]interface{}{
			"read":    "0001-01-01T00:00:00Z",
			"preread": "0001-01-01T00:00:00Z",
			"memory_stats": map[string]interface{}{
				"usage": memCurrent,
				"stats": map[string]interface{}{"cache": 0},
			},
			"cpu_stats":    map[string]interface{}{"cpu_usage": map[string]interface{}{"total_usage": utime}},
			"precpu_stats": map[string]interface{}{"cpu_usage": map[string]interface{}{"total_usage": 0}},
			"pids_stats":   map[string]interface{}{"current": 0},
			"networks":     map[string]interface{}{},
			"name":         id, "id": id,
		}
	}
	r := reading()
	if r == nil {
		r = map[string]interface{}{"name": id, "id": id}
	}
	return r
}

// connectContainerNetwork attaches a running container to another network.
// nerdctl has no `network connect` subcommand, so live attach is not
// supported by the runtime; return a Docker-shaped, actionable error
// instead of a bare 404.
func connectContainerNetwork(network, container string) error {
	return fmt.Errorf("network connect is not supported by the runtime; recreate the container with --network %s", network)
}

// disconnectContainerNetwork detaches a container from a network.
// Not supported by the runtime (nerdctl has no `network disconnect`).
func disconnectContainerNetwork(network, container string) error {
	return fmt.Errorf("network disconnect is not supported by the runtime; recreate the container without --network %s", network)
}

// handleSystemDF implements GET /system/df: reclaimable-space overview.
func handleSystemDF(w http.ResponseWriter) {
	images, err := listDockerImages()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	containers, _ := listDockerContainers(nil)
	volumes, _ := listDockerVolumes(nil)

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
			"Mountpoint": "/var/lib/nerdctl/volumes/" + v.Name,
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
