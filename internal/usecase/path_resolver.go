package usecase

import "anvil/internal/domain"

// PathResolver defines the interface for resolving profile-specific file paths.
type PathResolver interface {
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

	// GetLimaDir returns the directory for Lima.
	GetLimaDir() string

	// GetCacheDir returns the cache directory.
	GetCacheDir() string

	// GetSSHConfigFile returns the path to the generated SSH config file.
	GetSSHConfigFile() string
}
