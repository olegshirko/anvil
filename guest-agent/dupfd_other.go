//go:build !linux

package main

import "golang.org/x/sys/unix"

func dupFD(from, to int) error {
	return unix.Dup2(from, to)
}
