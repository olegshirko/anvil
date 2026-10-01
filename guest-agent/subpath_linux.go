package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// armSubpathMounts resolves each volume-subpath mount beneath its volume
// (openat2 RESOLVE_BENEATH: symlinks may not leave it) and binds the
// resolved inode onto the container's staging mountpoint, which is what
// runc mounts. A component swapped for a symlink after this point no
// longer matters: the staging mount pins the inode checked here.
func armSubpathMounts(m *containerMeta) error {
	var legacy []subpathMount
	for _, sp := range m.SubpathMounts {
		if sp.Staging == "" {
			legacy = append(legacy, sp)
			continue
		}
		if err := armSubpath(sp); err != nil {
			return fmt.Errorf("volume subpath %q: %w", sp.Subpath, err)
		}
	}
	return verifySubpathMounts(&containerMeta{SubpathMounts: legacy})
}

func armSubpath(sp subpathMount) error {
	volFD, err := unix.Open(sp.VolumeDir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(volFD)
	rel := strings.TrimPrefix(filepath.Clean("/"+sp.Subpath), "/")
	if rel == "" {
		rel = "."
	}
	fd, err := unix.Openat2(volFD, rel, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(sp.Staging), 0o700); err != nil {
		return err
	}
	// Drop what an earlier start left mounted there.
	for unix.Unmount(sp.Staging, unix.MNT_DETACH) == nil {
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		os.Remove(sp.Staging) // a file placeholder from an earlier run
		if err := os.Mkdir(sp.Staging, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
	} else {
		os.Remove(sp.Staging)
		f, err := os.OpenFile(sp.Staging, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		f.Close()
	}
	return unix.Mount(fmt.Sprintf("/proc/self/fd/%d", fd), sp.Staging, "", unix.MS_BIND, "")
}

// releaseSubpathMounts unmounts a removed container's staging mounts.
func releaseSubpathMounts(ns, id string) {
	dir := filepath.Join(subpathStagingRoot, ns, id)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		for unix.Unmount(p, unix.MNT_DETACH) == nil {
		}
	}
	os.RemoveAll(dir)
}
