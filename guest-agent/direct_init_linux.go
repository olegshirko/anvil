//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/olegshirko/anvil/guest-agent/internal/cgroupv2"
)

// The native container-init helper: a static C binary that executes an init
// plan precomputed by the agent, instead of a Go helper that parses the spec
// and assembles its own seccomp program in a second process. The Go helper
// stays the fallback for every start the native path cannot handle, and
// ANVIL_INIT_IMPL=go|native pins the choice (used for A/B measurements).

const nativeInitHelperName = "/bin/anvil-init-native"

// initImplPref is the ANVIL_INIT_IMPL override: "go", "native" or "" (auto).
func initImplPref() string {
	return strings.ToLower(strings.TrimSpace(os.Getenv("ANVIL_INIT_IMPL")))
}

var (
	nativeProbeOnce sync.Once
	nativeProbeErr  error
)

// nativeInitProbe runs the helper's capability probe once. The probe executes
// exactly the syscall classes the native path depends on — clone3 with
// CLONE_INTO_CGROUP, a new pid namespace, private mount/uts namespaces, tmpfs
// and proc mounts, pivot_root, sethostname, no-new-privs and seccomp — inside
// private namespaces, so a failure is a measured reason to keep the Go
// helper, not a guess about kernel support.
func nativeInitProbe() error {
	nativeProbeOnce.Do(func() {
		if _, err := os.Stat(nativeInitHelperName); err != nil {
			nativeProbeErr = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, nativeInitHelperName, "probe").CombinedOutput()
		if err != nil {
			nativeProbeErr = fmt.Errorf("native init probe: %v (%s)", err, strings.TrimSpace(string(out)))
			return
		}
		debugLog("[direct] native init helper probe ok")
	})
	return nativeProbeErr
}

// newDirectInitCommand builds the init helper invocation for one direct
// start: the argv, an optional cgroup directory to pass as fd 8, and whether
// the native implementation was chosen. Selection is per container: auto mode
// uses the native helper only when the probe passed and the plan is
// expressible; the Go helper takes over otherwise.
func newDirectInitCommand(id, bundle string, spec *specs.Spec) (argv []string, cgroupFile *os.File, native bool, err error) {
	useNative := false
	switch pref := initImplPref(); pref {
	case "native":
		useNative = true
	case "go":
		useNative = false
	default:
		useNative = nativeInitProbe() == nil
	}
	if useNative {
		planPath, perr := buildInitPlan(spec, bundle)
		if perr != nil {
			if initImplPref() == "native" {
				return nil, nil, true, fmt.Errorf("init plan: %w", perr)
			}
			debugLog("[direct] native init plan unavailable, using the Go helper: %v", perr)
		} else {
			// The agent creates the cgroup for the native helper (which then
			// only needs the directory fd for CLONE_INTO_CGROUP); the Go
			// helper creates its own.
			cgPath, cerr := cgroupv2.Prepare(spec)
			if cerr != nil {
				// Prepare may have created the directory before failing on a
				// limit; do not leak it into the containerd fallback.
				if spec.Linux != nil && spec.Linux.CgroupsPath != "" {
					_ = cgroupv2.Remove(cgroupv2.AbsPath(spec.Linux.CgroupsPath))
				}
				return nil, nil, true, cerr
			}
			if cgPath != "" {
				f, oerr := os.Open(cgPath)
				if oerr != nil {
					_ = cgroupv2.Remove(cgPath)
					return nil, nil, true, fmt.Errorf("open cgroup: %w", oerr)
				}
				cgroupFile = f
			}
			argv = []string{nativeInitHelperName, "init", "-id", id, "-plan", planPath}
			if cgroupFile != nil {
				argv = append(argv, "-cgroup")
			}
			return argv, cgroupFile, true, nil
		}
	}
	return []string{fastShimHelperName, "init", "-id", id, "-bundle", bundle, "-cgroup"}, nil, false, nil
}
