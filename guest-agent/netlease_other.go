//go:build !linux

package main

// The guest only runs Linux, but the package is built and tested on the host
// (make unit-tests). Duplicate-address detection needs an AF_PACKET socket and
// the VM console device, so both are no-ops here.

func startLeaseMonitor() {}

func mirrorConsole(string) {}
