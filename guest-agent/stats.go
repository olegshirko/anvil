package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// docker stats from the container's cgroup v2 (every process of the
// container, not just its init) and its network namespace. CPU percentages
// come out right because every reading carries the host CPU time next to
// the container's (system_cpu_usage) and the previous reading (precpu_stats),
// which is what the CLI divides.

type cpuSample struct {
	total  uint64 // container CPU time, ns
	system uint64 // all CPUs' time, ns
}

// clockTicks is USER_HZ, the unit of /proc/stat (100 on Linux).
const clockTicks = 100

func handleContainerStats(ctx context.Context, w http.ResponseWriter, id string, stream bool) {
	w.Header().Set("Content-Type", "application/json")
	ns, containerdID, name, err := resolveDockerID(ctx, id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	did := dockerID(ns, containerdID)
	sample := func(prev *cpuSample) (map[string]any, *cpuSample) {
		pid, ok := containerTaskPid(ctx, ns, containerdID)
		if !ok || pid <= 0 {
			return map[string]any{"name": "/" + name, "id": did, "read": time.Now().UTC().Format(time.RFC3339Nano)}, nil
		}
		return containerStats(pid, did, name, prev)
	}
	enc := json.NewEncoder(w)
	if !stream {
		// Like dockerd: a single reading still carries a precpu sample
		// taken a moment earlier, or the CLI shows 0% CPU.
		_, prev := sample(nil)
		time.Sleep(500 * time.Millisecond)
		reading, _ := sample(prev)
		enc.Encode(reading)
		return
	}
	flusher, _ := w.(http.Flusher)
	var prev *cpuSample
	for {
		reading, cur := sample(prev)
		if enc.Encode(reading) != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		prev = cur
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// containerStats reads one sample for the container whose init is pid.
func containerStats(pid int, did, name string, prev *cpuSample) (map[string]any, *cpuSample) {
	cg := cgroupDir(pid)
	read := func(file string) string {
		b, _ := os.ReadFile(filepath.Join(cg, file))
		return strings.TrimSpace(string(b))
	}
	now := time.Now().UTC()
	cur := &cpuSample{
		total:  parseKeyedUint(read("cpu.stat"))["usage_usec"] * 1000,
		system: procStatTotalNs(),
	}
	cpus := runtime.NumCPU()
	cpuStats := map[string]any{
		"cpu_usage":        map[string]any{"total_usage": cur.total},
		"system_cpu_usage": cur.system,
		"online_cpus":      cpus,
	}
	precpu := map[string]any{"cpu_usage": map[string]any{"total_usage": uint64(0)}}
	preread := "0001-01-01T00:00:00Z"
	if prev != nil {
		precpu = map[string]any{
			"cpu_usage":        map[string]any{"total_usage": prev.total},
			"system_cpu_usage": prev.system,
			"online_cpus":      cpus,
		}
		preread = now.Add(-time.Second).Format(time.RFC3339Nano)
	}

	memStat := parseKeyedUint(read("memory.stat"))
	limit := parseLimit(read("memory.max"), memTotalBytes())
	pidsLimit := parseLimit(read("pids.max"), 0)

	stats := map[string]any{
		"read":         now.Format(time.RFC3339Nano),
		"preread":      preread,
		"name":         "/" + name,
		"id":           did,
		"cpu_stats":    cpuStats,
		"precpu_stats": precpu,
		"memory_stats": map[string]any{
			"usage": parseUint(read("memory.current")),
			"limit": limit,
			// The CLI subtracts inactive_file from usage (cgroup v2).
			"stats": memStat,
		},
		"pids_stats":  map[string]any{"current": parseUint(read("pids.current")), "limit": pidsLimit},
		"blkio_stats": map[string]any{"io_service_bytes_recursive": parseIOStat(read("io.stat"))},
	}
	if nets := parseNetDev(readProc(pid, "net/dev")); len(nets) > 0 {
		stats["networks"] = nets
	}
	return stats, cur
}

func readProc(pid int, file string) string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/%s", pid, file))
	return string(b)
}

// cgroupDir is the guest path of pid's cgroup v2 directory ("0::/path").
func cgroupDir(pid int) string {
	for _, line := range strings.Split(readProc(pid, "cgroup"), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", filepath.Clean("/"+p))
		}
	}
	return "/sys/fs/cgroup/nonexistent"
}

func parseUint(s string) uint64 {
	v, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return v
}

// parseLimit reads a cgroup limit file; "max" means unlimited, reported as
// fallback (Docker shows the machine's memory as the memory limit).
func parseLimit(s string, fallback uint64) uint64 {
	if s == "" || s == "max" {
		return fallback
	}
	return parseUint(s)
}

// parseKeyedUint parses "key value" lines (cpu.stat, memory.stat).
func parseKeyedUint(s string) map[string]uint64 {
	out := map[string]uint64{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			out[f[0]] = parseUint(f[1])
		}
	}
	return out
}

// procStatTotalNs is the time of all CPUs from /proc/stat's "cpu" line.
func procStatTotalNs() uint64 {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := parseProcStatCPU(sc.Text()); ok {
			return v
		}
	}
	return 0
}

// parseProcStatCPU sums the aggregate "cpu" line (in USER_HZ ticks) as ns.
func parseProcStatCPU(line string) (uint64, bool) {
	f := strings.Fields(line)
	if len(f) < 2 || f[0] != "cpu" {
		return 0, false
	}
	var ticks uint64
	for _, v := range f[1:] {
		ticks += parseUint(v)
	}
	return ticks * (1e9 / clockTicks), true
}

func memTotalBytes() uint64 {
	b, _ := os.ReadFile("/proc/meminfo")
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				return parseUint(f[0]) * 1024
			}
		}
	}
	return 0
}

// parseIOStat turns io.stat ("8:0 rbytes=1 wbytes=2 ...") into Docker's
// io_service_bytes_recursive entries.
func parseIOStat(s string) []map[string]any {
	out := []map[string]any{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		maj, min, ok := strings.Cut(f[0], ":")
		if !ok {
			continue
		}
		kv := map[string]uint64{}
		for _, field := range f[1:] {
			if k, v, ok := strings.Cut(field, "="); ok {
				kv[k] = parseUint(v)
			}
		}
		for _, op := range []struct{ key, name string }{{"rbytes", "read"}, {"wbytes", "write"}} {
			out = append(out, map[string]any{"major": parseUint(maj), "minor": parseUint(min), "op": op.name, "value": kv[op.key]})
		}
	}
	return out
}

// parseNetDev turns /proc/<pid>/net/dev into Docker's per-interface network
// stats (loopback excluded, as Docker does).
func parseNetDev(s string) map[string]any {
	out := map[string]any{}
	for _, line := range strings.Split(s, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if name == "lo" || len(f) < 16 {
			continue
		}
		v := func(i int) uint64 { return parseUint(f[i]) }
		out[name] = map[string]any{
			"rx_bytes": v(0), "rx_packets": v(1), "rx_errors": v(2), "rx_dropped": v(3),
			"tx_bytes": v(8), "tx_packets": v(9), "tx_errors": v(10), "tx_dropped": v(11),
		}
	}
	return out
}
