package environment

import (
	"context"

	"anvil/internal/util"
)

// VirtualMachine abstracts the VM lifecycle and access.
type VirtualMachine interface {
	GuestActions
	Dependencies
	Host() HostActions
	Guest() GuestActions
	Teardown(ctx context.Context) error
}

// DefaultHypervisor returns the recommended VM backend for the current platform.
func DefaultHypervisor() string {
	if util.AppleSiliconAndModernOS() {
		return "vz"
	}
	return "qemu"
}
