package usecase

import (
	"context"

	"anvil/internal/domain"
)

// ContainerManager defines the interface for managing container runtimes (e.g., Docker, Containerd, Incus).
type ContainerManager interface {
	// Provision prepares the container runtime.
	Provision(ctx context.Context, config domain.Config) error

	// Start starts the container runtime.
	Start(ctx context.Context, config domain.Config) error

	// Stop stops the container runtime.
	Stop(ctx context.Context, config domain.Config) error

	// Teardown cleans up the container runtime.
	Teardown(ctx context.Context, config domain.Config) error

	// GetContainerRuntimeStatus retrieves the current status of the container runtime.
	GetContainerRuntimeStatus(ctx context.Context, config domain.Config) (ContainerRuntimeStatus, error)

	// IsContainerRuntimeRunning checks if the container runtime is currently running.
	IsContainerRuntimeRunning(ctx context.Context, config domain.Config) bool

	// HostSocketFile returns the path to the host socket file for the container runtime.
	HostSocketFile() string

	// HostSocketFiles returns the paths to the host socket files for containerd.
	HostSocketFiles() domain.HostSocketFiles

	// IsKubernetesRunning checks if Kubernetes is running.
	IsKubernetesRunning(ctx context.Context) bool

	// CurrentRuntime returns the name of the current container runtime.
	CurrentRuntime(ctx context.Context) (string, error)

	// Version returns the version of the container runtime.
	Version(ctx context.Context) (string, error)

	// Name returns the name of the container runtime.
	Name() string

	// Update updates the container runtime.
	Update(ctx context.Context) (bool, string, error)
}

// ContainerRuntimeStatus represents the detailed status of a container runtime.
type ContainerRuntimeStatus struct {
	Name    string
	Running bool
	// Add more status fields as needed
}

// ContainerManagerFactory defines the interface for creating container managers.
type ContainerManagerFactory interface {
	// Get returns a container manager for the given runtime.
	Get(runtime string) (ContainerManager, error)
}
