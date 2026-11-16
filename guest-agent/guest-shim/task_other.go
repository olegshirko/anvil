//go:build !linux

package main

import (
	"os"
	"syscall"
)

// Non-Linux stand-ins: the helper modes only ever run in the Linux guest,
// but the package must build on the development Mac (go vet / go test ./...)
// with and without the shimserver tag. The server-side stand-ins live in
// task_server_other.go behind the shimserver tag.

// stdioSet is the io plumbing type; off Linux it is an empty shell.
type stdioSet struct{}

// The helper entries are Linux-only; off Linux they never trigger.
func isInitHelper() bool { return false }
func isInitApply() bool  { return false }
func isExecHelper() bool { return false }
func isExecApply() bool  { return false }
func runInit(id, bundle string, ownCgroup bool) int {
	_, _, _ = id, bundle, ownCgroup
	return 1
}

func runInitApply(id, bundle string) int {
	_, _ = id, bundle
	return 1
}

func runExecHelper(initpid, bundle string) int {
	_, _ = initpid, bundle
	return 1
}

func runExecApply() int { return 1 }

// pidAlive reports whether the process is alive (Signal 0 probe).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
