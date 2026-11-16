//go:build linux

package main

import (
	"fmt"
	"unsafe"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/olegshirko/anvil/guest-agent/internal/specutil"
)

// Seccomp confinement: the agent applies containerd's default profile (docker's
// syscall allowlist, argument filters are Equal-style personality/socket
// constraints only) to every non-privileged container, so the shim must
// install it without libseccomp. specutil.BuildSeccompFilter assembles the
// classic socket-filter program; this file loads it. Custom profiles are
// excluded from fast-shim eligibility by the agent, and the argument
// operators beyond the single-value ones fail loudly at build time (Create
// fails, the runc shim fallback takes over).

// applySeccomp installs the filter in the current (init) process. Must run
// last, right before the user exec — the filter applies to this process.
// no-new-privileges is always set with it: the default container capability
// set has no CAP_SYS_ADMIN, and seccomp requires one or the other (runc
// does the same for its default-caps containers).
func applySeccomp(sc *specs.LinuxSeccomp) error {
	// TSYNC keeps every thread of this (Go, multi-threaded) process inside
	// the same filter. The probe passes 0 instead to bisect whether this
	// cross-thread sync is what freezes the process between the load and
	// the exec.
	return applySeccompFlagged(sc, unix.SECCOMP_FILTER_FLAG_TSYNC)
}

// applySeccompFlagged loads the assembled filter with the given seccomp(2)
// flags. The probe passes 0 to bisect whether the cross-thread sync is what
// freezes the process.
//
// The filter installs through the seccomp(2) syscall, NOT prctl: the
// SECCOMP_SET_MODE_* values are seccomp(2) operation codes and collide with
// the prctl SECCOMP_MODE_* values — prctl(PR_SET_SECCOMP, 1) would arm
// SECCOMP_MODE_STRICT (read/write/exit only, everything else SIGKILL), which
// silently killed every filtered container. seccomp(2) also carries the
// TSYNC flag, which the prctl path ignores.
func applySeccompFlagged(sc *specs.LinuxSeccomp, flags uintptr) error {
	sockFilter, err := specutil.BuildSeccompFilter(sc)
	if err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	// seccomp(SECCOMP_SET_MODE_FILTER, flags, &fprog) with the patched
	// program. The operation code is the seccomp(2) one: passing it to
	// prctl(PR_SET_SECCOMP) instead collides with SECCOMP_MODE_STRICT
	// (= 1 on the prctl interface) and arms strict mode.
	fprog := &unix.SockFprog{
		Len:    uint16(len(sockFilter)),
		Filter: &sockFilter[0],
	}
	if _, _, errno := unix.RawSyscall6(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		flags, uintptr(unsafe.Pointer(fprog)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("load seccomp filter: %w", errno)
	}
	return nil
}
