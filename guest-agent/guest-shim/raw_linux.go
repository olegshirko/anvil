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
// becomes the user process: close the gate fd (never leak it into the
// container), mark the confirm fd close-on-exec (its closure IS the start
// confirmation — the kernel does it as part of a successful execve) and
// execve. A failed execve reports through the confirm fd with a pre-built
// message and exits 127. Raw syscalls only.
func pid0RawChild(a pid0Args) {
	unix.RawSyscall(unix.SYS_CLOSE, uintptr(a.gateFD), 0, 0) //nolint:errcheck
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

// failExecMessage is the pre-built report the child writes on execve
// failure (the errno itself is not formattable without allocations).
var failExecMessage = []byte("X exec failed (executable not found or blocked)\n")
