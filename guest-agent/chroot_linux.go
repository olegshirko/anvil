//go:build linux

package main

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// inChroot runs fn on a dedicated OS thread whose root directory is dir.
//
// unshare(CLONE_FS) gives the thread its own root/cwd, so the chroot does
// not affect the agent's other threads. The goroutine never unlocks the
// thread: when it returns, the Go runtime destroys the tainted thread
// instead of reusing it, and while it is locked the runtime creates new
// threads from a clean template thread, never by cloning this one.
//
// fn must do all its file-system work on the calling goroutine — anything
// it hands to another goroutine runs outside the chroot.
func inChroot(dir string, fn func() error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			errc <- fmt.Errorf("unshare fs: %w", err)
			return
		}
		if err := unix.Chroot(dir); err != nil {
			errc <- fmt.Errorf("chroot %s: %w", dir, err)
			return
		}
		if err := unix.Chdir("/"); err != nil {
			errc <- fmt.Errorf("chdir: %w", err)
			return
		}
		errc <- fn()
	}()
	return <-errc
}
