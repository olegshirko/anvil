//go:build !linux

package main

import (
	"context"

	"github.com/containerd/containerd/v2/client"
)

func mountContainerBinds(ctx context.Context, c client.Container, root string) {}
