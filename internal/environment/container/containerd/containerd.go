package containerd

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"time"

	"anvil/internal/cli"
	"anvil/internal/domain"
	"anvil/internal/environment"
)

const Name = "containerd"

// HostSocketFiles returns the Unix socket paths exposed on the host.
func HostSocketFiles(profileConfigDir string) (files struct {
	Containerd string
	Buildkitd  string
}) {
	files.Containerd = filepath.Join(profileConfigDir, "containerd.sock")
	files.Buildkitd = filepath.Join(profileConfigDir, "buildkitd.sock")
	return files
}

//go:embed config.toml
var containerdConf []byte

//go:embed buildkitd.toml
var buildkitConf []byte

const containerdConfPath = "/etc/containerd/config.toml"
const buildkitConfPath = "/etc/buildkit/buildkitd.toml"

func createContainerdEngine(host environment.HostActions, guest environment.GuestActions, profileConfigDir string, profileID string, cacheDir string) environment.ContainerRuntime {
	return &containerdEngine{
		host:             host,
		guest:            guest,
		profileConfigDir: profileConfigDir,
		CommandChain:     cli.New(Name),
	}
}

func init() {
	environment.RegisterRuntime(Name, createContainerdEngine, false)
}

var _ environment.ContainerRuntime = (*containerdEngine)(nil)

type containerdEngine struct {
	host             environment.HostActions
	guest            environment.GuestActions
	profileConfigDir string
	cli.CommandChain
}

func (e containerdEngine) Name() string { return Name }

func (e containerdEngine) Dependencies() []string { return nil }

func (e containerdEngine) Provision(ctx context.Context, _ domain.Config) error {
	pipe := e.Init(ctx)
	pipe.Add(func() error {
		return e.guest.Write(containerdConfPath, containerdConf)
	})
	pipe.Add(func() error {
		return e.guest.Write(buildkitConfPath, buildkitConf)
	})
	return pipe.Exec()
}

func (e containerdEngine) Start(ctx context.Context, _ domain.Config) error {
	pipe := e.Init(ctx)
	pipe.Add(func() error {
		return e.guest.Run("sudo", "service", "containerd", "restart")
	})
	pipe.Retry("", time.Second, 10, func(int) error {
		return e.guest.RunQuiet("sudo", "nerdctl", "info")
	})
	pipe.Add(func() error {
		return e.guest.Run("sudo", "service", "buildkit", "start")
	})
	return pipe.Exec()
}

func (e containerdEngine) Running(ctx context.Context) bool {
	return e.guest.RunQuiet("service", "containerd", "status") == nil
}

func (e containerdEngine) Stop(ctx context.Context) error {
	pipe := e.Init(ctx)
	pipe.Add(func() error {
		return e.guest.Run("sudo", "service", "containerd", "stop")
	})
	return pipe.Exec()
}

func (e containerdEngine) Teardown(context.Context) error {
	return nil
}

func (e containerdEngine) Host() environment.HostActions   { return e.host }
func (e containerdEngine) Guest() environment.GuestActions { return e.guest }

func (e containerdEngine) Version(ctx context.Context, _ string) string {
	v, _ := e.guest.RunOutput("sudo", "nerdctl", "version", "--format", `client: {{.Client.Version}}{{printf "\n"}}server: {{(index .Server.Components 0).Version}}`)
	return v
}

func (e *containerdEngine) Update(ctx context.Context) (bool, error) {
	return false, fmt.Errorf("in-place upgrade is not supported for %s", Name)
}

// DataDisk describes the directories placed on the external data disk.
func DataDisk() environment.DataDisk {
	return environment.DataDisk{
		Dirs:   diskDirs,
		FSType: "ext4",
		PreMount: []string{
			"systemctl stop containerd.service",
			"systemctl stop buildkit.service",
		},
	}
}

var diskDirs = []environment.DiskDir{
	{Name: "containerd", Path: "/var/lib/containerd"},
	{Name: "buildkit", Path: "/var/lib/buildkit"},
	{Name: "nerdctl", Path: "/var/lib/nerdctl"},
	{Name: "rancher", Path: "/var/lib/rancher"},
	{Name: "cni", Path: "/var/lib/cni"},
}
