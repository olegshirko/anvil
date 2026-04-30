package docker

import (
	"fmt"
	"path/filepath"
	"strings"

	"anvil/internal/util"
)

// HostSocketFile returns the path to the docker socket on host.
func HostSocketFile(profileConfigDir string) string {
	return filepath.Join(profileConfigDir, "docker.sock")
}
func LegacyDefaultHostSocketFile(profileConfigDir string) string {
	return filepath.Join(filepath.Dir(profileConfigDir), "docker.sock")
}

func (d dockerEngine) contextCreated() bool {
	if d.host == nil {
		return false
	}
	return d.host.RunQuiet("docker", "context", "inspect", d.profileID) == nil
}

func (d dockerEngine) setupContext() error {
	if d.host == nil {
		return fmt.Errorf("docker host actions are not initialized")
	}
	if d.contextCreated() {
		return nil
	}

	return d.host.Run("docker", "context", "create", d.profileID,
		"--description", d.profileID,
		"--docker", "host=unix://"+HostSocketFile(d.profileConfigDir),
	)
}

func (d dockerEngine) useContext() error {
	if d.host == nil {
		return fmt.Errorf("docker host actions are not initialized")
	}
	if err := d.host.Run("docker", "context", "use", d.profileID); err != nil {
		return err
	}

	// Create a symlink from ~/.anvil/docker.sock to the actual socket file.
	// This helps in cases where DOCKER_HOST might be set to this path or user expects it.
	homeDir, err := util.UserHome()
	if err != nil {
		return fmt.Errorf("error getting home directory: %w", err)
	}
	anvilSocketDir := filepath.Join(homeDir, ".anvil")
	if err := d.host.RunQuiet("mkdir", "-p", anvilSocketDir); err != nil {
		return fmt.Errorf("error creating ~/.anvil directory: %w", err)
	}

	targetSocket := HostSocketFile(d.profileConfigDir)
	symlinkPath := filepath.Join(anvilSocketDir, "docker.sock")

	// Remove existing symlink if it points to a different target or is a regular file
	if _, err := d.host.Stat(symlinkPath); err == nil {
		linkTarget, err := d.host.RunOutput("readlink", symlinkPath)
		if err == nil && strings.TrimSpace(linkTarget) == targetSocket {
			// Symlink already exists and points to the correct target
			return nil
		}
		// Remove existing symlink/file before creating a new one
		if err := d.host.RunQuiet("rm", "-f", symlinkPath); err != nil {
			return fmt.Errorf("error removing old docker socket symlink: %w", err)
		}
	}

	if err := d.host.RunQuiet("ln", "-s", targetSocket, symlinkPath); err != nil {
		return fmt.Errorf("error creating docker socket symlink: %w", err)
	}

	return nil
}

func (d dockerEngine) teardownContext() error {
	if d.host == nil {
		return fmt.Errorf("docker host actions are not initialized")
	}
	if !d.contextCreated() {
		return nil
	}

	if err := d.host.Run("docker", "context", "rm", "--force", d.profileID); err != nil {
		return err
	}

	homeDir, err := util.UserHome()
	if err == nil {
		symlinkPath := filepath.Join(homeDir, ".anvil", "docker.sock")
		_ = d.host.RunQuiet("rm", "-f", symlinkPath)
	}

	return nil
}
