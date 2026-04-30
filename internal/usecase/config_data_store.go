package usecase

import (
	"context"

	"anvil/internal/domain"
)

// ConfigDataStore defines the interface for raw storage and retrieval of configuration data.
// It handles the concrete I/O operations without knowledge of profiles or business logic.
type ConfigDataStore interface {
	// LoadFromPath loads a Config from a specified file path.
	LoadFromPath(ctx context.Context, path string) (domain.Config, error)

	// SaveToPath saves a Config to a specified file path.
	SaveToPath(ctx context.Context, cfg domain.Config, path string) error
}
