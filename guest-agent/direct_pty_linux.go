//go:build linux

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openContainerPty allocates a pty pair inside the container's own devpts
// instance. The container mounts devpts with newinstance, so a pty obtained
// from the agent's /dev/ptmx is a tty whose device node does not exist in the
// container's /dev/pts: isatty(3) is true but ttyname(3) fails, which is why
// `docker exec -t c tty` printed "not a tty". The init's /dev/ptmx is reached
// through /proc/<pid>/root (path resolution crosses into the container's
// mount namespace) and TIOCGPTPEER returns the matching slave fd.
func openContainerPty(initPid int) (master, slave *os.File, err error) {
	ptmx, err := os.OpenFile(fmt.Sprintf("/proc/%d/root/dev/ptmx", initPid), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open container ptmx: %w", err)
	}
	var peer uintptr
	for {
		r, _, errno := unix.Syscall(unix.SYS_IOCTL, ptmx.Fd(), unix.TIOCGPTPEER,
			uintptr(unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC))
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			ptmx.Close() //nolint:errcheck
			return nil, nil, fmt.Errorf("TIOCGPTPEER: %w", errno)
		}
		peer = r
		break
	}
	return ptmx, os.NewFile(peer, "pty-slave"), nil
}
