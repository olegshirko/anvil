//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/log"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// rootfs setup, running inside the init helper (already in the container's
// namespaces): spec mounts applied onto the prepared rootfs, standard device
// nodes created, masked/read-only paths enforced, then pivot_root. The
// algorithm follows runc's rootfs_linux step by step; destinations are
// resolved against the rootfs while still outside, so relative binds keep
// working after the switch.

// mountFlagOpts maps fstab-style mount options to syscall flags and mount
// data. bind options are extracted separately.
func mountFlagOpts(opts []string) (int, []string) {
	var flags int
	var data []string
	for _, o := range opts {
		switch o {
		case "ro":
			flags |= unix.MS_RDONLY
		case "rw":
		case "nosuid":
			flags |= unix.MS_NOSUID
		case "nodev":
			flags |= unix.MS_NODEV
		case "noexec":
			flags |= unix.MS_NOEXEC
		case "relatime":
			flags |= unix.MS_RELATIME
		case "noatime":
			flags |= unix.MS_NOATIME
		case "nodiratime":
			flags |= unix.MS_NODIRATIME
		case "strictatime":
			flags |= unix.MS_STRICTATIME
		case "suid", "dev", "exec", "atime", "diratime", "lazytime":
			if o == "lazytime" {
				flags |= unix.MS_LAZYTIME
			}
		case "bind", "rbind", "rprivate", "rslave", "rshared", "private", "slave", "shared", "unbindable", "runbindable", "recursive", "norecursive", "copy=sloppy":
			// mount topology handled separately; nothing here
		default:
			data = append(data, o)
		}
	}
	return flags, data
}

// applySpecMounts mounts every spec mount onto the rootfs (pre-pivot).
func applySpecMounts(rootfs string, spec *specs.Spec) error {
	for i := range spec.Mounts {
		m := &spec.Mounts[i]
		// runc's mountpoint rule: a bind mount whose source is a regular
		// file binds onto a file. The image usually ships one already
		// (alpine's /etc/hosts), and mkdir-ing over it fails with
		// ENOTDIR - create the target as a file in that case.
		fileBind := false
		if m.Type == "bind" {
			if fi, serr := os.Stat(m.Source); serr == nil && !fi.IsDir() {
				fileBind = true
			}
		}
		dst, err := safeJoin(rootfs, m.Destination)
		if err != nil {
			return err
		}
		if fileBind {
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
			}
			if _, serr := os.Stat(dst); serr != nil {
				f, ferr := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644)
				if ferr != nil {
					return fmt.Errorf("create mountpoint file %s: %w", dst, ferr)
				}
				f.Close()
			}
		} else if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dst, err)
		}
		flags, data := mountFlagOpts(m.Options)
		dataStr := strings.Join(data, ",")

		switch m.Type {
		case "bind":
			var bflags int = unix.MS_BIND
			for _, o := range m.Options {
				if o == "rbind" {
					bflags |= unix.MS_REC
				}
			}
			if err := unix.Mount(m.Source, dst, "", uintptr(bflags), ""); err != nil {
				return fmt.Errorf("bind %s -> %s: %w", m.Source, m.Destination, err)
			}
			// Re-mount with the combined options (ro, nosuid...); the
			// first bind ignores them, like util-linux's mount --bind.
			if err := unix.Mount("", dst, "", uintptr(bflags|unix.MS_REMOUNT|flags), dataStr); err != nil {
				return fmt.Errorf("remount bind %s: %w", m.Destination, err)
			}
		case "cgroup":
			// cgroup v2. With a private cgroup namespace (the docker
			// default) the container must see its OWN cgroup subtree at
			// /sys/fs/cgroup: mount a fresh cgroup2 hierarchy — the new
			// mount is rooted at the namespace root, which is the
			// container's cgroup (stage A moved into it / stage B was
			// born into it). Binding the guest's existing mount instead
			// exposes the whole guest hierarchy: a bind copy keeps the
			// superblock root and the cgroup namespace does not rewrite
			// an already-mounted view, so memory.max/cpu.max/pids.max
			// were absent. With a shared cgroup namespace (--cgroupns
			// host) runc binds the guest root, and so do we.
			if !cgroupNamespacePrivate(spec) {
				if err := unix.Mount("/sys/fs/cgroup", dst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
					return fmt.Errorf("bind cgroup root to %s: %w", m.Destination, err)
				}
				if err := unix.Mount("", dst, "", uintptr(unix.MS_BIND|unix.MS_REC|unix.MS_REMOUNT|flags), ""); err != nil {
					return fmt.Errorf("remount cgroup root %s: %w", m.Destination, err)
				}
				break
			}
			if err := unix.Mount("cgroup2", dst, "cgroup2", uintptr(flags), dataStr); err != nil {
				return fmt.Errorf("mount cgroup2 %s: %w", m.Destination, err)
			}
		default:
			src := m.Source
			if src == "" {
				src = m.Type
			}
			if err := unix.Mount(src, dst, m.Type, uintptr(flags), dataStr); err != nil {
				return fmt.Errorf("mount %s %s: %w", m.Type, m.Destination, err)
			}
		}
	}
	return nil
}

