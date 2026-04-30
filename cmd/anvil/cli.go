package main

import (
	"anvil/cmd/root"
	"anvil/internal/usecase"

	"github.com/sirupsen/logrus"
)

// Globals are flags available on all commands.
type Globals struct {
	Profile     string              `short:"p" env:"ANVIL_PROFILE" default:"default" help:"Profile name, for multiple instances"`
	Verbose     bool                `short:"v" help:"Enable verbose log"`
	VeryVerbose bool                `long:"very-verbose" help:"Enable more verbose log"`
	app         usecase.Application `kong:"-"`
}

// BeforeApply initializes profile and logging before any command runs.
func (g *Globals) BeforeApply() error {
	if g.Verbose {
		logrus.SetLevel(logrus.DebugLevel)
	}
	if g.VeryVerbose {
		logrus.SetLevel(logrus.TraceLevel)
	}
	return nil
}

// resolveProfile updates the global profile from positional argument if present.
func (g *Globals) resolveProfile(arg string) {
	if g.app == nil {
		g.app = root.NewApp()
	}
	if arg != "" && g.Profile == "default" {
		g.app.ConfigService().SetProfile(arg)
	} else if g.Profile != "" {
		g.app.ConfigService().SetProfile(g.Profile)
	}
}

// CLI is the root command structure.
type CLI struct {
	Globals

	Version    versionCmd    `cmd:"" help:"Print the version of anvil"`
	List       listCmd       `cmd:"" aliases:"ls" help:"List instances"`
	Status     statusCmd     `cmd:"" help:"Show status"`
	SSH        sshCmd        `cmd:"" aliases:"exec,x" help:"SSH into the VM"`
	SSHConfig  sshConfigCmd  `cmd:"" help:"Show SSH connection config"`
	Health     healthCmd     `cmd:"" help:"Check health of the active profile"`
	Doctor     doctorCmd     `cmd:"" help:"Diagnose host readiness"`
	Start      startCmd      `cmd:"" help:"Start anvil"`
	Stop       stopCmd       `cmd:"" help:"Stop anvil"`
	Restart    restartCmd    `cmd:"" help:"Restart anvil"`
	Delete     deleteCmd     `cmd:"" help:"Delete and teardown anvil"`
	Prune      pruneCmd      `cmd:"" help:"Prune cached downloaded assets"`
	Template   templateCmd   `cmd:"" aliases:"tmpl,tpl,t" help:"Edit the template for default configurations"`
	Completion completionCmd `cmd:"" help:"Generate completion script"`
	Clone      cloneCmd      `cmd:"" help:"Clone anvil profile"`
	Update     updateCmd     `cmd:"" aliases:"u,up" help:"Update the container runtime"`
	Kubernetes kubernetesCmd `cmd:"" aliases:"kube,k8s,k3s,k" help:"Manage Kubernetes cluster"`
	Nerdctl    nerdctlCmd    `cmd:"" aliases:"nerd,n" help:"Run nerdctl (requires containerd runtime)"`
	Watch      watchCmd      `cmd:"" help:"Watch Lima instance events (Lima 2.1+)"`
	Images     imagesCmd     `cmd:"" help:"Discover and request VM base or container images"`
	Compose    composeCmd    `cmd:"" help:"Run docker compose with docker-mirror fallback for missing images"`
	Daemon     daemonCmd     `cmd:"" hidden:"" help:"Runner for background daemons"`
}
