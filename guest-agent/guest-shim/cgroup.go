//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containerd/log"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// cgroup v2 management for the fast shim: one directory under
// /sys/fs/cgroup (the containerd default spec sets Linux.CgroupsPath to
// "/<namespace>/<id>"), limits written from the spec before Create, the
// init helper moving itself — and with it the container init it creates —
// into the dir via a cgroup.procs write, and freeze/pause via
// cgroup.freeze.

const cgroupRoot = "/sys/fs/cgroup"

// prepareCgroup creates the container's cgroup and applies the spec's
// limits. Returns the absolute path; the init helper moves into it with a
// cgroup.procs write (the children inherit it — the CLONE_INTO_CGROUP
// equivalent for the trampoline design). An empty CgroupsPath (no cgroup
// requested) returns an empty path.
func prepareCgroup(spec *specs.Spec) (string, error) {
	if spec.Linux == nil || spec.Linux.CgroupsPath == "" {
		return "", nil
	}
	abs := cgroupAbsPath(spec.Linux.CgroupsPath)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("create cgroup %s: %w", abs, err)
	}
	// Domain controllers must be enabled in every ancestor's
	// subtree_control for the limit files to exist. The guest root cgroup
	// is exempt from the no-internal-process rule, so this works even
	// though the agent and containerd live in the root.
	if err := enableControllers(abs); err != nil {
		return "", err
	}
	if err := writeResourceLimits(abs, spec.Linux.Resources); err != nil {
		return "", err
	}
	return abs, nil
}

// cgroupAbsPath resolves the spec's CgroupsPath (may carry a leading slash)
// against the guest's cgroup2 root.
func cgroupAbsPath(cgroupsPath string) string {
	return filepath.Join(cgroupRoot, strings.TrimPrefix(cgroupsPath, "/"))
}

// enableControllers turns on every available controller down to dir.
func enableControllers(dir string) error {
	for {
		parent := filepath.Dir(dir)
		if err := enableAt(parent); err != nil {
			return err
		}
		if parent == cgroupRoot || parent == "/" {
			return nil
		}
		dir = parent
	}
}

func enableAt(dir string) error {
	avail, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // below the controller boundary; nothing to enable
		}
		return fmt.Errorf("read %s/cgroup.controllers: %w", dir, err)
	}
	enabled, err := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("read %s/cgroup.subtree_control: %w", dir, err)
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(string(enabled)) {
		have[c] = true
	}
	var missing []string
	for _, c := range strings.Fields(string(avail)) {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte("+"+strings.Join(missing, " +")), 0o600); err != nil {
		return fmt.Errorf("enable controllers %v at %s: %w", missing, dir, err)
	}
	return nil
}

// writeResourceLimits translates spec.Linux.Resources into cgroup v2 files
// (the same conversion runc's cgroupfs manager does).
func writeResourceLimits(dir string, res *specs.LinuxResources) error {
	if res == nil {
		return nil
	}
	write := func(file, val string) error {
		p := filepath.Join(dir, file)
		if _, err := os.Stat(p); err != nil {
			log.L.WithField("file", p).Debug("cgroup file absent, skipping")
			return nil
		}
		return os.WriteFile(p, []byte(val), 0o600)
	}
	if m := res.Memory; m != nil {
		if m.Limit != nil {
			val := "max"
			if *m.Limit > 0 {
				val = strconv.FormatInt(*m.Limit, 10)
			}
			if err := write("memory.max", val); err != nil {
				return err
			}
		}
		if m.Swap != nil {
			val := "max"
			if *m.Swap > 0 {
				// The spec builder stores docker's --memory-swap (memory +
				// swap combined); cgroup v2's memory.swap.max is swap only,
				// so subtract the memory limit — runc's v1->v2 conversion.
				swap := *m.Swap
				if m.Limit != nil && *m.Limit > 0 {
					swap -= *m.Limit
					if swap < 0 {
						swap = 0
					}
				}
				val = strconv.FormatInt(swap, 10)
			}
			if err := write("memory.swap.max", val); err != nil {
				return err
			}
		}
		if m.Reservation != nil && *m.Reservation > 0 {
			if err := write("memory.low", strconv.FormatInt(*m.Reservation, 10)); err != nil {
				return err
			}
		}
	}
	if c := res.CPU; c != nil {
		period, quota := uint64(100000), int64(0)
		if c.Period != nil && *c.Period != 0 {
			period = *c.Period
		}
		if c.Quota != nil {
			quota = *c.Quota
		}
		cpuMax := "max " + strconv.FormatUint(period, 10)
		if quota > 0 {
			cpuMax = strconv.FormatInt(quota, 10) + " " + strconv.FormatUint(period, 10)
		}
		if err := write("cpu.max", cpuMax); err != nil {
			return err
		}
		if c.Shares != nil && *c.Shares > 0 {
			// runc's shares(2..262144) -> weight(1..10000) conversion.
			weight := 1 + (((*c.Shares - 2) * 9999) / 262142)
			if err := write("cpu.weight", strconv.FormatUint(weight, 10)); err != nil {
				return err
			}
		}
		if c.Cpus != "" {
			if err := write("cpuset.cpus", c.Cpus); err != nil {
				return err
			}
		}
		if c.Mems != "" {
			if err := write("cpuset.mems", c.Mems); err != nil {
				return err
			}
		}
	}
	if p := res.Pids; p != nil && p.Limit != nil && *p.Limit > 0 {
		if err := write("pids.max", strconv.FormatInt(*p.Limit, 10)); err != nil {
			return err
		}
	}
	for k, v := range res.Unified {
		if err := write(k, v); err != nil {
			return err
		}
	}
	return nil
}

// updateCgroup reapplies resource limits (docker update / task Update).
func updateCgroup(cgPath string, res *specs.LinuxResources) error {
	if cgPath == "" || res == nil {
		return nil
	}
	return writeResourceLimits(cgPath, res)
}

// freezeCgroup pauses/unpauses every process in the container (cgroup v2
// freezer; the same mechanism containerd uses for task pause).
func freezeCgroup(cgPath string, freeze bool) error {
	if cgPath == "" {
		return fmt.Errorf("no cgroup for freeze")
	}
	val := "0"
	if freeze {
		val = "1"
	}
	return os.WriteFile(filepath.Join(cgPath, "cgroup.freeze"), []byte(val), 0o600)
}

// killCgroup SIGKILLs every process in the container (cgroup v2 kill file).
func killCgroup(cgPath string) {
	if cgPath == "" {
		return
	}
	os.WriteFile(filepath.Join(cgPath, "cgroup.kill"), []byte("1"), 0o600) //nolint:errcheck — best effort teardown
}

// removeCgroup deletes the container's cgroup. Frozen state must be lifted
// first or the directory stays referenced.
func removeCgroup(cgPath string) {
	if cgPath == "" {
		return
	}
	os.WriteFile(filepath.Join(cgPath, "cgroup.freeze"), []byte("0"), 0o600) //nolint:errcheck
	if err := os.RemoveAll(cgPath); err != nil {
		log.L.WithField("path", cgPath).WithError(err).Warn("remove cgroup")
	}
}
