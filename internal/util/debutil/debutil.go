package debutil

import (
	"context"
	"fmt"
	"strings"

	"anvil/internal/cli"
	"anvil/internal/environment"
)

// debPackages is a slice of Debian package names.
type debPackages []string

// UpgradeCheckCmd returns a shell command that checks if the packages have upgrades available.
func (p debPackages) UpgradeCheckCmd() string {
	cmd := "sudo apt list --upgradable | grep"
	for _, pkg := range p {
		cmd += fmt.Sprintf(" -e '^%s/'", pkg)
	}
	return cmd
}

// InstallCmd returns a shell command that installs the packages via apt.
func (p debPackages) InstallCmd() string {
	return "sudo apt-get install -y --allow-change-held-packages " + strings.Join(p, " ")
}

// VerifyCmd returns a shell command that verifies all packages are installed.
func (p debPackages) VerifyCmd() string {
	return fmt.Sprintf("[ $(dpkg-query -l %s | grep -c '^ii') -eq %d ]", strings.Join(p, " "), len(p))
}

// EnsurePackages installs the requested packages if they are not already present.
func EnsurePackages(
	ctx context.Context,
	guest environment.GuestActions,
	pipeline cli.CommandChain,
	binName string,
	pkgs ...string,
) (bool, error) {
	exec := pipeline.Init(ctx)
	log := exec.Logger()

	// skip when the primary binary is already present
	if binName != "" {
		if err := guest.RunQuiet("command", "-v", binName); err == nil {
			log.Traceln("binary exists, skipping package install:", binName)
			return false, nil
		}
	}

	packages := debPackages(pkgs)

	// skip when every package is already installed
	if err := guest.RunQuiet("sh", "-c", packages.VerifyCmd()); err == nil {
		log.Traceln("packages already installed:", strings.Join(pkgs, ", "))
		return false, nil
	}

	var upgradable, changed bool

	exec.Stage("refreshing package manager")
	exec.Add(func() error {
		return guest.RunQuiet("sh", "-c", "sudo apt-get update -qq -y")
	})

	exec.Stage("checking for updates")
	exec.Add(func() error {
		upgradable = guest.RunQuiet("sh", "-c", packages.UpgradeCheckCmd()) == nil
		return nil
	})

	exec.Add(func() error {
		if !upgradable {
			log.Warnln("no package updates available")
		} else {
			log.Println("updating packages ...")
		}

		if err := guest.RunQuiet("sh", "-c", packages.InstallCmd()); err != nil {
			return err
		}
		changed = true
		if upgradable {
			log.Println("done")
		} else {
			log.Println("packages installed")
		}
		return nil
	})

	return changed, exec.Exec()
}
