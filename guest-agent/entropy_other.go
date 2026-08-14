//go:build !linux

package main

import (
	"fmt"
	"os"
)

// rndAddEntropyIoctl is Linux-only (the RNDADDENTROPY ioctl). The stub lets
// the package — and its unit tests — compile on the development host.
func rndAddEntropyIoctl(f *os.File, info []byte) error {
	return fmt.Errorf("RNDADDENTROPY not supported on this platform")
}
