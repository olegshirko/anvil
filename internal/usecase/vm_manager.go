package usecase

import (
	"context"

	"anvil/internal/domain"
)

// VMManager defines the interface for managing Virtual Machines.
// It represents a core use case of the application.
type VMManager interface {
	// StartVM starts a virtual machine with the given configuration.
	StartVM(ctx context.Context, config domain.Config) error

	// StopVM stops a virtual machine. Forcefully stops if force is true.
	StopVM(ctx context.Context, profileID string, force bool) error

	// DeleteVM deletes a virtual machine.
	DeleteVM(ctx context.Context, profileID string, deleteData, force bool) error

	// GetVMStatus retrieves the current status of a virtual machine.
	GetVMStatus(ctx context.Context, profileID string) (VMStatus, error)

	// IsVMRunning checks if a specific virtual machine is currently running.
	IsVMRunning(ctx context.Context, profileID string) bool

	// SSH executes a shell command in the virtual machine.
	SSH(workDir string, args ...string) error

	// User returns the current user in the virtual machine.
	User() (string, error)

	// Arch returns the architecture of the VM.
	Arch() domain.Arch

	// IPAddress returns the IP address of the VM.
	IPAddress(ctx context.Context, profileID string) string

	// Instance returns information about the running VM instance.
	Instance() (domain.Instance, error)

	// Prune prunes cached VM assets.
	Prune() error
	// Watch watches Lima instance events.
	Watch(args ...string) error
}

// VMStatus represents the detailed status of a virtual machine.
type VMStatus struct {
	ProfileName string
	Running     bool
	Arch        string
	// Add more status fields as needed, e.g., IPAddress, CPU, Memory, etc.
}