// cgroupNamespacePrivate reports whether the spec gives the container its own
// cgroup namespace (a pathless CgroupNamespace entry). That is what makes a
// freshly mounted cgroup2 hierarchy root at the container's cgroup; a shared
// namespace (--cgroupns host: no entry) keeps the guest-wide view.
func cgroupNamespacePrivate(spec *specs.Spec) bool {
	if spec.Linux == nil {
		return false
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.CgroupNamespace {
			return ns.Path == ""
		}
	}
	return false
}

// safeJoin joins dest under root, refusing escapes.
func safeJoin(root, dest string) (string, error) {
	p := filepath.Join(root, filepath.Clean("/"+dest))
	if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("mount destination %q escapes rootfs", dest)
	}
	return p, nil
}

// standardDevices are created inside the container's /dev unless already
// present (runc's createDevices).
var standardDevices = []struct {
	path string
	mode uint32
	dev  uint64
}{
	{"null", 0o666, mkdev(1, 3)},
	{"zero", 0o666, mkdev(1, 5)},
	{"full", 0o666, mkdev(1, 7)},
	{"random", 0o666, mkdev(1, 8)},
	{"urandom", 0o666, mkdev(1, 9)},
	{"tty", 0o666, mkdev(5, 0)},
}

func mkdev(major, minor uint64) uint64 {
	return minor | (major << 8) // glibc's makedev for <256 majors
}

// setupDevices creates the standard and spec-requested device nodes plus
// the usual /dev symlinks, inside the freshly mounted tmpfs /dev.
func setupDevices(rootfs string, spec *specs.Spec) error {
	devDir := filepath.Join(rootfs, "dev")
	for _, d := range standardDevices {
		p := filepath.Join(devDir, d.path)
		if err := unix.Mknod(p, unix.S_IFCHR|d.mode, int(d.dev)); err != nil && !os.IsExist(err) {
			return fmt.Errorf("mknod /dev/%s: %w", d.path, err)
		}
	}
	// Devices requested with --device (spec.Linux.Devices).
	if spec.Linux != nil {
		for _, d := range spec.Linux.Devices {
			p := filepath.Join(devDir, strings.TrimPrefix(d.Path, "/dev"))
			mode := uint32(0o666)
			if d.FileMode != nil {
				mode = uint32(*d.FileMode & 0o7777)
			}
			typ := unix.S_IFCHR
			switch d.Type {
			case "b":
				typ = unix.S_IFBLK
			case "p":
				typ = unix.S_IFIFO
			case "u", "c", "":
			}
			dev := int(mkdev(uint64(d.Major), uint64(d.Minor)))
			if err := unix.Mknod(p, uint32(typ)|mode, dev); err != nil && !os.IsExist(err) {
				log.L.WithField("path", d.Path).Warnf("mknod requested device: %v", err)
			}
		}
	}
	symlinks := [][2]string{
		{"/proc/self/fd", "fd"},
		{"/proc/self/fd/0", "stdin"},
		{"/proc/self/fd/1", "stdout"},
		{"/proc/self/fd/2", "stderr"},
		{"pts/ptmx", "ptmx"},
	}
	for _, l := range symlinks {
		p := filepath.Join(devDir, l[1])
		os.Remove(p) //nolint:errcheck
		if err := unix.Symlink(l[0], p); err != nil && !os.IsExist(err) {
			return fmt.Errorf("symlink /dev/%s: %w", l[1], err)
		}
	}
	return nil
}

