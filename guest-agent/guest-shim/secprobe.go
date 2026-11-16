//go:build linux

package main

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// secprobe is a diagnostic subcommand replaying the start tail of the init
// (process attrs -> seccomp -> exec) against a real bundle config.json,
// logging each step with wall-clock timestamps. It exists to root-cause
// start hangs like the seccomp one without burning an initramfs cycle per
// hypothesis: run it inside any container with the shim binary bind-mounted
// in and a runc bundle at hand.
//
//	anvil-fast-shim secprobe -bundle /path/to/bundle [-exec /bin/true] [-no-tsync]
//
// SECPROBE_NO_TSYNC / -no-tsync load the filter without
// SECCOMP_FILTER_FLAG_TSYNC (single-thread filter) for bisecting whether the
// cross-thread sync is what freezes a multi-threaded Go process. The flag is
// probe-only: the real init always keeps the shim's behavior.

func runSecProbe(args []string) int {
	bundle := flagValue(args, "-bundle")
	if bundle == "" {
		fmt.Println("secprobe: -bundle is required")
		return 2
	}
	execTarget := flagValue(args, "-exec")
	noTSync := hasFlag(args, "-no-tsync") || os.Getenv("SECPROBE_NO_TSYNC") != ""

	step := func(format string, v ...interface{}) {
		fmt.Printf("%s secprobe: "+format+"\n", append([]interface{}{time.Now().Format("15:04:05.000000")}, v...)...)
	}

	spec, err := readSpecFile(bundle + "/config.json")
	if err != nil {
		step("read spec: %v", err)
		return 1
	}
	if spec.Process == nil {
		step("spec has no process")
		return 1
	}
	step("spec read: args=%v user=%v noNewPrivs=%v seccomp=%d syscalls defaultAction=%v",
		spec.Process.Args, spec.Process.User, spec.Process.NoNewPrivileges,
		len(scSeccompSyscalls(spec)), defaultActionOf(spec))

	if execTarget != "" {
		spec.Process.Args = append([]string{execTarget}, spec.Process.Args[1:]...)
		step("exec target overridden to %q", execTarget)
	}

	step("goroutines before attrs: %d", runtime.NumGoroutine())
	if err := applyProcessAttrsNoSeccomp(spec); err != nil {
		step("applyProcessAttrs (no seccomp): %v", err)
		return 1
	}
	step("attrs applied (caps/user/rlimits/oom), pre-seccomp")

	if spec.Linux != nil && spec.Linux.Seccomp != nil {
		if noTSync {
			if err := applySeccompNoTSync(spec.Linux.Seccomp); err != nil {
				step("applySeccomp(no-tsync): %v", err)
				return 1
			}
		} else {
			if err := applySeccomp(spec.Linux.Seccomp); err != nil {
				step("applySeccomp: %v", err)
				return 1
			}
		}
	}
	// Something between the filter load and the exec must still run for the
	// log below to appear; its absence pins the freeze to the load itself.
	step("filter loaded, post-seccomp")

	step("execving %v", spec.Process.Args)
	if err := unix.Exec(spec.Process.Args[0], spec.Process.Args, spec.Process.Env); err != nil {
		step("exec: %v", err)
		return 127
	}
	return 0 // unreached
}

func flagValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// applyProcessAttrsNoSeccomp is applyProcessAttrs with the seccomp step
// split out, so the probe can log around the filter load exactly like the
// real path cannot.
func applyProcessAttrsNoSeccomp(spec *specs.Spec) error {
	if spec.Linux != nil && spec.Linux.Seccomp != nil {
		saved := spec.Linux.Seccomp
		spec.Linux.Seccomp = nil
		defer func() { spec.Linux.Seccomp = saved }()
	}
	return applyProcessAttrs("", spec)
}

// applySeccompNoTSync is applySeccomp without SECCOMP_FILTER_FLAG_TSYNC —
// the filter then covers only the calling thread. Probe-only bisection
// helper: a multi-threaded Go process left partially unfiltered is NOT a
// candidate production behavior, only evidence.
func applySeccompNoTSync(sc *specs.LinuxSeccomp) error {
	return applySeccompFlagged(sc, 0)
}

func scSeccompSyscalls(spec *specs.Spec) []specs.LinuxSyscall {
	if spec.Linux == nil || spec.Linux.Seccomp == nil {
		return nil
	}
	return spec.Linux.Seccomp.Syscalls
}

func defaultActionOf(spec *specs.Spec) specs.LinuxSeccompAction {
	if spec.Linux == nil || spec.Linux.Seccomp == nil {
		return ""
	}
	return spec.Linux.Seccomp.DefaultAction
}
