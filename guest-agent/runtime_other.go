//go:build !linux

package main

import (
	"context"
	"fmt"
)

// Non-linux stubs keep the package — and its unit tests — compilable on the
// development host. The real implementations live in runtime_linux.go.

func createNamedNetNS(name string) (string, error) {
	return "", fmt.Errorf("network namespaces not supported on this platform")
}

func releaseNamedNetNS(name string) {}

func attachNetwork(ctx context.Context, netName, ns, id, netnsPath string, ports []cniPortMapping) (string, string, error) {
	return "", "", fmt.Errorf("CNI not supported on this platform")
}

func detachNetwork(ctx context.Context, netName, ns, id, netnsPath string, ports []cniPortMapping) error {
	return fmt.Errorf("CNI not supported on this platform")
}
