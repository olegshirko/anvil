// Package cgroupv2 owns the container cgroup v2 directory: creation, limit
// translation and removal. Shared by the init helper (which creates the
// cgroup itself on the Go path), the agent (which creates it for the native
// helper and passes the directory to CLONE_INTO_CGROUP) and live updates.
package cgroupv2

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Root is the guest's cgroup v2 mount point.
const Root = "/sys/fs/cgroup"

// AbsPath resolves the spec's CgroupsPath (may carry a leading slash) against
// the guest's cgroup2 root.
func AbsPath(cgroupsPath string) string {
	return filepath.Join(Root, strings.TrimPrefix(cgroupsPath, "/"))
}

// Prepare creates the container's cgroup and applies the spec's limits,
// returning the absolute path (empty when the spec places none).
func Prepare(spec *specs.Spec) (string, error) {
	if spec == nil || spec.Linux == nil || spec.Linux.CgroupsPath == "" {
		return "", nil
	}
	abs := AbsPath(spec.Linux.CgroupsPath)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("create cgroup %s: %w", abs, err)
	}
	if err := EnableControllers(abs); err != nil {
		return "", err
	}
	if err := WriteResourceLimits(abs, spec.Linux.Resources); err != nil {
		return "", err
	}
	return abs, nil
}

// EnableControllers turns on every available controller from the cgroup root
// down to the parent of dir — top-down, not bottom-up. A cgroup's
// cgroup.controllers file lists only the controllers already enabled in its
// parent's subtree_control, so on a cold boot (root subtree_control empty) a
// bottom-up walk found nothing to enable at the namespace cgroup and only
// enabled the root afterwards: the first container after a boot had no
// memory.max, cpu.max or pids.max, while every later one did.
func EnableControllers(dir string) error {
	if dir == Root || !strings.HasPrefix(dir, Root+"/") {
		return nil
	}
	var chain []string
	for d := filepath.Dir(dir); strings.HasPrefix(d, Root); d = filepath.Dir(d) {
		chain = append(chain, d)
		if d == Root {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if err := enableAt(chain[i]); err != nil {
			return err
		}
	}
	return nil
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

// WriteResourceLimits translates spec.Linux.Resources into cgroup v2 files
// (the same conversion runc's cgroupfs manager does).
func WriteResourceLimits(dir string, res *specs.LinuxResources) error {
	if res == nil {
		return nil
	}
	write := func(file, val string) error {
		p := filepath.Join(dir, file)
		if _, err := os.Stat(p); err != nil {
			return nil // controller not enabled for this cgroup: skip
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

// Remove deletes the container's cgroup. Frozen state must be lifted first or
// the directory stays referenced.
func Remove(path string) error {
	if path == "" {
		return nil
	}
	os.WriteFile(filepath.Join(path, "cgroup.freeze"), []byte("0"), 0o600) //nolint:errcheck
	return os.RemoveAll(path)
}
