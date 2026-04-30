package main

import (
	"context"
	"fmt"
	"os"

	"anvil/internal/domain"
)

// versionCmd

type versionCmd struct {
	ProfileArg string `arg:"" optional:"" help:"Profile name"`
}

func (c *versionCmd) Run(g *Globals) error {
	g.resolveProfile(c.ProfileArg)
	version := g.app.ConfigService().AppVersion()
	fmt.Println(domain.AppName, "version", version.Version)
	fmt.Println("git commit:", version.Revision)
	return g.app.Version(context.Background())
}

// statusCmd

type statusCmd struct {
	ProfileArg string `arg:"" optional:"" help:"Profile name"`
	Extended   bool   `short:"e" help:"Include additional details"`
	JSON       bool   `short:"j" help:"Print JSON output"`
}

func (c *statusCmd) Run(g *Globals) error {
	g.resolveProfile(c.ProfileArg)
	return g.app.Status(context.Background(), c.Extended, c.JSON)
}

// sshCmd

type sshCmd struct {
	ProfileArg string   `arg:"" optional:"" help:"Profile name"`
	Args       []string `arg:"" optional:"" help:"Command to run in VM"`
}

func (c *sshCmd) Run(g *Globals) error {
	g.resolveProfile(c.ProfileArg)
	return g.app.SSH(context.Background(), c.Args...)
}

// sshConfigCmd

type sshConfigCmd struct {
	ProfileArg string `arg:"" optional:"" help:"Profile name"`
}

func (c *sshConfigCmd) Run(g *Globals) error {
	g.resolveProfile(c.ProfileArg)
	resp, err := g.app.SSHConfig(context.Background())
	if err == nil {
		fmt.Println(resp)
	}
	return err
}

// healthCmd

type healthCmd struct {
	ProfileArg string `arg:"" optional:"" help:"Profile name"`
	JSON       bool   `short:"j" help:"Print JSON output"`
}

func (c *healthCmd) Run(g *Globals) error {
	g.resolveProfile(c.ProfileArg)
	return g.app.Health(context.Background(), c.JSON)
}

// doctorCmd

type doctorCmd struct {
	JSON bool `short:"j" help:"Print JSON output"`
}

func (c *doctorCmd) Run(g *Globals) error {
	return g.app.Doctor(context.Background(), c.JSON)
}

// updateCmd

type updateCmd struct {
	ProfileArg string `arg:"" optional:"" help:"Profile name"`
}

func (c *updateCmd) Run(g *Globals) error {
	g.resolveProfile(c.ProfileArg)
	return g.app.Update(context.Background())
}

// watchCmd

type watchCmd struct {
	ProfileArg string   `arg:"" optional:"" help:"Profile name"`
	History    bool     `help:"Include historical events from before watch started"`
	JSON       bool     `short:"j" help:"Output events as newline-delimited JSON"`
	Args       []string `arg:"" optional:"" help:"Additional arguments to pass to limactl watch"`
}

func (c *watchCmd) Run(g *Globals) error {
	g.resolveProfile(c.ProfileArg)
	var args []string
	if c.History {
		args = append(args, "--history")
	}
	if c.JSON {
		args = append(args, "--json")
	}
	args = append(args, c.Args...)
	return g.app.Watch(context.Background(), args)
}

// completionCmd

type completionCmd struct {
	Shell string `arg:"" enum:"bash,zsh,fish,powershell" help:"Shell type"`
}

func (c *completionCmd) Run() error {
	// Kong doesn't have built-in completion generation like Cobra.
	// For now, we output a placeholder message.
	fmt.Fprintf(os.Stderr, "completion for %s is not yet implemented in Kong CLI\n", c.Shell)
	return nil
}
