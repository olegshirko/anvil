//go:build linux

package main

import (
	"golang.org/x/sys/unix"
)

// Raw fork helpers shared by the init and exec helpers. Go code cannot run
// safely in the child of a multithreaded fork beyond raw syscalls (runtime
// locks and allocator state are copies), so every window between the fork
// and the exec is a straight syscall sequence with pointers prepared before
// the fork — the same contract os/exec's forkAndExecInChild implements.

// forkRaw is a bare fork(2) — arm64 has no fork syscall, so it goes through
// clone(SIGCHLD): returns 0 in the child, the child's pid in the parent.
func forkRaw() (int, unix.Errno) {
	pid, _, errno := unix.RawSyscall(unix.SYS_CLONE, uintptr(unix.SIGCHLD), 0, 0)
	return int(pid), errno
}
