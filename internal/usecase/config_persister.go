package usecase

import (
	"context"

	"anvil/internal/domain"
)

// ConfigPersister defines the interface for persisting configuration changes.
type ConfigPersister interface {
	// Save saves the configuration for a given profile.
	Save(ctx context.Context, profile *domain.Profile, config domain.Config) error

	// Teardown deletes the configuration for a given profile.
	Teardown(ctx context.Context, profile *domain.Profile) error

	// SetRuntime persists the container runtime.
	SetRuntime(ctx context.Context, runtime string) error

	// SetKubernetes persists the Kubernetes configuration.
	SetKubernetes(ctx context.Context, kubernetes domain.Kubernetes) error
}
