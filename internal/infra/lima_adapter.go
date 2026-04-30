package infra

import (
	"context"

	"anvil/internal/domain"
	"anvil/internal/environment"
	"anvil/internal/environment/host"
	"anvil/internal/environment/vm/lima"
	"anvil/internal/environment/vm/lima/limautil"
	"anvil/internal/usecase"
)

// ensure LimaVMManagerAdapter implements usecase.VMManager
var _ usecase.VMManager = (*LimaVMManagerAdapter)(nil)

// LimaVMManagerAdapter is an adapter for lima.limaVM to implement usecase.VMManager.
type LimaVMManagerAdapter struct {
	environment.VirtualMachine
	configService usecase.ConfigService
}

// NewLimaVMManagerAdapter creates a new LimaVMManagerAdapter.
func NewLimaVMManagerAdapter(configService usecase.ConfigService, configRepo usecase.ConfigRepository, configHelper *usecase.ConfigHelper) *LimaVMManagerAdapter {
	host := host.New().(environment.HostActions)
	return &LimaVMManagerAdapter{
		VirtualMachine: lima.New(host, configService, configRepo, configHelper),
		configService:  configService,
	}
}

// StartVM starts a virtual machine with the given configuration.
func (a *LimaVMManagerAdapter) StartVM(ctx context.Context, conf domain.Config) error {
	return a.Start(ctx, conf)
}

// StopVM stops a virtual machine. Forcefully stops if force is true.
func (a *LimaVMManagerAdapter) StopVM(ctx context.Context, profileID string, force bool) error {
	return a.Stop(ctx, force)
}

// DeleteVM deletes a virtual machine.
func (a *LimaVMManagerAdapter) DeleteVM(ctx context.Context, profileID string, deleteData, force bool) error {
	return a.Teardown(ctx)
}

// GetVMStatus retrieves the current status of a virtual machine.
func (a *LimaVMManagerAdapter) GetVMStatus(ctx context.Context, profileID string) (usecase.VMStatus, error) {
	running := a.Running(ctx)
	return usecase.VMStatus{
		ProfileName: profileID,
		Running:     running,
		Arch:        string(a.Arch()),
	}, nil
}

// IsVMRunning checks if a specific virtual machine is currently running.
func (a *LimaVMManagerAdapter) IsVMRunning(ctx context.Context, profileID string) bool {
	return a.Running(ctx)
}

// SSH executes a shell command in the virtual machine.
func (a *LimaVMManagerAdapter) SSH(workDir string, args ...string) error {
	return a.VirtualMachine.SSH(workDir, args...)
}

// User returns the current user in the virtual machine.
func (a *LimaVMManagerAdapter) User() (string, error) {
	return a.VirtualMachine.User()
}

// Arch returns the architecture of the VM.
func (a *LimaVMManagerAdapter) Arch() domain.Arch {
	return a.VirtualMachine.Arch()
}

// IPAddress returns the IP address of the VM.
func (a *LimaVMManagerAdapter) IPAddress(ctx context.Context, profileID string) string {
	return limautil.NewNetworkInspector(profileID).FindIP()
}

// Instance returns information about the running VM instance.
func (a *LimaVMManagerAdapter) Instance() (domain.Instance, error) {
	inst, err := limautil.Instance(a.configService.Profile().ID)
	if err != nil {
		return domain.Instance{}, err
	}
	return domain.Instance{
		CPU:    inst.CPU,
		Memory: inst.Memory,
		Disk:   inst.Disk,
	}, nil
}

// Prune prunes cached Lima assets.
func (a *LimaVMManagerAdapter) Prune() error {
	return limautil.Limactl("prune").Run()
}

// Watch watches Lima instance events.
func (a *LimaVMManagerAdapter) Watch(args ...string) error {
	shellArgs := append([]string{limautil.LimactlCommand, "watch", a.configService.Profile().ID}, args...)
	return a.VirtualMachine.Host().RunInteractive(shellArgs...)
}
