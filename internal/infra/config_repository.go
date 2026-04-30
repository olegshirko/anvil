package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"anvil/internal/domain"
	"anvil/internal/store"
	"anvil/internal/usecase"

	"github.com/sirupsen/logrus"
)

// ensure ConfigRepositoryImpl implements usecase.ConfigRepository
var _ usecase.ConfigRepository = (*ConfigRepositoryImpl)(nil)

// ensure ProfileManagerImpl implements usecase.ProfileManager
var _ usecase.ProfileManager = (*ProfileManagerImpl)(nil)

// isWSL checks if the current environment is Windows Subsystem for Linux.
// This is a simplified check based on common WSL environment variables.
func isWSL() bool {
	return os.Getenv("WSL_DISTRO_NAME") != ""
}

// getUserHomeDir gets the user's home directory.
func getUserHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Fallback or panic, depending on application needs.
		// For now, return empty string or log error.
		return ""
	}
	return home
}

// ConfigRepositoryImpl is an infrastructure implementation of ConfigRepository.
type ConfigRepositoryImpl struct {
	configFileName  string
	limaDirFunc     func() string
	configBaseDir   func() string
	storeDirFunc    func() string
	cacheDirFunc    func() string
	configDataStore usecase.ConfigDataStore
	configHelper    *usecase.ConfigHelper
	profileMan      usecase.ProfileManager
}

// NewConfigRepository creates a new ConfigRepositoryImpl.
func NewConfigRepository(configDataStore usecase.ConfigDataStore, profileMan usecase.ProfileManager) *ConfigRepositoryImpl {
	homeDirProvider := &SystemHomeDirProvider{}
	osInfoProvider := &SystemOSInfoProvider{}
	configHelper := usecase.NewConfigHelper(homeDirProvider, osInfoProvider)

	return &ConfigRepositoryImpl{
		configFileName: "config.yaml",
		limaDirFunc: func() string {
			home := getUserHomeDir()
			if isWSL() {
				return filepath.Join(home, ".lima")
			}
			return filepath.Join(home, ".lima")
		},
		configBaseDir: func() string {
			home := getUserHomeDir()
			if isWSL() {
				return filepath.Join(home, ".config", domain.AppName)
			}
			return filepath.Join(home, ".config", domain.AppName)
		},
		storeDirFunc: func() string {
			home := getUserHomeDir()
			return filepath.Join(home, ".local", "share", domain.AppName)
		},
		cacheDirFunc: func() string { // Initialized cacheDirFunc
			if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
				return filepath.Join(dir, domain.AppName)
			}
			home, err := os.UserCacheDir()
			if err != nil {
				return filepath.Join(getUserHomeDir(), ".cache", domain.AppName) // Fallback
			}
			return filepath.Join(home, domain.AppName)
		},
		configDataStore: configDataStore,
		configHelper:    configHelper,
		profileMan:      profileMan,
	}
}

// SetRuntime persists the container runtime.
func (r *ConfigRepositoryImpl) SetRuntime(ctx context.Context, runtime string) error {
	profile := r.profileMan.GetCurrentProfile(ctx)
	storeFile := r.GetProfileStoreFile(profile)
	err := store.Mutate(storeFile, func(s *store.State) {
		// update runtime if runtime disk is in use
		if s.DiskFormatted {
			s.DiskRuntime = runtime
		}
	})

	if err != nil {
		logrus.Traceln("error persisting store:", err)
	}

	return nil
}

// SetKubernetes persists the Kubernetes configuration.
func (r *ConfigRepositoryImpl) SetKubernetes(ctx context.Context, kubernetes domain.Kubernetes) error {
	b, err := json.Marshal(kubernetes)
	if err != nil {
		return err
	}

	profile := r.profileMan.GetCurrentProfile(ctx)
	storeFile := r.GetProfileStoreFile(profile)
	if err := store.Mutate(storeFile, func(s *store.State) {
		s.Kubernetes = string(b)
	}); err != nil {
		return fmt.Errorf("error persisting kubernetes store: %w", err)
	}

	return nil
}

// GetLimaDir returns the directory for Lima.
func (r *ConfigRepositoryImpl) GetLimaDir() string {
	return r.limaDirFunc()
}

// GetCacheDir returns the cache directory.
func (r *ConfigRepositoryImpl) GetCacheDir() string {
	return r.cacheDirFunc()
}

// GetSSHConfigFile returns the path to the generated SSH config file.
func (r *ConfigRepositoryImpl) GetSSHConfigFile() string {
	return filepath.Join(r.configBaseDir(), "ssh_config")
}

// ConfigMountsOrDefault returns the configured mounts or a default set.
func (r *ConfigRepositoryImpl) ConfigMountsOrDefault(c domain.Config) ([]domain.Mount, error) {
	return r.configHelper.ConfigMountsOrDefault(c)
}

// ConfigDriverLabel returns the driver label for the VM type.
func (r *ConfigRepositoryImpl) ConfigDriverLabel(c domain.Config) string {
	return r.configHelper.ConfigDriverLabel(c)
}

// GetProfileStoreFile returns the path to the store file for a profile.
func (r *ConfigRepositoryImpl) GetProfileStoreFile(profile *domain.Profile) string {
	return filepath.Join(r.storeDirFunc(), profile.ID+".json")
}

