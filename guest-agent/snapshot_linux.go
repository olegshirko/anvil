//go:build linux

package main

import (
	"os"
	"syscall"

	"github.com/containerd/containerd/v2/core/mount"
)

// mountAll mounts a snapshot's mount list at target.
func mountAll(mounts []mount.Mount, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	return mount.All(mounts, target)
}

// unmountAll unmounts and removes the temporary rootfs mountpoint.
func unmountAll(target string) {
	mount.UnmountRecursive(target, 0) //nolint:errcheck
	syscall.Unmount(target, syscall.MNT_DETACH)
	os.Remove(target)
}
