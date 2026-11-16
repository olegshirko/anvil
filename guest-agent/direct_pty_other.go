//go:build !linux

package main

import (
	"errors"
	"os"
)

// openContainerPty is Linux-only (raw /proc/<pid>/root walk + TIOCGPTPEER);
// elsewhere the caller falls back to a pty from the process's own devpts.
func openContainerPty(int) (master, slave *os.File, err error) {
	return nil, nil, errors.New("container devpts pty requires linux")
}
