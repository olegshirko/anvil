package docker

import (
	"context"
	"time"

	"anvil/internal/cli"
	"anvil/internal/domain"
	"anvil/internal/environment"
	"anvil/internal/util/debutil"
)

const Name = "docker"

func init() {
	environment.RegisterRuntime(Name, New, false)
}

var _ environment.ContainerRuntime = (*dockerEngine)(nil)

type dockerEngine struct {
	host             environment.HostActions
	guest            environment.GuestActions
	profileConfigDir string
	profileID        string
	cli.CommandChain
}

// New creates a new docker runtime.
func New(host environment.HostActions, guest environment.GuestActions, profileConfigDir string, profileID string, cacheDir string) environment.ContainerRuntime {
	return &dockerEngine{
		host:             host,
		guest:            guest,
		profileConfigDir: profileConfigDir,
		profileID:        profileID,
		CommandChain:     cli.New(Name),
	}
}

func (e dockerEngine) Name() string { return Name }

func (e dockerEngine) Dependencies() []string {
	return []string{"docker"}
}

func (e dockerEngine) Provision(ctx context.Context, conf domain.Config) error {
	pipe := e.Init(ctx)
	log := e.Logger(ctx)

	pkgs := []string{"docker-ce", "docker-ce-cli", "containerd.io"}
	pipe.Add(func() error {
		_, err := debutil.EnsurePackages(ctx, e.guest, e, "docker", pkgs...)
		return err
	})

	pipe.Add(func() error {
		return e.ensureContainerdConfig(ctx)
	})

	pipe.Add(func() error {
		changed := false
		if c, err := e.writeDaemonConfig(conf.Docker, conf.Env); err != nil {
			log.Warnln(err)
		} else if c {
			changed = true
		}
		if c, err := e.writeSystemdOverride(conf.Docker); err != nil {
			log.Warnln(err)
		} else if c {
			changed = true
		}
		if changed {
			if err := e.restartDockerService(); err != nil {
				log.Warnln(err)
			}
		}
		return nil
	})

	pipe.Add(e.setupContext)
	pipe.Add(e.useContext)

	return pipe.Exec()
}

func (e dockerEngine) Start(ctx context.Context, _ domain.Config) error {
	pipe := e.Init(ctx)

	pipe.Retry("", 200*time.Millisecond, 60, func(int) error {
		return e.guest.RunQuiet("sudo", "systemctl", "start", "docker.service")
	})

	pipe.Retry("", 200*time.Millisecond, 60, func(int) error {
		return e.guest.RunQuiet("sudo", "docker", "info")
	})

	pipe.Add(func() error {
		if err := e.guest.RunQuiet("docker", "info"); err == nil {
			return nil
		}
		ctx := context.WithValue(ctx, cli.CtxKeyQuiet, true)
		return e.guest.Restart(ctx)
	})

	return pipe.Exec()
}

func (e dockerEngine) Running(ctx context.Context) bool {
	return e.guest.RunQuiet("service", "docker", "status") == nil
}

func (e dockerEngine) Stop(ctx context.Context) error {
	pipe := e.Init(ctx)
	pipe.Add(func() error {
		if !e.Running(ctx) {
			return nil
		}
		return e.guest.Run("sudo", "systemctl", "stop", "docker.service")
	})
	pipe.Add(e.teardownContext)
	return pipe.Exec()
}

func (e dockerEngine) Teardown(ctx context.Context) error {
	pipe := e.Init(ctx)
	pipe.Add(e.teardownContext)
	return pipe.Exec()
}

func (e dockerEngine) Host() environment.HostActions   { return e.host }
func (e dockerEngine) Guest() environment.GuestActions { return e.guest }

func (e dockerEngine) Version(ctx context.Context, profile string) string {
	v, _ := e.host.RunOutput("docker", "--context", profile, "version", "--format", `client: v{{.Client.Version}}{{printf "\n"}}server: v{{.Server.Version}}`)
	return v
}

func (e *dockerEngine) Update(ctx context.Context) (bool, error) {
	pkgs := []string{"docker-ce", "docker-ce-cli", "containerd.io"}
	return debutil.EnsurePackages(ctx, e.guest, e, "docker", pkgs...)
}

// DataDisk describes the directories placed on the external data disk.
func DataDisk() environment.DataDisk {
	return environment.DataDisk{
		Dirs:   diskDirs,
		FSType: "ext4",
		PreMount: []string{
			"systemctl stop docker.service",
			"systemctl stop containerd.service",
		},
	}
}

var diskDirs = []environment.DiskDir{
	{Name: "docker", Path: "/var/lib/docker"},
	{Name: "containerd", Path: "/var/lib/containerd"},
	{Name: "rancher", Path: "/var/lib/rancher"},
	{Name: "cni", Path: "/var/lib/cni"},
}
