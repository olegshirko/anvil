//go:build linux

package main

import "golang.org/x/sys/unix"

// dupFD makes `to` a copy of `from`. linux/arm64 has no dup2 syscall;
// dup3 with flags=0 is equivalent.
func dupFD(from, to int) error {
	return unix.Dup3(from, to, 0)
}
