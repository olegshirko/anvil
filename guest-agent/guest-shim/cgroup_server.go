//go:build linux && shimserver

package main

import (
	"fmt"
	"os"
	"path/filepath"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/olegshirko/anvil/guest-agent/internal/cgroupv2"
)

// The shim server's cgroup operations (Task Update/Freeze/Kill). They live
// behind the shimserver tag because the helper-only build never serves those
// RPCs; cgroup.go keeps the creation and join paths both builds need.

// updateCgroup reapplies resource limits (docker update / task Update).
func updateCgroup(cgPath string, res *specs.LinuxResources) error {
	if cgPath == "" || res == nil {
		return nil
	}
	return cgroupv2.WriteResourceLimits(cgPath, res)
}

// freezeCgroup pauses/unpauses every process in the container (cgroup v2
// freezer; the same mechanism containerd uses for task pause).
func freezeCgroup(cgPath string, freeze bool) error {
	if cgPath == "" {
		return fmt.Errorf("no cgroup for freeze")
	}
	val := "0"
	if freeze {
		val = "1"
	}
	return os.WriteFile(filepath.Join(cgPath, "cgroup.freeze"), []byte(val), 0o600)
}

// killCgroup SIGKILLs every process in the container (cgroup v2 kill file).
func killCgroup(cgPath string) {
	if cgPath == "" {
		return
	}
	os.WriteFile(filepath.Join(cgPath, "cgroup.kill"), []byte("1"), 0o600) //nolint:errcheck — best effort teardown
}