// Load loads the configuration for a given profile.
func (r *ConfigRepositoryImpl) Load(ctx context.Context, profile *domain.Profile) (domain.Config, error) {
	cfg, err := r.configDataStore.LoadFromPath(ctx, r.GetProfileConfigPath(profile))
	if err != nil {
		return cfg, err
	}
	r.applyDefaults(&cfg)
	return cfg, nil
}

// Save saves the configuration for a given profile.
func (r *ConfigRepositoryImpl) Save(ctx context.Context, profile *domain.Profile, cfg domain.Config) error {
	return r.configDataStore.SaveToPath(ctx, cfg, r.GetProfileConfigPath(profile))
}

// LoadInstanceState loads the configuration of a currently running instance for a profile.
func (r *ConfigRepositoryImpl) LoadInstanceState(ctx context.Context, profile *domain.Profile) (domain.Config, error) {
	cfg, err := r.configDataStore.LoadFromPath(ctx, r.GetProfileStatePath(profile))
	if err != nil {
		return cfg, err
	}
	r.applyDefaults(&cfg)
	return cfg, nil
}

// Teardown deletes the configuration for a given profile.
func (r *ConfigRepositoryImpl) Teardown(ctx context.Context, profile *domain.Profile) error {
	dir := r.ProfileConfigDir(profile)
	if _, err := os.Stat(dir); err == nil {
		return os.RemoveAll(dir)
	}
	return nil
}

// GetProfileConfigPath returns the path to the main configuration file for a profile.
func (r *ConfigRepositoryImpl) GetProfileConfigPath(profile *domain.Profile) string {
	return filepath.Join(r.ProfileConfigDir(profile), r.configFileName)
}

// GetProfileStatePath returns the path to the state file for a profile.
func (r *ConfigRepositoryImpl) GetProfileStatePath(profile *domain.Profile) string {
	return filepath.Join(r.GetProfileLimaInstanceDir(profile), r.configFileName)
}

// GetProfileLimaFile returns the path to the Lima configuration file for a profile.
func (r *ConfigRepositoryImpl) GetProfileLimaFile(profile *domain.Profile) string {
	return filepath.Join(r.GetProfileLimaInstanceDir(profile), "lima.yaml")
}

// GetProfileLimaInstanceDir returns the directory for the Lima instance for a profile.
func (r *ConfigRepositoryImpl) GetProfileLimaInstanceDir(profile *domain.Profile) string {
	return filepath.Join(r.limaDirFunc(), profile.ID)
}

// applyDefaults sets default values for missing config fields.
func (r *ConfigRepositoryImpl) applyDefaults(cfg *domain.Config) {
	if cfg.ImageVersion == "" {
		cfg.ImageVersion = "24.04"
	}
}

// ProfileConfigDir returns the configuration directory for a profile.
func (r *ConfigRepositoryImpl) ProfileConfigDir(profile *domain.Profile) string {
	return filepath.Join(r.configBaseDir(), profile.ShortName)
}

// GetTemplatesDir returns the directory for template files.
func (r *ConfigRepositoryImpl) GetTemplatesDir() string {
	return filepath.Join(r.configBaseDir(), "_templates")
}

// ConfigDataStore returns the underlying ConfigDataStore for direct access.
func (r *ConfigRepositoryImpl) ConfigDataStore() usecase.ConfigDataStore {
	return r.configDataStore
}

// ConfigHelper returns the ConfigHelper associated with the repository.
func (r *ConfigRepositoryImpl) ConfigHelper() *usecase.ConfigHelper {
	return r.configHelper
}

// ProfileManagerImpl is an infrastructure implementation of ProfileManager.
type ProfileManagerImpl struct {
	currentProfile *domain.Profile
}

// NewProfileManager creates a new ProfileManagerImpl.
func NewProfileManager() *ProfileManagerImpl {
	return &ProfileManagerImpl{
		currentProfile: &domain.Profile{ID: domain.AppName, DisplayName: domain.AppName, ShortName: "default"},
	}
}

// GetCurrentProfile returns the currently active profile.
func (m *ProfileManagerImpl) GetCurrentProfile(ctx context.Context) *domain.Profile {
	return m.currentProfile
}

// GetProfileFromName retrieves a profile given name.
func (m *ProfileManagerImpl) GetProfileFromName(ctx context.Context, name string) *domain.Profile {
	var i domain.Profile

	switch name {
	case "", domain.AppName, "default":
		i.ID = domain.AppName
		i.DisplayName = domain.AppName
		i.ShortName = "default"
		return &i
	}

	// sanitize
	name = strings.TrimPrefix(name, domain.AppName+"-")

	// if custom profile is specified,
	// use a prefix to prevent possible name clashes
	i.ID = domain.AppName + "-" + name
	i.DisplayName = domain.AppName + " [profile=" + name + "]"
	i.ShortName = name
	return &i
}

// SetProfile sets the profile name for the application.
func (m *ProfileManagerImpl) SetProfile(ctx context.Context, profileName string) {
	m.currentProfile = m.GetProfileFromName(ctx, profileName)
	m.currentProfile.Changed = true
}
