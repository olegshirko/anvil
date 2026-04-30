package lima

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
)

const vmSettingsPath = "/etc/anvil/anvil.json"

// readSettings reads the per-VM key-value settings file inside the guest.
func (l limaVM) readSettings() map[string]string {
	m := map[string]string{}

	b, err := l.Read(vmSettingsPath)
	if err != nil {
		l.Logger(context.Background()).Tracef("cannot read VM settings: %v", err)
		return m
	}

	if err := json.Unmarshal([]byte(b), &m); err != nil {
		l.Logger(context.Background()).Tracef("cannot parse VM settings: %v", err)
	}

	return m
}

// Setting reads a single key from the VM settings.
func (l limaVM) Setting(key string) string {
	return l.readSettings()[key]
}

// SetSetting writes a single key-value pair to the VM settings.
func (l limaVM) SetSetting(key, value string) error {
	m := l.readSettings()
	m[key] = value

	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("cannot marshal VM settings: %w", err)
	}

	if err := l.Run("sudo", "mkdir", "-p", filepath.Dir(vmSettingsPath)); err != nil {
		return fmt.Errorf("cannot create settings directory: %w", err)
	}

	if err := l.Write(vmSettingsPath, b); err != nil {
		return fmt.Errorf("cannot write VM settings: %w", err)
	}

	return nil
}
