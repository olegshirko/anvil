//go:build linux

package main

import (
	"fmt"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// clone3 is the one syscall the fast shim is built around: it creates the
// container init process with all pathless namespaces of the spec at once,
// directly inside the prepared cgroup (CLONE_INTO_CGROUP) — no intermediate
// runc processes, no second exec.

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

// namespaceFlags builds the clone3 flags for every pathless namespace in the
// spec. Namespaces with a path (a pre-created netns, --pid host) are joined
// by the init helper with setns instead — clone3 cannot join existing ones.
func namespaceFlags(spec *specs.Spec) (uint64, error) {
	var flags uint64
	for _, ns := range spec.Linux.Namespaces {
		if ns.Path != "" {
			continue
		}
		f, ok := namespaceCloneFlag(ns.Type)
		if !ok {
			return 0, fmt.Errorf("unsupported namespace %q", ns.Type)
		}
		flags |= f
	}
	return flags, nil
}
