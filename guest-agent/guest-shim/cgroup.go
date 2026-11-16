//go:build linux

package main

import (
	"github.com/containerd/log"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/olegshirko/anvil/guest-agent/internal/cgroupv2"
)

// cgroup v2 management for the fast shim: one directory under
// /sys/fs/cgroup (the containerd default spec sets Linux.CgroupsPath to
// "/<namespace>/<id>"), limits written from the spec before Create, the
// init helper moving itself — and with it the container init it creates —
// into the dir via a cgroup.procs write, and freeze/pause via
// cgroup.freeze. The implementation lives in internal/cgroupv2 so the agent's
// direct path uses the same translation.

const cgroupRoot = cgroupv2.Root

// prepareCgroup creates the container's cgroup and applies the spec's
// limits. Returns the absolute path; the init helper moves into it with a
// cgroup.procs write (the children inherit it — the CLONE_INTO_CGROUP
// equivalent for the trampoline design). An empty CgroupsPath (no cgroup
// requested) returns an empty path.
func prepareCgroup(spec *specs.Spec) (string, error) {
	return cgroupv2.Prepare(spec)
}

// cgroupAbsPath resolves the spec's CgroupsPath (may carry a leading slash)
// against the guest's cgroup2 root.
func cgroupAbsPath(cgroupsPath string) string {
	return cgroupv2.AbsPath(cgroupsPath)
}

// removeCgroup deletes the container's cgroup. Frozen state must be lifted
// first or the directory stays referenced.
func removeCgroup(cgPath string) {
	if cgPath == "" {
		return
	}
	if err := cgroupv2.Remove(cgPath); err != nil {
		log.L.WithField("path", cgPath).WithError(err).Warn("remove cgroup")
	}
}
