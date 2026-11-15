//go:build linux

package main

import (
	"fmt"
	"unsafe"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// clone3 is the one syscall the fast shim is built around: it creates the
// container init process with all pathless namespaces of the spec at once,
// directly inside the prepared cgroup (CLONE_INTO_CGROUP) — no intermediate
// runc processes, no second exec.

// cloneArgs mirrors the kernel's struct clone_args (uapi/linux/sched.h).
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

// cloneInto runs clone3 and returns the child's pid. cgroupFd (-1 = none)
// places the child into that cgroup before it runs its first instruction.
func cloneInto(flags uint64, cgroupFd int) (int, error) {
	args := cloneArgs{
		flags:      flags | uint64(unix.SIGCHLD),
		exitSignal: uint64(unix.SIGCHLD),
	}
	if cgroupFd >= 0 {
		args.flags |= unix.CLONE_INTO_CGROUP
		args.cgroup = uint64(cgroupFd)
	}
	pid, _, errno := unix.RawSyscall(unix.SYS_CLONE3, uintptr(unsafe.Pointer(&args)), unsafe.Sizeof(args), 0)
	if errno != 0 {
		return -1, fmt.Errorf("clone3: %w", errno)
	}
	return int(pid), nil
}
