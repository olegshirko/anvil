//go:build linux

package main

import (
	"strings"

	"golang.org/x/sys/unix"
)

// lowerLookup opens the lower layer directories and returns a check for
// whether a path exists (and is not a whiteout) in the topmost layer that
// has it. Lookups stay beneath each layer and refuse symlinks on the way:
// layers come from images, and following one of their symlinks would stat
// guest paths. The descriptors outlive a chroot (overlayChanges runs in one).
func lowerLookup(lowers []string) (func(p string) bool, func(), error) {
	var fds []int
	closeAll := func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}
	for _, l := range lowers {
		fd, err := unix.Open(l, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		fds = append(fds, fd)
	}
	how := &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	check := func(p string) bool {
		rel := strings.TrimPrefix(p, "/")
		for _, dir := range fds {
			fd, err := unix.Openat2(dir, rel, how)
			if err != nil {
				continue
			}
			var st unix.Stat_t
			serr := unix.Fstat(fd, &st)
			unix.Close(fd)
			if serr != nil {
				continue
			}
			whiteout := st.Mode&unix.S_IFMT == unix.S_IFCHR && st.Rdev == 0
			return !whiteout
		}
		return false
	}
	return check, closeAll, nil
}
