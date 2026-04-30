package usecase

import (
	"fmt"

	"anvil/internal/domain"
)

// ConfigHelper provides helper functions for configuration that depend on external system information.
type ConfigHelper struct {
	homeDirProvider HomeDirProvider
	osInfoProvider  OSInfoProvider
}

// NewConfigHelper creates a new ConfigHelper.
func NewConfigHelper(homeDirProvider HomeDirProvider, osInfoProvider OSInfoProvider) *ConfigHelper {
	return &ConfigHelper{
		homeDirProvider: homeDirProvider,
		osInfoProvider:  osInfoProvider,
	}
}

// ConfigMountsOrDefault returns the configured mounts or a default set.
func (h *ConfigHelper) ConfigMountsOrDefault(c domain.Config) ([]domain.Mount, error) {
	if len(c.Mounts) > 0 {
		return c.Mounts, nil
	}

	home, err := h.homeDirProvider.GetHomeDir()
	if err != nil {
		return nil, err
	}

	return []domain.Mount{
		{Location: home, Writable: true},
	}, nil
}

// ConfigAutoActivate returns if auto-activation of host client config is enabled.
func ConfigAutoActivate(c domain.Config) bool {
	if c.ActivateRuntime == nil {
		return true
	}
	return *c.ActivateRuntime
}

// ConfigEmpty checks if the configuration is empty.
func ConfigEmpty(c domain.Config) bool { return c.Runtime == "" }

// ConfigDriverLabel returns the driver label for the VM type.
func (h *ConfigHelper) ConfigDriverLabel(c domain.Config) string {
	if h.osInfoProvider.IsMacOS13OrNewer() && c.VMType == "vz" {
		return "macOS Virtualization.Framework"
	} else if h.osInfoProvider.IsMacOS13OrNewerOnArm() && c.VMType == "krunkit" {
		return "Krunkit"
	}
	return "QEMU"
}

// DiskGiB returns the string represent of the disk in GiB.
func DiskGiB(d domain.Disk) string { return fmt.Sprintf("%dGiB", d) }

// DiskInt returns the disk size in bytes.
func DiskInt(d domain.Disk) int64 { return 1024 * 1024 * 1024 * int64(d) }
