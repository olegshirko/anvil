package main

import (
	"context"
	"path/filepath"
	"time"

	"anvil/cmd/root"
	"anvil/cmd/service"
	"anvil/internal/environment/host"
	"anvil/internal/environment/vm/lima"
	"anvil/internal/service/process"
	"anvil/internal/service/process/inotify"
	"anvil/internal/service/process/vmnet"
)

// daemonCmd

type daemonCmd struct {
	Start  daemonStartCmd  `cmd:"" help:"Start daemon"`
	Stop   daemonStopCmd   `cmd:"" help:"Stop daemon"`
	Status daemonStatusCmd `cmd:"" help:"Status of the daemon"`
}

type daemonStartCmd struct {
	Profile        string   `arg:"" help:"Profile name"`
	VMNet          bool     `name:"vmnet" help:"Start vmnet"`
	VMNetMode      string   `name:"vmnet-mode" default:"shared" help:"Vmnet mode (shared, bridged)"`
	VMNetIf        string   `name:"vmnet-if" default:"en0" help:"Vmnet interface for bridged mode"`
	INotify        bool     `name:"inotify" help:"Start inotify"`
	INotifyDirs    []string `name:"inotify-dirs" help:"Set inotify directories"`
	INotifyRuntime string   `name:"inotify-runtime" default:"docker" help:"Set runtime"`
}

func (c *daemonStartCmd) Run(g *Globals) error {
	g.app.ConfigService().SetProfile(c.Profile)
	workDir := filepath.Join(g.app.ConfigService().ProfileConfigDir(g.app.ConfigService().Profile()), "daemon")
	process.SetDir(workDir)
	ctx := context.Background()

	var processes []process.Process
	if c.VMNet {
		processes = append(processes, vmnet.NewService(c.VMNetMode, c.VMNetIf, g.app.ConfigService().Profile().ShortName))
	}
	if c.INotify {
		processes = append(processes, inotify.New())
		guest := lima.New(host.New(), g.app.ConfigService(), root.GetConfigRepository(), root.GetConfigHelper())
		args := inotify.Args{
			GuestActions: guest,
			Runtime:      c.INotifyRuntime,
			Dirs:         c.INotifyDirs,
			ProfileID:    g.app.ConfigService().Profile().ID,
		}
		ctx = context.WithValue(ctx, inotify.ArgsContextKey(), args)
	}

	supervisor := service.NewSupervisor(workDir)
	return supervisor.Start(ctx, processes)
}

type daemonStopCmd struct {
	Profile string `arg:"" help:"Profile name"`
}

func (c *daemonStopCmd) Run(g *Globals) error {
	g.app.ConfigService().SetProfile(c.Profile)
	workDir := filepath.Join(g.app.ConfigService().ProfileConfigDir(g.app.ConfigService().Profile()), "daemon")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*60)
	defer cancel()
	supervisor := service.NewSupervisor(workDir)
	return supervisor.Stop(ctx)
}

type daemonStatusCmd struct {
	Profile string `arg:"" help:"Profile name"`
}

func (c *daemonStatusCmd) Run(g *Globals) error {
	g.app.ConfigService().SetProfile(c.Profile)
	workDir := filepath.Join(g.app.ConfigService().ProfileConfigDir(g.app.ConfigService().Profile()), "daemon")
	supervisor := service.NewSupervisor(workDir)
	if !supervisor.Alive() {
		return nil
	}
	return nil
}
