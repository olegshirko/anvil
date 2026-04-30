package infra

import (
	"context"

	"anvil/internal/usecase"
	"strings"
)

// ensure PathValidatorImpl implements usecase.PathValidator
var _ usecase.PathValidator = (*PathValidatorImpl)(nil)

// PathValidatorImpl is an infrastructure implementation of usecase.PathValidator.
type PathValidatorImpl struct {
	configRepo usecase.ConfigRepository
	profileMan usecase.ProfileManager
}

// NewPathValidator creates a new PathValidatorImpl.
func NewPathValidator(
	configRepo usecase.ConfigRepository,
	profileMan usecase.ProfileManager,
) *PathValidatorImpl {
	return &PathValidatorImpl{
		configRepo: configRepo,
		profileMan: profileMan,
	}
}

// IsMounted checks if a path is mounted.
func (v *PathValidatorImpl) IsMounted(path string) (bool, error) {
	ctx := context.Background()
	profile := v.profileMan.GetCurrentProfile(ctx)
	conf, err := v.configRepo.LoadInstanceState(ctx, profile)
	if err != nil {
		return false, err
	}

	homeDirProvider := &SystemHomeDirProvider{}
	homeDir, err := homeDirProvider.GetHomeDir()
	if err != nil {
		return false, err
	}

	pwd, err := usecase.CleanPath(path, homeDir)
	if err != nil {
		return false, err
	}

	mounts, err := v.configRepo.ConfigMountsOrDefault(conf)
	if err != nil {
		return false, err
	}
	for _, m := range mounts {
		location := m.MountPoint
		if location == "" {
			location = m.Location
		}
		location, err := usecase.CleanPath(location, homeDir)
		if err != nil {
			continue
		}
		if strings.HasPrefix(pwd, location) {
			return true, nil
		}
	}

	return false, nil
}
