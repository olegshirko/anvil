//go:build !linux

package main

import "errors"

// inChroot is linux-only (see chroot_linux.go).
func inChroot(dir string, fn func() error) error {
	return errors.New("chroot is not supported on this platform")
}
