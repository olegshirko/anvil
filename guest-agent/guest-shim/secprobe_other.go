//go:build !linux

package main

// runSecProbe is the Linux seccomp diagnostic (secprobe.go); the shim
// package must still compile on darwin for vet and tests.

import "fmt"

func runSecProbe([]string) int {
	fmt.Println("secprobe: linux-only")
	return 2
}
