package main

import (
	"context"
)

// stopCmd

type stopCmd struct {
	ProfileArg string `arg:"" optional:"" help:"Profile name"`
	Force      bool   `short:"f" help:"Stop without graceful shutdown"`
}

func (c *stopCmd) Run(g *Globals) error {
	g.resolveProfile(c.ProfileArg)
	return g.app.Stop(context.Background(), c.Force)
}

// deleteCmd

type deleteCmd struct {
	ProfileArg string `arg:"" optional:"" help:"Profile name"`
	Force      bool   `short:"f" help:"Do not prompt for yes/no"`
	Data       bool   `short:"d" help:"Delete container runtime data"`
}

func (c *deleteCmd) Run(g *Globals) error {
	g.resolveProfile(c.ProfileArg)
	return g.app.Delete(context.Background(), c.Data, c.Force)
}

// restartCmd

type restartCmd struct {
	ProfileArg string `arg:"" optional:"" help:"Profile name"`
	Force      bool   `short:"f" help:"During restart, do stop without graceful shutdown"`
}

func (c *restartCmd) Run(g *Globals) error {
	g.resolveProfile(c.ProfileArg)
	return g.app.Restart(context.Background(), c.Force)
}
