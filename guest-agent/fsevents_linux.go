package main

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func applyFSEvent(ev fsEvent) {
	p := filepath.Clean(ev.Path)
	if !onMacShare(p) {
		return
	}
	// The mode as the Mac has it now (FORCE_SYNC revalidates virtiofs's
	// cached attributes), so the chmod changes nothing there.
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, p, unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_FORCE_SYNC, unix.STATX_MODE, &stx); err != nil {
		return // gone again
	}
	if stx.Mode&unix.S_IFMT == unix.S_IFLNK {
		return // a symlink has no mode of its own to set
	}
	if err := unix.Fchmodat(unix.AT_FDCWD, p, uint32(stx.Mode&0o7777), 0); err != nil && !os.IsNotExist(err) {
		debugLog("[fsevents] %s: %v", p, err)
	}
}
