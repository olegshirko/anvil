package infra

import (
	"fmt"

	"anvil/internal/environment"
	_ "anvil/internal/environment/container/containerd"
	_ "anvil/internal/environment/container/docker"
	_ "anvil/internal/environment/container/incus"
	_ "anvil/internal/environment/container/kubernetes"
	"anvil/internal/usecase"
)

// ensure ContainerManagerFactoryImpl implements usecase.ContainerManagerFactory
var _ usecase.ContainerManagerFactory = (*ContainerManagerFactoryImpl)(nil)

// vmEnvironmentProvider provides access to the VM's host and guest environments.
// This is satisfied by *LimaVMManagerAdapter via its embedded environment.VM.
type vmEnvironmentProvider interface {
	Host() environment.HostActions
	Guest() environment.GuestActions
}

// ContainerManagerFactoryImpl is an infrastructure implementation of usecase.ContainerManagerFactory.
type ContainerManagerFactoryImpl struct {
	vm            vmEnvironmentProvider
	configService usecase.ConfigService
}

// NewContainerManagerFactory creates a new ContainerManagerFactoryImpl.
func NewContainerManagerFactory(vm vmEnvironmentProvider, configService usecase.ConfigService) *ContainerManagerFactoryImpl {
	return &ContainerManagerFactoryImpl{
		vm:            vm,
		configService: configService,
	}
}

// Get returns a container manager for the given runtime.
func (f *ContainerManagerFactoryImpl) Get(runtime string) (usecase.ContainerManager, error) {
	profile := f.configService.Profile()
	container, err := environment.RuntimeFor(runtime, f.vm.Host(), f.vm.Guest(), f.configService.ProfileConfigDir(profile), profile.ID, f.configService.CacheDir())
	if err != nil {
		return nil, fmt.Errorf("error creating container: %w", err)
	}

	return NewContainerManagerAdapter(container, f.configService), nil
}
