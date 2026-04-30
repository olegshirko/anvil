package usecase

import (
	"context"

	"anvil/internal/domain"
)

// ConfigLoader defines the interface for loading configuration data.
type ConfigLoader interface {
	// Load loads the configuration for a given profile.
	Load(ctx context.Context, profile *domain.Profile) (domain.Config, error)

	// LoadInstanceState loads the configuration of a currently running instance for a profile.
	LoadInstanceState(ctx context.Context, profile *domain.Profile) (domain.Config, error)
}
