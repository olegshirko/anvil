package infra

import (
	"context"
	"fmt"
	"os"

	"anvil/internal/environment/vm/lima/limautil"
	"anvil/internal/usecase"
)

// ensure SSHConfigManagerImpl implements usecase.SSHConfigManager
var _ usecase.SSHConfigManager = (*SSHConfigManagerImpl)(nil)

// SSHConfigManagerImpl is an infrastructure implementation of usecase.SSHConfigManager.
type SSHConfigManagerImpl struct {
	configRepo usecase.ConfigRepository
	profileMan usecase.ProfileManager
}

// NewSSHConfigManager creates a new SSHConfigManagerImpl.
func NewSSHConfigManager(
	configRepo usecase.ConfigRepository,
	profileMan usecase.ProfileManager,
) *SSHConfigManagerImpl {
	return &SSHConfigManagerImpl{
		configRepo: configRepo,
		profileMan: profileMan,
	}
}

func (s *SSHConfigManagerImpl) Generate() error {
	profile := s.profileMan.GetCurrentProfile(context.Background())
	resp, err := limautil.GenerateSSHConfig(profile.ID, s.configRepo.GetSSHConfigFile(), s.configRepo.GetProfileLimaInstanceDir(profile))
	if err != nil {
		return fmt.Errorf("error generating ssh config: %w", err)
	}

	if err := os.WriteFile(s.configRepo.GetSSHConfigFile(), []byte(resp.Output), 0644); err != nil {
		return fmt.Errorf("error writing ssh_config file: %w", err)
	}

	return nil
}

// Show returns the SSH connection configuration for the current profile.
func (s *SSHConfigManagerImpl) Show(ctx context.Context) (string, error) {
	profile := s.profileMan.GetCurrentProfile(ctx)
	resp, err := limautil.GenerateSSHConfig(profile.ID, s.configRepo.GetSSHConfigFile(), s.configRepo.GetProfileLimaInstanceDir(profile))
	if err != nil {
		return "", fmt.Errorf("error retrieving ssh config: %w", err)
	}
	return resp.Output, nil
}
