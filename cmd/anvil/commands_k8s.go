package main

import (
	"context"
	"fmt"

	"anvil/internal/domain"
)

// kubernetesCmd

type kubernetesCmd struct {
	Start  kubernetesStartCmd  `cmd:"" help:"Start the Kubernetes cluster"`
	Stop   kubernetesStopCmd   `cmd:"" help:"Stop the Kubernetes cluster"`
	Delete kubernetesDeleteCmd `cmd:"" help:"Delete the Kubernetes cluster"`
	Reset  kubernetesResetCmd  `cmd:"" help:"Reset the Kubernetes cluster"`
}

type kubernetesStartCmd struct{}

func (c *kubernetesStartCmd) Run(g *Globals) error {
	app := g.app
	k, err := app.Kubernetes(context.Background())
	if err != nil {
		return err
	}
	conf, err := g.app.LoadConfig(context.Background())
	if err != nil {
		return err
	}
	if err := k.Provision(context.Background(), conf); err != nil {
		return err
	}
	return k.Start(context.Background(), conf)
}

type kubernetesStopCmd struct{}

func (c *kubernetesStopCmd) Run(g *Globals) error {
	app := g.app
	k, err := app.Kubernetes(context.Background())
	if err != nil {
		return err
	}
	conf, err := g.app.LoadConfig(context.Background())
	if err != nil {
		return err
	}
	if !k.IsKubernetesRunning(context.Background()) {
		return fmt.Errorf("%s is not enabled", domain.RuntimeKubernetes)
	}
	return k.Stop(context.Background(), conf)
}

type kubernetesDeleteCmd struct{}

func (c *kubernetesDeleteCmd) Run(g *Globals) error {
	app := g.app
	k, err := app.Kubernetes(context.Background())
	if err != nil {
		return err
	}
	conf, err := g.app.LoadConfig(context.Background())
	if err != nil {
		return err
	}
	if !k.IsKubernetesRunning(context.Background()) {
		return fmt.Errorf("%s is not enabled", domain.RuntimeKubernetes)
	}
	return k.Teardown(context.Background(), conf)
}

type kubernetesResetCmd struct{}

func (c *kubernetesResetCmd) Run(g *Globals) error {
	app := g.app
	k, err := app.Kubernetes(context.Background())
	if err != nil {
		return err
	}
	conf, err := g.app.LoadConfig(context.Background())
	if err != nil {
		return err
	}
	if err := k.Teardown(context.Background(), conf); err != nil {
		return fmt.Errorf("error deleting %s: %w", domain.RuntimeKubernetes, err)
	}
	if err := k.Provision(context.Background(), conf); err != nil {
		return err
	}
	if err := k.Start(context.Background(), conf); err != nil {
		return fmt.Errorf("error starting %s: %w", domain.RuntimeKubernetes, err)
	}
	return nil
}
