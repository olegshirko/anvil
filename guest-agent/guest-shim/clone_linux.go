//go:build linux

package main

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"
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

// namespaceCloneFlag maps an OCI namespace type to its clone(2) flag.
func namespaceCloneFlag(t specs.LinuxNamespaceType) (uint64, bool) {
	switch t {
	case specs.MountNamespace:
		return unix.CLONE_NEWNS, true
	case specs.PIDNamespace:
		return unix.CLONE_NEWPID, true
	case specs.IPCNamespace:
		return unix.CLONE_NEWIPC, true
	case specs.UTSNamespace:
		return unix.CLONE_NEWUTS, true
	case specs.CgroupNamespace:
		return unix.CLONE_NEWCGROUP, true
	case specs.NetworkNamespace:
		return unix.CLONE_NEWNET, true
	case specs.TimeNamespace:
		return unix.CLONE_NEWTIME, true
	}
	return 0, false
}
