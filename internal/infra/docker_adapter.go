package infra

import (
	"context"
	"fmt"

	"anvil/internal/domain"
	"anvil/internal/environment"
	"anvil/internal/environment/container/containerd"
	"anvil/internal/environment/container/docker"
	"anvil/internal/environment/container/incus"
	"anvil/internal/store"
	"anvil/internal/usecase"
)

// ensure ContainerManagerAdapter implements usecase.ContainerManager
var _ usecase.ContainerManager = (*ContainerManagerAdapter)(nil)

// ContainerManagerAdapter is an adapter for environment.ContainerRuntime to implement usecase.ContainerManager.
type ContainerManagerAdapter struct {
	environment.ContainerRuntime
	configService usecase.ConfigService
}

// NewContainerManagerAdapter creates a new ContainerManagerAdapter.
func NewContainerManagerAdapter(container environment.ContainerRuntime, configService usecase.ConfigService) *ContainerManagerAdapter {
	return &ContainerManagerAdapter{
		ContainerRuntime: container,
		configService:    configService,
	}
}

// Provision prepares the container runtime.
func (a *ContainerManagerAdapter) Provision(ctx context.Context, config domain.Config) error {
	return a.ContainerRuntime.Provision(ctx, config)
}

// Start starts the container runtime.
func (a *ContainerManagerAdapter) Start(ctx context.Context, config domain.Config) error {
	return a.ContainerRuntime.Start(ctx, config)
}

// Stop stops the container runtime.
func (a *ContainerManagerAdapter) Stop(ctx context.Context, config domain.Config) error {
	return a.ContainerRuntime.Stop(ctx)
}

// Teardown cleans up the container runtime.
func (a *ContainerManagerAdapter) Teardown(ctx context.Context, config domain.Config) error {
	return a.ContainerRuntime.Teardown(ctx)
}

// GetContainerRuntimeStatus retrieves the current status of the container runtime.
func (a *ContainerManagerAdapter) GetContainerRuntimeStatus(ctx context.Context, config domain.Config) (usecase.ContainerRuntimeStatus, error) {
	running := a.Running(ctx)
	return usecase.ContainerRuntimeStatus{
		Name:    a.ContainerRuntime.Name(),
		Running: running,
	}, nil
}

// IsContainerRuntimeRunning checks if the container runtime is currently running.
func (a *ContainerManagerAdapter) IsContainerRuntimeRunning(ctx context.Context, config domain.Config) bool {
	return a.Running(ctx)
}

// HostSocketFile returns the path to the host socket file for the container runtime.
func (a *ContainerManagerAdapter) HostSocketFile() string {
	switch a.ContainerRuntime.Name() {
	case docker.Name:
		return docker.HostSocketFile(a.configService.ProfileConfigDir(a.configService.Profile()))
	case incus.Name:
		return incus.HostSocketFile(a.configService.ProfileConfigDir(a.configService.Profile()))
	default:
		return ""
	}
}

// HostSocketFiles returns the paths to the host socket files for containerd.
func (a *ContainerManagerAdapter) HostSocketFiles() domain.HostSocketFiles {
	switch a.ContainerRuntime.Name() {
	case docker.Name, containerd.Name:
		files := containerd.HostSocketFiles(a.configService.ProfileConfigDir(a.configService.Profile()))
		return domain.HostSocketFiles{
			Containerd: files.Containerd,
			Buildkitd:  files.Buildkitd,
		}
	default:
		return domain.HostSocketFiles{}
	}
}

// IsKubernetesRunning checks if Kubernetes is running.
func (a *ContainerManagerAdapter) IsKubernetesRunning(ctx context.Context) bool {
	return false
}

// CurrentRuntime returns the name of the current container runtime.
func (a *ContainerManagerAdapter) CurrentRuntime(ctx context.Context) (string, error) {
	profile := a.configService.Profile()
	storeFile := a.configService.ProfileStoreFile(profile)
	s, err := store.Fetch(storeFile)
	if err != nil {
		return "", fmt.Errorf("error loading store: %w", err)
	}
	if s.DiskRuntime != "" {
		return s.DiskRuntime, nil
	}
	// fallback to configured runtime
	conf, err := a.configService.LoadConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("error loading config: %w", err)
	}
	if conf.Runtime != "" {
		return conf.Runtime, nil
	}
	return "", fmt.Errorf("error retrieving current runtime: empty value")
}

// Version returns the version of the container runtime.
func (a *ContainerManagerAdapter) Version(ctx context.Context) (string, error) {
	profile := a.configService.Profile()
	return a.ContainerRuntime.Version(ctx, profile.ID), nil
}

// Name returns the name of the container runtime.
func (a *ContainerManagerAdapter) Name() string {
	return a.ContainerRuntime.Name()
}

// Update updates the container runtime.
func (a *ContainerManagerAdapter) Update(ctx context.Context) (bool, string, error) {
	profile := a.configService.Profile()
	oldVersion := a.ContainerRuntime.Version(ctx, profile.ID)
	updated, err := a.ContainerRuntime.Update(ctx)
	return updated, oldVersion, err
}
