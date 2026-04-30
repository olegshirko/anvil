package environment

import (
	"context"
	"fmt"

	"anvil/internal/domain"
)

// RuntimeIsNone reports whether the runtime name indicates "no runtime".
func RuntimeIsNone(runtime string) bool { return runtime == "none" }

// ContainerRuntime is the interface implemented by all container backends (docker, containerd, incus).
type ContainerRuntime interface {
	// Name is the runtime identifier, e.g. "docker", "containerd".
	Name() string
	// Provision installs/configures the runtime inside the VM. Must be idempotent.
	Provision(ctx context.Context, config domain.Config) error
	// Start launches the runtime inside the VM.
	Start(ctx context.Context, config domain.Config) error
	// Stop halts the runtime.
	Stop(ctx context.Context) error
	// Teardown removes the runtime from the VM.
	Teardown(ctx context.Context) error
	// Update upgrades the runtime packages if possible.
	Update(ctx context.Context) (bool, error)
	// Version returns the installed runtime version.
	Version(ctx context.Context, profile string) string
	// Running reports whether the runtime is currently active.
	Running(ctx context.Context) bool
	// Host returns host-side actions for this runtime.
	Host() HostActions
	// Guest returns guest-side actions for this runtime.
	Guest() GuestActions

	Dependencies
}

// RuntimeFor creates a container runtime instance by name.
func RuntimeFor(name string, host HostActions, guest GuestActions, profileConfigDir string, profileID string, cacheDir string) (ContainerRuntime, error) {
	if registryErr != nil {
		return nil, registryErr
	}
	entry, ok := runtimeRegistry[name]
	if !ok {
		return nil, fmt.Errorf("unsupported container runtime %q", name)
	}
	return entry.Factory(host, guest, profileConfigDir, profileID, cacheDir), nil
}

// RuntimeFactory is the constructor signature for container runtime implementations.
type RuntimeFactory func(host HostActions, guest GuestActions, profileConfigDir string, profileID string, cacheDir string) ContainerRuntime

var runtimeRegistry = map[string]registeredRuntime{}

var registryErr error

type registeredRuntime struct {
	Factory RuntimeFactory
	Hidden  bool
}

// RegisterRuntime adds a new container runtime to the global registry.
// If hidden is true, the runtime is omitted from user-facing lists.
func RegisterRuntime(name string, f RuntimeFactory, hidden bool) {
	if registryErr != nil {
		return
	}
	if _, exists := runtimeRegistry[name]; exists {
		registryErr = fmt.Errorf("container runtime %q already registered", name)
		return
	}
	runtimeRegistry[name] = registeredRuntime{Factory: f, Hidden: hidden}
}

// RegistryError returns any error encountered during runtime registration.
func RegistryError() error { return registryErr }

// AvailableRuntimes returns the names of all non-hidden runtimes.
func AvailableRuntimes() []string {
	var out []string
	for name, rt := range runtimeRegistry {
		if rt.Hidden {
			continue
		}
		out = append(out, name)
	}
	return out
}

// DataDisk describes an external disk used for runtime data.
type DataDisk struct {
	Dirs     []DiskDir // directories to mount from the disk
	PreMount []string  // scripts to execute before mounting
	FSType   string    // filesystem type, e.g. "ext4"
}

// DiskDir is a single directory mounted from a data disk.
type DiskDir struct {
	Name string
	Path string
}
