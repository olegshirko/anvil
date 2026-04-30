package environment

import (
	"context"
	"runtime"

	"anvil/internal/domain"
)

// ...
// GuestActions are actions performed on the guest i.e. VM.
type GuestActions interface {
	runActions
	fileActions
	// Start starts up the VM
	Start(ctx context.Context, conf domain.Config) error
	// Stop shuts down the VM
	Stop(ctx context.Context, force bool) error
	// Restart restarts the VM
	Restart(ctx context.Context) error
	// SSH performs an ssh connection to the VM
	SSH(workingDir string, args ...string) error
	// Created returns if the VM has been previously created.
	Created() bool
	// Running returns if the VM is currently running.
	Running(ctx context.Context) bool
	// Env retrieves environment variable in the VM.
	Env(string) (string, error)
	// Setting retrieves a configuration in the VM.
	Setting(key string) string
	// SetSetting sets configuration in the VM.
	SetSetting(key, value string) error
	// User returns the username of the user in the VM.
	User() (string, error)
	// Arch returns the architecture of the VM.
	Arch() Arch
}

// VM configurations
const (
	// ContainerRuntimeKey is the settings key for container runtime.
	ContainerRuntimeKey = "runtime"
)

// Arch is the CPU architecture of the VM.
type Arch = domain.Arch

const (
	X8664   Arch = domain.X8664
	AARCH64 Arch = domain.AARCH64
)

// HostArch returns the host CPU architecture.
func HostArch() Arch {
	return Arch(runtime.GOARCH).Value()
}

// Dependencies are dependencies that must exist on the host.
type Dependencies interface {
	// Dependencies are dependencies that must exist on the host.
	// TODO this may need to accommodate non-brew installable dependencies
	Dependencies() []string
}
