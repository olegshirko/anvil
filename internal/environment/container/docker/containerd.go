package docker

import (
	"context"
	_ "embed"
	"fmt"
)

const (
	containerdConfigPath = "/etc/containerd/config.toml"
	containerdBackupPath = "/etc/containerd/config.anvil.bak.toml"
)

//go:embed config.toml
var embeddedContainerdConfig []byte

// ensureContainerdConfig installs the embedded containerd configuration inside
// the VM, backing up any existing file first.
func (d dockerEngine) ensureContainerdConfig(ctx context.Context) error {
	pipe := d.Init(ctx)
	pipe.Add(d.applyContainerdConfig)
	return pipe.Exec()
}

func (d dockerEngine) applyContainerdConfig() error {
	// nothing to do if we already created a backup
	if _, err := d.guest.Stat(containerdBackupPath); err == nil {
		return nil
	}

	// preserve existing config
	if _, err := d.guest.Stat(containerdConfigPath); err == nil {
		if err := d.guest.Run("sudo", "cp", containerdConfigPath, containerdBackupPath); err != nil {
			return fmt.Errorf("cannot backup containerd config: %w", err)
		}
	}

	if err := d.guest.Write(containerdConfigPath, embeddedContainerdConfig); err != nil {
		return fmt.Errorf("cannot write containerd config: %w", err)
	}

	return d.guest.Run("sudo", "service", "containerd", "restart")
}