// applyMaskedAndReadonly enforces Linux.MaskedPaths (bind /dev/null over)
// and Linux.ReadonlyPaths (read-only bind of the path onto itself),
// pre-pivot, with destinations inside the rootfs.
func applyMaskedAndReadonly(rootfs string, spec *specs.Spec) error {
	if spec.Linux == nil {
		return nil
	}
	for _, p := range spec.Linux.MaskedPaths {
		dst, err := safeJoin(rootfs, p)
		if err != nil {
			continue
		}
		if _, statErr := os.Stat(dst); statErr != nil {
			continue // nothing to mask
		}
		if err := unix.Mount("/dev/null", dst, "", unix.MS_BIND, ""); err != nil {
			log.L.WithField("path", p).Debugf("mask path: %v", err)
		}
	}
	for _, p := range spec.Linux.ReadonlyPaths {
		dst, err := safeJoin(rootfs, p)
		if err != nil {
			continue
		}
		if _, statErr := os.Stat(dst); statErr != nil {
			continue
		}
		if err := unix.Mount(dst, dst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			continue
		}
		if err := unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REC|unix.MS_RDONLY|unix.MS_REMOUNT, ""); err != nil {
			log.L.WithField("path", p).Debugf("readonly path: %v", err)
		}
	}
	return nil
}

// pivotRoot moves the process into the prepared rootfs. Mirrors runc: a
// temporary dir under the new root receives the old root, which is then
// lazily unmounted. When the rootfs is not a separate mount (pivot_root
// EINVAL), fall back to MS_MOVE + chroot.
func pivotRoot(rootfs string) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / rprivate: %w", err)
	}
	oldRoot := filepath.Join(rootfs, ".pivot_root")
	os.RemoveAll(oldRoot) //nolint:errcheck
	if err := os.Mkdir(oldRoot, 0o700); err != nil {
		return fmt.Errorf("mkdir pivot point: %w", err)
	}
	if err := unix.PivotRoot(rootfs, oldRoot); err != nil {
		// EINVAL: rootfs is not a mount. Move it over the current root and
		// chroot instead (runc's fallback).
		if err != unix.EINVAL {
			return fmt.Errorf("pivot_root %s: %w", rootfs, err)
		}
		if err := unix.Mount(rootfs, "/", "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("MS_MOVE rootfs: %w", err)
		}
		if err := unix.Chroot("."); err != nil {
			return fmt.Errorf("chroot after MS_MOVE: %w", err)
		}
		if err := unix.Chdir("/"); err != nil {
			return err
		}
		return nil
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	// The old root now sits at /.pivot_root; detach it and drop the dir.
	if err := unix.Unmount("/.pivot_root", unix.MNT_DETACH); err != nil {
		log.L.Debugf("unmount old root: %v", err)
	}
	os.RemoveAll("/.pivot_root") //nolint:errcheck
	return nil
}

// applySysctls writes spec.Linux.Sysctl into /proc/sys (the proc mount of
// the container's own namespaces).
func applySysctls(sysctls map[string]string) error {
	for k, v := range sysctls {
		p := filepath.Join("/proc/sys", strings.ReplaceAll(k, ".", "/"))
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			return fmt.Errorf("sysctl %s=%s: %w", k, v, err)
		}
	}
	return nil
}
