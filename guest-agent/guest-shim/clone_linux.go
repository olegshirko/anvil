//go:build linux

package main

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// Raw namespace creation for the fast shim: the container init is born
// inside a fresh pid namespace by a clone(CLONE_NEWPID) in the init helper
// (unshare(CLONE_NEWPID) cannot switch the caller's active namespace, and
// the child must be born inside it); every other namespace of the spec
// joins by setns (pathed) or unshare (pathless) in the born child — no
// intermediate runc processes, no second exec.

// cloneRawPid creates a child inside a NEW pid namespace (clone with
// CLONE_NEWPID: unshare(CLONE_NEWPID) does not switch the caller's active
// namespace, and a Go fork after that unshare fails with ENOMEM — the
// child must be born inside the namespace). Returns the child's pid in the
// caller's namespace (the global pid), 0 in the child.
func cloneRawPid() (int, unix.Errno) {
	pid, _, errno := unix.RawSyscall(unix.SYS_CLONE,
		uintptr(unix.SIGCHLD|unix.CLONE_NEWPID), 0, 0)
	return int(pid), errno
}

// cloneIntoCGroupp is CLONE_INTO_CGROUP (clone3 only): the child is born
// directly inside the cgroup of the passed directory fd, instead of being
// moved there afterwards. The later self-move (write to cgroup.procs) was
// measured at ~27 ms of pure wait against ~0.03 ms of CPU on the dev guest;
// the born-in placement costs 0.03-0.15 ms there.
const cloneIntoCGroupp = uint64(1) << 33

// cloneArgs mirrors struct clone_args (linux/sched.h) up to the cgroup
// field (kernel 5.7+; the guest runs 6.6).
type cloneArgs struct {
	flags      uint64
	pidfd      uint64
	childTid   uint64
	parentTid  uint64
	exitSignal uint64
	stack      uint64
	stackSize  uint64
	tls        uint64
	setTid     uint64
	setTidSize uint64
	cgroup     uint64
}

// cloneRawPidInto creates the child inside a NEW pid namespace AND inside
// the cgroup of cgroupFD. Falls back to the plain clone (caller then moves
// the child) when cgroupFD < 0 or the kernel rejects clone3.
func cloneRawPidInto(cgroupFD int) (int, unix.Errno) {
	if cgroupFD < 0 {
		return cloneRawPid()
	}
	args := cloneArgs{
		// clone3 keeps the exit signal out of flags (CSIGNAL bits must be
		// zero there), unlike clone(2).
		flags:      uint64(unix.CLONE_NEWPID) | cloneIntoCGroupp,
		exitSignal: uint64(unix.SIGCHLD),
		cgroup:     uint64(cgroupFD),
	}
	r1, _, errno := unix.RawSyscall(unix.SYS_CLONE3,
		uintptr(unsafe.Pointer(&args)), unsafe.Sizeof(args), 0)
	if errno == unix.ENOSYS || errno == unix.EINVAL || errno == unix.EOPNOTSUPP {
		return cloneRawPid()
	}
	return int(r1), errno
}
