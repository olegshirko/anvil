package usecase

import (
	"context"

	"anvil/internal/domain"
)

// ConfigRepository defines the interface for persisting and retrieving configuration data.
// It also handles profile-specific file paths.
type ConfigRepository interface {
	// Load loads the configuration for a given profile.
	Load(ctx context.Context, profile *domain.Profile) (domain.Config, error)

	// Save saves the configuration for a given profile.
	Save(ctx context.Context, profile *domain.Profile, config domain.Config) error

	// LoadInstance loads the configuration of a currently running instance for a profile.
	LoadInstanceState(ctx context.Context, profile *domain.Profile) (domain.Config, error)

	// Teardown deletes the configuration for a given profile.
	Teardown(ctx context.Context, profile *domain.Profile) error

	// GetProfileConfigPath returns the path to the main configuration file for a profile.
	GetProfileConfigPath(profile *domain.Profile) string

	// GetProfileStatePath returns the path to the state file for a profile.
	GetProfileStatePath(profile *domain.Profile) string

	// GetProfileLimaFile returns the path to the Lima configuration file for a profile.
	GetProfileLimaFile(profile *domain.Profile) string

	// GetProfileLimaInstanceDir returns the directory for the Lima instance for a profile.
	GetProfileLimaInstanceDir(profile *domain.Profile) string

	// GetProfileStoreFile returns the path to the store file for a profile.
	GetProfileStoreFile(profile *domain.Profile) string

	// ProfileConfigDir returns the configuration directory for a profile.
	ProfileConfigDir(profile *domain.Profile) string

	// GetTemplatesDir returns the directory for template files.
	GetTemplatesDir() string

	// ConfigDataStore returns the underlying ConfigDataStore for direct access.
	ConfigDataStore() ConfigDataStore

	// GetLimaDir returns the directory for Lima.
	GetLimaDir() string

	// GetCacheDir returns the cache directory.
	GetCacheDir() string

	// GetSSHConfigFile returns the path to the generated SSH config file.
	GetSSHConfigFile() string

	// ConfigMountsOrDefault returns the configured mounts or a default set.
	ConfigMountsOrDefault(c domain.Config) ([]domain.Mount, error)

	// ConfigDriverLabel returns the driver label for the VM type.
	ConfigDriverLabel(c domain.Config) string

	// SetRuntime persists the container runtime.
	SetRuntime(ctx context.Context, runtime string) error

	// SetKubernetes persists the Kubernetes configuration.
	SetKubernetes(ctx context.Context, kubernetes domain.Kubernetes) error

	// ConfigHelper returns the ConfigHelper associated with the repository.
	ConfigHelper() *ConfigHelper
}

// ProfileManager defines the interface for managing profiles.
type ProfileManager interface {
	// GetCurrentProfile returns the currently active profile.
	GetCurrentProfile(ctx context.Context) *domain.Profile

	// GetProfileFromName retrieves a profile by its name.
	GetProfileFromName(ctx context.Context, name string) *domain.Profile

	// SetProfile sets the currently active profile.
	SetProfile(ctx context.Context, name string)
}
