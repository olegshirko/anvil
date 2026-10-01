//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"golang.org/x/sys/unix"
)

// mountContainerBinds bind-mounts a stopped container's volumes and bind
// mounts onto its mounted rootfs, so docker cp into or out of a container
// that has not started yet (Testcontainers copies files before start) sees
// the same paths a running container does — a file copied to a volume path
// lands in the volume, not hidden underneath it.
func mountContainerBinds(ctx context.Context, c client.Container, root string) {
	spec, err := c.Spec(ctx)
	if err != nil || spec == nil {
		return
	}
	type bind struct {
		src, dst string
		ro       bool
	}
	var binds []bind
	ns, _ := namespaces.Namespace(ctx)
	staged := stagedSubpaths(ns, c.ID())
	for _, m := range spec.Mounts {
		if m.Type != "bind" || m.Source == "" || m.Destination == "" {
			continue
		}
		src := m.Source
		if sp, ok := staged[filepath.Clean(src)]; ok {
			// The staging mountpoint is armed only while the container
			// runs: resolve the subpath in the volume now instead.
			real, err := volumeSubpath(sp.VolumeDir, sp.Subpath)
			if err != nil {
				continue
			}
			src = real
		}
		binds = append(binds, bind{src, m.Destination, slices.Contains(m.Options, "ro")})
	}
	// Parents before children.
	sort.Slice(binds, func(i, j int) bool {
		return strings.Count(filepath.Clean(binds[i].dst), "/") < strings.Count(filepath.Clean(binds[j].dst), "/")
	})
	for _, b := range binds {
		fi, err := os.Stat(b.src)
		if err != nil {
			continue
		}
		target, err := resolveInRoot(root, b.dst)
		if err != nil {
			continue
		}
		if err := ensureMountpoint(target, fi.IsDir()); err != nil {
			continue
		}
		if err := unix.Mount(b.src, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			debugLog("[cp] bind %s -> %s: %v", b.src, b.dst, err)
			continue
		}
		if b.ro {
			// As the container sees it: docker cp into a read-only mount fails.
			unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_REC, "") //nolint:errcheck
		}
	}
}
