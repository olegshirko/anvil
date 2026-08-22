//go:build !linux

package main

import (
	"fmt"

	"github.com/containerd/containerd/v2/core/mount"
)

// Non-linux stubs keep the package — and its unit tests — compilable on the
// development host. The real implementations live in snapshot_linux.go.

func mountAll(mounts []mount.Mount, target string) error {
	return fmt.Errorf("snapshot mounts not supported on this platform")
}

func unmountAll(target string) {}
