package usecase

import (
	"context"

	"anvil/internal/domain"
)

// DoctorService defines the interface for diagnosing host readiness.
type DoctorService interface {
	// Diagnose checks host readiness and returns a list of diagnoses.
	Diagnose(ctx context.Context) ([]domain.Diagnosis, error)
}
