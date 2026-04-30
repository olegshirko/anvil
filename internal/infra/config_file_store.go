package infra

import (
	"context"
	"fmt"
	"os"

	"anvil/internal/domain"
	"anvil/internal/usecase"
	"anvil/internal/util/yamlutil" // Re-using existing yaml utility

	"gopkg.in/yaml.v3"
)

// ConfigFileStoreImpl is an infrastructure implementation of usecase.ConfigDataStore.
type ConfigFileStoreImpl struct{}

// NewConfigFileStore creates a new ConfigFileStoreImpl.
func NewConfigFileStore() *ConfigFileStoreImpl {
	return &ConfigFileStoreImpl{}
}

// LoadFromPath loads a Config from a specified file path.
func (f *ConfigFileStoreImpl) LoadFromPath(ctx context.Context, path string) (domain.Config, error) {
	var cfg domain.Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("could not load config from file '%s': %w", path, err)
	}

	err = yaml.Unmarshal(b, &cfg)
	if err != nil {
		return cfg, fmt.Errorf("could not unmarshal config from file '%s': %w", path, err)
	}

	return cfg, nil
}

// SaveToPath saves a Config to a specified file path.
func (f *ConfigFileStoreImpl) SaveToPath(ctx context.Context, cfg domain.Config, path string) error {
	return yamlutil.PersistConfig(cfg, path)
}

// ensure ConfigFileStoreImpl implements usecase.ConfigDataStore
var _ usecase.ConfigDataStore = (*ConfigFileStoreImpl)(nil)
