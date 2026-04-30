package usecase

import (
	"context"

	"anvil/internal/domain"
)

// StatusProvider defines the interface for providing application status.
type StatusProvider interface {
	// GetStatus retrieves the current status of the application.
	GetStatus(ctx context.Context) (domain.StatusInfo, error)

	// GetHealth retrieves the health of the active profile.
	GetHealth(ctx context.Context) (domain.HealthReport, error)

	// ListInstances retrieves a list of VM instances.
	ListInstances(ctx context.Context, profileArgs []string) ([]domain.InstanceInfo, error)
}
