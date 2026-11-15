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
// container) and execve. A failed execve is reported through the report fd
// with a pre-built message and the child exits 127. Raw syscalls only.
func pid0RawChild(a pid0Args) {
	unix.RawSyscall(unix.SYS_CLOSE, uintptr(a.gateFD), 0, 0) //nolint:errcheck
	_, _, errno := unix.RawSyscall6(unix.SYS_EXECVE,
		uintptr(unsafe.Pointer(a.pathPtr)),
		uintptr(unsafe.Pointer(&a.argvPtrs[0])),
		uintptr(unsafe.Pointer(&a.envPtrs[0])),
		0, 0, 0)
	if errno != 0 {
		unix.RawSyscall(unix.SYS_WRITE, uintptr(a.reportFD),
			uintptr(unsafe.Pointer(a.failMsg)), uintptr(a.failMsgLen))
	}
	unix.RawSyscall(unix.SYS_EXIT_GROUP, 127, 0, 0) //nolint:errcheck
}

// failExecMessage is the pre-built report the child writes on execve
// failure (the errno itself is not formattable without allocations).
var failExecMessage = []byte("X exec failed (executable not found or blocked)\n")
