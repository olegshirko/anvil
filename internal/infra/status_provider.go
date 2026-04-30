package infra

import (
	"context"
	"fmt"

	"anvil/internal/domain"
	"anvil/internal/environment/container/containerd"
	"anvil/internal/environment/container/docker"
	"anvil/internal/environment/vm/lima/limautil"
	"anvil/internal/usecase"
)

// ensure StatusProviderImpl implements usecase.StatusProvider
var _ usecase.StatusProvider = (*StatusProviderImpl)(nil)

// StatusProviderImpl is an infrastructure implementation of usecase.StatusProvider.
type StatusProviderImpl struct {
	configRepo usecase.ConfigRepository
	profileMan usecase.ProfileManager
	vmMan      usecase.VMManager
	contMan    usecase.ContainerManager
}

// NewStatusProvider creates a new StatusProviderImpl.
func NewStatusProvider(
	configRepo usecase.ConfigRepository,
	profileMan usecase.ProfileManager,
	vmMan usecase.VMManager,
	contMan usecase.ContainerManager,
) *StatusProviderImpl {
	return &StatusProviderImpl{
		configRepo: configRepo,
		profileMan: profileMan,
		vmMan:      vmMan,
		contMan:    contMan,
	}
}

// GetStatus retrieves the current status of the application.
func (p *StatusProviderImpl) GetStatus(ctx context.Context) (domain.StatusInfo, error) {
	var status domain.StatusInfo

	profile := p.profileMan.GetCurrentProfile(ctx)
	if !p.vmMan.IsVMRunning(ctx, profile.ID) {
		return status, fmt.Errorf("%s is not running. Run 'anvil start --profile %s' to start it", profile.DisplayName, profile.ID)
	}

	currentRuntime, err := p.contMan.CurrentRuntime(ctx)
	if err != nil {
		return status, fmt.Errorf("error getting current runtime: %w", err)
	}

	status.DisplayName = profile.DisplayName
	status.Driver = "QEMU" // default
	conf, _ := p.configRepo.LoadInstanceState(ctx, profile)
	if conf.Runtime != "" {
		status.Driver = p.configRepo.ConfigDriverLabel(conf)
	}
	status.Arch = p.vmMan.Arch().String()
	status.Runtime = currentRuntime
	status.MountType = conf.MountType

	ipAddress := p.vmMan.IPAddress(ctx, profile.ID)
	if ipAddress != "127.0.0.1" {
		status.IPAddress = ipAddress
	}

	if currentRuntime == docker.Name {
		status.DockerSocket = "unix://" + p.contMan.HostSocketFile()
		status.ContainerdSocket = "unix://" + p.contMan.HostSocketFiles().Containerd
	}
	if currentRuntime == containerd.Name {
		status.ContainerdSocket = "unix://" + p.contMan.HostSocketFiles().Containerd
		status.BuildkitdSocket = "unix://" + p.contMan.HostSocketFiles().Buildkitd
	}
	status.Kubernetes = p.contMan.IsKubernetesRunning(ctx)

	inst, err := p.vmMan.Instance()
	if err == nil {
		status.CPU = inst.CPU
		status.Memory = inst.Memory
		status.Disk = inst.Disk
	}

	return status, nil
}

// GetHealth retrieves the health of the active profile.
func (p *StatusProviderImpl) GetHealth(ctx context.Context) (domain.HealthReport, error) {
	profile := p.profileMan.GetCurrentProfile(ctx)
	report := domain.HealthReport{
		Profile: profile.DisplayName,
		Overall: "healthy",
	}

	// VM check
	if p.vmMan.IsVMRunning(ctx, profile.ID) {
		report.Checks = append(report.Checks, domain.HealthCheck{
			Component: "vm",
			Status:    "ok",
			Message:   "running",
		})
	} else {
		report.Checks = append(report.Checks, domain.HealthCheck{
			Component: "vm",
			Status:    "error",
			Message:   "not running",
		})
		report.Overall = "unhealthy"
	}

	// Container runtime check
	conf, err := p.configRepo.LoadInstanceState(ctx, profile)
	if err == nil && conf.Runtime != "" && conf.Runtime != "none" {
		if p.contMan.IsContainerRuntimeRunning(ctx, conf) {
			report.Checks = append(report.Checks, domain.HealthCheck{
				Component: "runtime",
				Status:    "ok",
				Message:   conf.Runtime + " is running",
			})
		} else {
			report.Checks = append(report.Checks, domain.HealthCheck{
				Component: "runtime",
				Status:    "error",
				Message:   conf.Runtime + " is not running",
			})
			if report.Overall == "healthy" {
				report.Overall = "degraded"
			}
		}
	}

	// Kubernetes check
	if conf.Kubernetes.Enabled {
		if p.contMan.IsKubernetesRunning(ctx) {
			report.Checks = append(report.Checks, domain.HealthCheck{
				Component: "kubernetes",
				Status:    "ok",
				Message:   "running",
			})
		} else {
			report.Checks = append(report.Checks, domain.HealthCheck{
				Component: "kubernetes",
				Status:    "error",
				Message:   "not running",
			})
			if report.Overall == "healthy" {
				report.Overall = "degraded"
			}
		}
	}

	return report, nil
}

// ListInstances retrieves a list of VM instances.
func (p *StatusProviderImpl) ListInstances(ctx context.Context, profileArgs []string) ([]domain.InstanceInfo, error) {
	limaIDs := make([]string, len(profileArgs))
	for i, arg := range profileArgs {
		limaIDs[i] = p.profileMan.GetProfileFromName(ctx, arg).ID
	}
	instances, err := limautil.Instances(limaIDs...)
	if err != nil {
		return nil, fmt.Errorf("error listing instances: %w", err)
	}

	result := make([]domain.InstanceInfo, len(instances))
	for i, inst := range instances {
		result[i] = domain.InstanceInfo{
			Name:      inst.Name,
			Status:    inst.Status,
			Arch:      inst.Arch,
			CPU:       inst.CPU,
			Memory:    inst.Memory,
			Disk:      inst.Disk,
			Runtime:   inst.Runtime,
			IPAddress: inst.IPAddress,
			Dir:       inst.Dir,
		}
	}

	return result, nil
}
