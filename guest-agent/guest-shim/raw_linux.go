//go:build linux

package main

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// The raw fork+execve trampoline. Go code cannot run safely in the child of
// a multithreaded fork beyond raw syscalls (runtime locks and allocator
// state are copies), so the container init's last steps are executed as a
// straight syscall sequence with pointers prepared before the fork — the
// same contract os/exec's forkAndExecInChild implements.

// forkRaw is a bare fork(2) — arm64 has no fork syscall, so it goes through
// clone(SIGCHLD): returns 0 in the child, the child's pid in the parent.
func forkRaw() (int, unix.Errno) {
	pid, _, errno := unix.RawSyscall(unix.SYS_CLONE, uintptr(unix.SIGCHLD), 0, 0)
	return int(pid), errno
}

// pid0RawChild is everything the forked container init runs before it
// becomes the user process: join the exec's pid/mount namespaces, chdir,
// close every fd the user process must not inherit (gate, report, the
// confirm pipe's read end, the namespace fds — the shim passes the list),
// mark the confirm fd close-on-exec (its closure IS the start confirmation
// — the kernel does it as part of a successful execve) and execve. A failed
// execve reports through the confirm fd with a pre-built message and exits
// 127. Raw syscalls only.
func pid0RawChild(a pid0Args) {
	// Exec processes join the pid and mount namespaces here: setns(pid) is
	// EINVAL for a multi-threaded process, and only this raw window is
	// single-threaded.
	if a.pidNsFd >= 0 {
		_, _, errno := unix.RawSyscall(unix.SYS_SETNS, uintptr(a.pidNsFd), 0, 0)
		if errno != 0 {
			unix.RawSyscall(unix.SYS_EXIT_GROUP, 126, 0, 0) //nolint:errcheck
		}
	}
	if a.mntNsFd >= 0 {
		_, _, errno := unix.RawSyscall(unix.SYS_SETNS, uintptr(a.mntNsFd), 0, 0)
		if errno != 0 {
			unix.RawSyscall(unix.SYS_EXIT_GROUP, 126, 0, 0) //nolint:errcheck
		}
	}
	if a.cwdPtr != nil {
		_, _, errno := unix.RawSyscall(unix.SYS_CHDIR, uintptr(unsafe.Pointer(a.cwdPtr)), 0, 0)
		if errno != 0 {
			unix.RawSyscall(unix.SYS_WRITE, uintptr(a.confirmFD),
				uintptr(unsafe.Pointer(&failCwdMessage[0])), uintptr(len(failCwdMessage)))
			unix.RawSyscall(unix.SYS_EXIT_GROUP, 126, 0, 0) //nolint:errcheck
		}
	}
	for _, fd := range a.closeFDs {
		unix.RawSyscall(unix.SYS_CLOSE, uintptr(fd), 0, 0) //nolint:errcheck
	}
	// F_SETFD=2, FD_CLOEXEC=1: fcntl(confirmFD, F_SETFD, FD_CLOEXEC).
	unix.RawSyscall(unix.SYS_FCNTL, uintptr(a.confirmFD), 2, 1) //nolint:errcheck
	_, _, errno := unix.RawSyscall6(unix.SYS_EXECVE,
		uintptr(unsafe.Pointer(a.pathPtr)),
		uintptr(unsafe.Pointer(&a.argvPtrs[0])),
		uintptr(unsafe.Pointer(&a.envPtrs[0])),
		0, 0, 0)
	if errno != 0 {
		unix.RawSyscall(unix.SYS_WRITE, uintptr(a.confirmFD),
			uintptr(unsafe.Pointer(a.failMsg)), uintptr(a.failMsgLen))
	}
	unix.RawSyscall(unix.SYS_EXIT_GROUP, 127, 0, 0) //nolint:errcheck
}

// pid0Args carries the pre-built pointers across the fork boundary. The
// namespace fds (pid/mount) are only set for exec processes, which join
// them in the raw window; chdir happens there too (paths resolve in the
// container's mount namespace). closeFDs lists everything closed before
// the execve — the user process must be left with exactly 0/1/2 plus the
// close-on-exec confirm fd.
type pid0Args struct {
	confirmFD  int
	pathPtr    *byte
	argvPtrs   []*byte
	envPtrs    []*byte
	failMsg    *byte
	failMsgLen int
	pidNsFd    int   // -1 = none
	mntNsFd    int   // -1 = none
	cwdPtr     *byte // nil = no chdir
	closeFDs   []int // closed before the execve
}

// failExecMessage is the pre-built report the child writes on execve
// failure (the errno itself is not formattable without allocations).
var failExecMessage = []byte("X exec failed (executable not found or blocked)\n")

// failCwdMessage is the pre-built report for a failed chdir into the exec
// spec's working directory.
var failCwdMessage = []byte("X exec failed (working directory missing)\n")
