package service

import (
	"context"
	"fmt"
	"path/filepath"

	"anvil/internal/cli"
	"anvil/internal/domain"
	"anvil/internal/environment"
	"anvil/internal/service/process"
	"anvil/internal/service/process/inotify"
	"anvil/internal/service/process/vmnet"
	"anvil/internal/util"
	"anvil/internal/util/fsutil"
	"anvil/internal/util/osutil"
)

// Supervisor orchestrates background host processes (vmnet, inotify).
type Supervisor interface {
	Launch(context.Context, domain.Config) error
	Shutdown(context.Context, domain.Config) error
	CheckHealth(context.Context, domain.Config) (HealthReport, error)
	FindDeps(ctx context.Context, cfg domain.Config, name string) (deps process.Dependency, root bool)
}

// HealthReport describes the state of the supervisor and its subprocesses.
type HealthReport struct {
	Active    bool
	Processes []subProcessState
}

type subProcessState struct {
	Name    string
	Running bool
	Error   error
}

// mountProvider resolves host mount points from configuration.
type mountProvider interface {
	ConfigMountsOrDefault(c domain.Config) ([]domain.Mount, error)
}

// ContextKey returns a unique context key for the given service name.
func ContextKey(s string) any { return struct{ svc string }{svc: s} }

// NewSupervisor creates a host-side process supervisor.
func NewSupervisor(host environment.HostActions, mounts mountProvider, profileShortName string, profileConfigDir string) Supervisor {
	process.SetDir(filepath.Join(profileConfigDir, "daemon"))
	return &hostSupervisor{
		host:             host,
		mounts:           mounts,
		profileShortName: profileShortName,
	}
}

var _ Supervisor = (*hostSupervisor)(nil)

type hostSupervisor struct {
	host             environment.HostActions
	mounts           mountProvider
	profileShortName string
}

// FindDeps returns the dependencies for a named subprocess.
func (s hostSupervisor) FindDeps(ctx context.Context, cfg domain.Config, name string) (process.Dependency, bool) {
	for _, p := range s.buildProcs(cfg) {
		if p.Name() == name {
			return process.Dependencies(p)
		}
	}
	return process.Dependencies()
}

// prepareWorkDir ensures the runtime directory exists.
func (s hostSupervisor) prepareWorkDir() error {
	if err := fsutil.EnsureDir(process.Dir()); err != nil {
		return fmt.Errorf("cannot create service workdir: %w", err)
	}
	return nil
}

// CheckHealth queries the supervisor and each subprocess for liveness.
func (s hostSupervisor) CheckHealth(ctx context.Context, cfg domain.Config) (HealthReport, error) {
	var r HealthReport
	if err := s.host.RunQuiet(osutil.SelfPath(), "daemon", "status", s.profileShortName); err != nil {
		return r, nil
	}
	r.Active = true
	ctx = context.WithValue(ctx, process.CtxKeyDaemon(), r.Active)

	for _, p := range s.buildProcs(cfg) {
		alive := p.Alive(ctx)
		r.Processes = append(r.Processes, subProcessState{
			Name:    p.Name(),
			Running: alive == nil,
			Error:   alive,
		})
	}
	return r, nil
}

// Launch starts the background service runner.
func (s hostSupervisor) Launch(ctx context.Context, cfg domain.Config) error {
	_ = s.Shutdown(ctx, cfg) // idempotent

	if err := s.prepareWorkDir(); err != nil {
		return fmt.Errorf("service setup failed: %w", err)
	}

	args := s.buildDaemonArgs(cfg)
	home, err := util.UserHome()
	if err != nil {
		return err
	}
	return s.host.WithDir(home).RunQuiet(args...)
}

// Shutdown stops the background service runner.
func (s hostSupervisor) Shutdown(ctx context.Context, cfg domain.Config) error {
	if st, err := s.CheckHealth(ctx, cfg); err != nil || !st.Active {
		return nil
	}
	return s.host.RunQuiet(osutil.SelfPath(), "daemon", "stop", s.profileShortName)
}

// buildDaemonArgs assembles the CLI arguments for the service binary.
func (s hostSupervisor) buildDaemonArgs(cfg domain.Config) []string {
	args := []string{osutil.SelfPath(), "daemon", "start", s.profileShortName}

	if cfg.Network.Address {
		args = append(args, "--vmnet", "--vmnet-mode", cfg.Network.Mode, "--vmnet-interface", cfg.Network.BridgeInterface)
	}
	if cfg.MountINotify {
		args = append(args, "--inotify", "--inotify-runtime", cfg.Runtime)
		mnts, err := s.mounts.ConfigMountsOrDefault(cfg)
		if err == nil {
			for _, m := range mnts {
				p, err := util.ResolveMountPath(m.Location)
				if err == nil {
					args = append(args, "--inotify-dir", p)
				}
			}
		}
	}
	if cli.Settings.Verbose {
		args = append(args, "--very-verbose")
	}
	return args
}

// buildProcs constructs the list of active subprocesses from configuration.
func (s hostSupervisor) buildProcs(cfg domain.Config) []process.Process {
	var procs []process.Process
	if cfg.Network.Address {
		procs = append(procs, vmnet.NewService(cfg.Network.Mode, cfg.Network.BridgeInterface, s.profileShortName))
	}
	if cfg.MountINotify {
		procs = append(procs, inotify.New())
	}
	return procs
}
