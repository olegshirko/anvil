package usecase

import (
	"context"
	"fmt"
	"net"
	"strings"

	"anvil/internal/domain"
	"anvil/internal/util"
)

// ConfigService replaces the global config package facade.
type ConfigService interface {
	LoadConfig(ctx context.Context) (domain.Config, error)
	SaveConfig(ctx context.Context, cfg domain.Config) error
	LoadFromFile(path string) (domain.Config, error)
	ValidateConfig(cfg domain.Config) error
	Profile() *domain.Profile
	SetProfile(name string)
	ProfileFromName(name string) *domain.Profile
	CacheDir() string
	TemplatesDir() string
	ProfileLimaInstanceDir(profile *domain.Profile) string
	ProfileConfigDir(profile *domain.Profile) string
	ProfileStateFile(profile *domain.Profile) string
	ProfileFile(profile *domain.Profile) string
	ProfileStoreFile(profile *domain.Profile) string
	SSHConfigFile() string
	SaveTemplate(cfg domain.Config, filepath string) error
	AppVersion() domain.VersionInfo
	LimaDir() string
	ProfileLimaFile(profile *domain.Profile) string
}

// ConfigServiceImpl implements ConfigService using repository and manager.
type ConfigServiceImpl struct {
	repo    ConfigRepository
	profile ProfileManager
}

func NewConfigService(repo ConfigRepository, profile ProfileManager) *ConfigServiceImpl {
	return &ConfigServiceImpl{repo: repo, profile: profile}
}

func (s *ConfigServiceImpl) LoadConfig(ctx context.Context) (domain.Config, error) {
	return s.repo.Load(ctx, s.profile.GetCurrentProfile(ctx))
}

func (s *ConfigServiceImpl) SaveConfig(ctx context.Context, cfg domain.Config) error {
	return s.repo.Save(ctx, s.profile.GetCurrentProfile(ctx), cfg)
}

func (s *ConfigServiceImpl) LoadFromFile(path string) (domain.Config, error) {
	return s.repo.ConfigDataStore().LoadFromPath(context.Background(), path)
}

func (s *ConfigServiceImpl) ValidateConfig(c domain.Config) error {
	validMountTypes := map[string]bool{"9p": true, "sshfs": true}
	validPortForwarders := map[string]bool{"grpc": true, "ssh": true}

	if util.AtLeastVentura() {
		validMountTypes["virtiofs"] = true
	}
	if _, ok := validMountTypes[c.MountType]; !ok {
		return fmt.Errorf("invalid mountType: '%s'", c.MountType)
	}
	validVMTypes := map[string]bool{"qemu": true}
	if util.AtLeastVentura() {
		validVMTypes["vz"] = true
	}
	if util.AppleSiliconAndModernOS() {
		validVMTypes["krunkit"] = true
	}
	if _, ok := validVMTypes[c.VMType]; !ok {
		return fmt.Errorf("invalid vmType: '%s'", c.VMType)
	}
	if c.VMType == "qemu" {
		if err := util.RequireQemuImg(); err != nil {
			return fmt.Errorf("cannot use vmType: '%s', error: %w", c.VMType, err)
		}
	}

	if c.DiskImage != "" {
		if strings.HasPrefix(c.DiskImage, "http://") || strings.HasPrefix(c.DiskImage, "https://") {
			return fmt.Errorf("cannot use diskImage: remote URLs not supported, only local files can be specified")
		}
	}

	if _, ok := validPortForwarders[c.PortForwarder]; !ok {
		return fmt.Errorf("invalid port forwarder: '%s'", c.PortForwarder)
	}

	validRuntimes := map[string]bool{"docker": true, "containerd": true, "incus": true, "none": true}
	if _, ok := validRuntimes[c.Runtime]; !ok {
		return fmt.Errorf("invalid runtime: '%s', valid options are: docker, containerd, incus, none", c.Runtime)
	}

	if c.Network.GatewayAddress != nil {
		if err := validateGatewayAddress(c.Network.GatewayAddress); err != nil {
			return err
		}
	}

	return nil
}

func validateGatewayAddress(gateway net.IP) error {
	ip4 := gateway.To4()
	if ip4 == nil {
		return fmt.Errorf("gateway %q is not IPv4", gateway)
	}
	if ip4[3] != 2 {
		return fmt.Errorf("the last octet of gateway %q is not 2", gateway)
	}
	return nil
}

func (s *ConfigServiceImpl) Profile() *domain.Profile {
	return s.profile.GetCurrentProfile(context.Background())
}

func (s *ConfigServiceImpl) SetProfile(name string) {
	s.profile.SetProfile(context.Background(), name)
}

func (s *ConfigServiceImpl) ProfileFromName(name string) *domain.Profile {
	return s.profile.GetProfileFromName(context.Background(), name)
}

func (s *ConfigServiceImpl) CacheDir() string     { return s.repo.GetCacheDir() }
func (s *ConfigServiceImpl) TemplatesDir() string { return s.repo.GetTemplatesDir() }
func (s *ConfigServiceImpl) ProfileLimaInstanceDir(profile *domain.Profile) string {
	return s.repo.GetProfileLimaInstanceDir(profile)
}
func (s *ConfigServiceImpl) ProfileConfigDir(profile *domain.Profile) string {
	return s.repo.ProfileConfigDir(profile)
}
func (s *ConfigServiceImpl) ProfileStateFile(profile *domain.Profile) string {
	return s.repo.GetProfileStatePath(profile)
}
func (s *ConfigServiceImpl) ProfileFile(profile *domain.Profile) string {
	return s.repo.GetProfileConfigPath(profile)
}
func (s *ConfigServiceImpl) ProfileStoreFile(profile *domain.Profile) string {
	return s.repo.GetProfileStoreFile(profile)
}
func (s *ConfigServiceImpl) SSHConfigFile() string { return s.repo.GetSSHConfigFile() }
func (s *ConfigServiceImpl) LimaDir() string       { return s.repo.GetLimaDir() }
func (s *ConfigServiceImpl) ProfileLimaFile(profile *domain.Profile) string {
	return s.repo.GetProfileLimaFile(profile)
}

func (s *ConfigServiceImpl) SaveTemplate(cfg domain.Config, filepath string) error {
	return s.repo.ConfigDataStore().SaveToPath(context.Background(), cfg, filepath)
}

func (s *ConfigServiceImpl) AppVersion() domain.VersionInfo {
	v := GetAppVersion()
	return domain.VersionInfo{Version: v.Version, Revision: v.Revision}
}
