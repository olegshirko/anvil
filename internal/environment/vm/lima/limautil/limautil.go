package limautil

import (
	"fmt"
	"os"
	"os/exec"

	"anvil/internal/cli"
	"anvil/internal/domain"

	"gopkg.in/yaml.v3"
)

// EnvLimaHome is the environment variable for the Lima directory.
const EnvLimaHome = "LIMA_HOME"

// EnvLimaDrivers is the environment variable for the path to external Lima drivers.
const EnvLimaDrivers = "LIMA_DRIVERS_PATH"

// LimactlCommand is the limactl command.
const LimactlCommand = "limactl"

var limaHome string

// SetLimaHome sets the Lima home directory for limactl commands.
func SetLimaHome(home string) {
	limaHome = home
}

// Limactl prepares a limactl command.
func Limactl(args ...string) *exec.Cmd {
	cmd := cli.Exec(LimactlCommand, args...).Cmd()
	cmd.Env = append(cmd.Env, os.Environ()...)
	cmd.Env = append(cmd.Env, EnvLimaHome+"="+limaHome)
	return cmd
}

// LoadInstance loads a Lima instance configuration (which is actually a domain.Config).
func LoadInstance(profile *domain.Profile, stateFile string) (domain.Config, error) {
	var c domain.Config
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return c, fmt.Errorf("error reading profile state file: %w", err)
	}

	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("error unmarshalling profile state file: %w", err)
	}

	return c, nil
}
