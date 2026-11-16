//go:build linux

package specutil

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// NamespaceCloneFlag maps an OCI namespace type to its clone(2) flag.
func NamespaceCloneFlag(t specs.LinuxNamespaceType) (uint64, bool) {
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
