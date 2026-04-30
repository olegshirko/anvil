package infra

import (
	"anvil/internal/usecase"
	"anvil/internal/util"
)

// ensure SystemHomeDirProvider implements usecase.HomeDirProvider
var _ usecase.HomeDirProvider = (*SystemHomeDirProvider)(nil)

// SystemHomeDirProvider is an infrastructure implementation of usecase.HomeDirProvider that uses the util package.
type SystemHomeDirProvider struct{}

// GetHomeDir returns the user's home directory using UserHome().
func (p *SystemHomeDirProvider) GetHomeDir() (string, error) {
	return util.UserHome()
}

// ensure SystemOSInfoProvider implements usecase.OSInfoProvider
var _ usecase.OSInfoProvider = (*SystemOSInfoProvider)(nil)

// SystemOSInfoProvider is an infrastructure implementation of usecase.OSInfoProvider that uses the util package.
type SystemOSInfoProvider struct{}

// IsMacOS13OrNewer returns true if the current OS is macOS 13 or newer.
func (p *SystemOSInfoProvider) IsMacOS13OrNewer() bool {
	return util.AtLeastVentura()
}

// IsMacOS13OrNewerOnArm returns true if the current OS is macOS 13 or newer and on an ARM processor.
func (p *SystemOSInfoProvider) IsMacOS13OrNewerOnArm() bool {
	return util.AppleSiliconAndModernOS()
}
