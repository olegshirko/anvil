package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"anvil/internal/domain"

	"github.com/docker/go-units"
	log "github.com/sirupsen/logrus"
)

// SSHConfigManager defines the interface for managing SSH configuration.
type SSHConfigManager interface {
	Generate() error
	Show(ctx context.Context) (string, error)
}

// Application defines the interface for the main application service.
// It orchestrates the main functionalities of the application.
type Application interface {
	Start(ctx context.Context, config domain.Config) error
	Stop(ctx context.Context, force bool) error
	Restart(ctx context.Context, force bool) error
	Delete(ctx context.Context, data, force bool) error
	SSH(ctx context.Context, args ...string) error
	SSHConfig(ctx context.Context) (string, error)
	Status(ctx context.Context, extended bool, json bool) error
	Version(ctx context.Context) error
	Runtime(ctx context.Context) (string, error)
	Update(ctx context.Context) error
	Prune(ctx context.Context, all bool) error
	List(ctx context.Context, profileArgs []string, jsonOutput bool) ([]domain.InstanceInfo, error)
	Watch(ctx context.Context, args []string) error
	Active() bool
	Kubernetes(ctx context.Context) (ContainerManager, error)

	// Health checks the health of the active profile.
	Health(ctx context.Context, jsonOutput bool) error
	// Doctor diagnoses host readiness.
	Doctor(ctx context.Context, jsonOutput bool) error

	// Profile returns the currently active profile.
	Profile() *domain.Profile
	// LoadConfig loads the configuration for the current profile.
	LoadConfig(ctx context.Context) (domain.Config, error)
	// SaveConfig saves the configuration for the current profile.
	SaveConfig(ctx context.Context, conf domain.Config) error
	// ConfigService returns the config service for direct access.
	ConfigService() ConfigService

	// ImageDiscovery returns the image discovery service.
	ImageDiscovery() ImageDiscovery
	// ImageRequester returns the image requester service.
	ImageRequester() ImageRequester
}

// ApplicationImpl is the implementation of the Application interface.
type ApplicationImpl struct {
	configRepo     ConfigRepository
	configService  ConfigService
	profileMan     ProfileManager
	vmMan          VMManager
	contMan        ContainerManager
	sshConfigMan   SSHConfigManager
	pathValidator  PathValidator
	statusProvider StatusProvider
	contManFactory ContainerManagerFactory
	doctorService  DoctorService
	imageDiscovery ImageDiscovery
	imageRequester ImageRequester
}

// NewApplication creates a new ApplicationImpl.
func NewApplication(
	configRepo ConfigRepository,
	profileMan ProfileManager,
	vmMan VMManager,
	contMan ContainerManager,
	sshConfigMan SSHConfigManager,
	pathValidator PathValidator,
	statusProvider StatusProvider,
	contManFactory ContainerManagerFactory,
	doctorService DoctorService,
	imageDiscovery ImageDiscovery,
	imageRequester ImageRequester,
) *ApplicationImpl {
	return &ApplicationImpl{
		configRepo:     configRepo,
		configService:  NewConfigService(configRepo, profileMan),
		profileMan:     profileMan,
		vmMan:          vmMan,
		contMan:        contMan,
		sshConfigMan:   sshConfigMan,
		pathValidator:  pathValidator,
		statusProvider: statusProvider,
		contManFactory: contManFactory,
		doctorService:  doctorService,
		imageDiscovery: imageDiscovery,
		imageRequester: imageRequester,
	}
}

// ConfigService returns the config service.
func (a *ApplicationImpl) ConfigService() ConfigService {
	return a.configService
}

// ImageDiscovery returns the image discovery service.
func (a *ApplicationImpl) ImageDiscovery() ImageDiscovery {
	return a.imageDiscovery
}

// ImageRequester returns the image requester service.
func (a *ApplicationImpl) ImageRequester() ImageRequester {
	return a.imageRequester
}

// ensure ApplicationImpl implements Application
var _ Application = (*ApplicationImpl)(nil)

func (a *ApplicationImpl) Start(ctx context.Context, conf domain.Config) error {
	log.Infof("starting %s", a.profileMan.GetCurrentProfile(ctx).DisplayName)
	log.Tracef("starting with config file: %s\n", a.configRepo.GetProfileConfigPath(a.profileMan.GetCurrentProfile(ctx)))

	startTime := time.Now()

	var containers []ContainerManager
	if conf.Runtime != "none" {
		cs, err := a.startWithRuntime(conf)
		if err != nil {
			return err
		}
		containers = cs
	}

	vmStart := time.Now()
	if err := a.vmMan.StartVM(ctx, conf); err != nil {
		return fmt.Errorf("error starting vm: %w", err)
	}
	log.Printf("vm startup took %s", time.Since(vmStart))

	for _, cont := range containers {
		provisionStart := time.Now()
		log.Info("provisioning ...")
		if err := cont.Provision(ctx, conf); err != nil {
			return fmt.Errorf("error provisioning %s: %w", "container", err)
		}
		log.Printf("provisioning %s took %s", cont.Name(), time.Since(provisionStart))

		startStart := time.Now()
		log.Info("starting ...")
		if err := cont.Start(ctx, conf); err != nil {
			return fmt.Errorf("error starting %s: %w", "container", err)
		}
		log.Printf("starting %s took %s", cont.Name(), time.Since(startStart))
	}

	if err := a.setRuntime(conf.Runtime); err != nil {
		log.Errorf("error persisting runtime settings: %v", err)
	}

	if err := a.setKubernetes(conf.Kubernetes); err != nil {
		log.Errorf("error persisting kubernetes settings: %v", err)
	}

	log.Printf("total startup took %s", time.Since(startTime))
	log.Info("done")

	if err := a.sshConfigMan.Generate(); err != nil {
		log.Trace("error generating ssh_config: %w", err)
	}
	return nil
}

func (a *ApplicationImpl) Stop(ctx context.Context, force bool) error {
	log.Infof("stopping %s", a.profileMan.GetCurrentProfile(ctx).DisplayName)

	if a.vmMan.IsVMRunning(ctx, a.profileMan.GetCurrentProfile(ctx).ID) && !force {
		containers, err := a.currentContainerEnvironments(ctx)
		if err != nil {
			log.Warnf("error retrieving runtimes: %v", err)
		}

		for i := len(containers) - 1; i >= 0; i-- {
			cont := containers[i]
			log.Info("stopping ...")

			if err := cont.Stop(ctx, domain.Config{}); err != nil {
				log.Warnf("error stopping %s: %v", "container", err)
			}
		}
	}

	if err := a.vmMan.StopVM(ctx, a.profileMan.GetCurrentProfile(ctx).ID, force); err != nil {
		return fmt.Errorf("error stopping vm: %w", err)
	}

	log.Info("done")

	if err := a.sshConfigMan.Generate(); err != nil {
		log.Tracef("error generating ssh_config: %v", err)
	}
	return nil
}

func (a *ApplicationImpl) Restart(ctx context.Context, force bool) error {
	if _, err := a.vmMan.Instance(); err != nil {
		return err
	}

	if err := a.Stop(ctx, force); err != nil {
		return err
	}

	// poll until VM is fully stopped before restarting
	for i := 0; i < 30 && a.Active(); i++ {
		time.Sleep(100 * time.Millisecond)
	}

	conf, err := a.configRepo.Load(ctx, a.profileMan.GetCurrentProfile(ctx))
	if err != nil {
		return err
	}

	return a.Start(ctx, conf)
}

func (a *ApplicationImpl) SSHConfig(ctx context.Context) (string, error) {
	return a.sshConfigMan.Show(ctx)
}

func (a *ApplicationImpl) Prune(ctx context.Context, all bool) error {
	if all {
		if err := a.vmMan.Prune(); err != nil {
			return fmt.Errorf("error during Lima prune: %w", err)
		}
	}
	return nil
}

func (a *ApplicationImpl) List(ctx context.Context, profileArgs []string, jsonOutput bool) ([]domain.InstanceInfo, error) {
	return a.statusProvider.ListInstances(ctx, profileArgs)
}

func (a *ApplicationImpl) Delete(ctx context.Context, data, force bool) error {
	log.Infof("deleting %s", a.profileMan.GetCurrentProfile(ctx).DisplayName)

	if a.vmMan.IsVMRunning(ctx, a.profileMan.GetCurrentProfile(ctx).ID) {
		containers, err := a.currentContainerEnvironments(ctx)
		if err != nil {
			log.Warnf("error retrieving runtimes: %v", err)
		}
		for _, cont := range containers {
			log.Info("deleting ...")
			if err := cont.Teardown(ctx, domain.Config{}); err != nil {
				log.Warnf("error during teardown of %s: %v", "container", err)
			}
		}
	}

	if err := a.vmMan.DeleteVM(ctx, a.profileMan.GetCurrentProfile(ctx).ID, data, force); err != nil {
		return fmt.Errorf("error during teardown of vm: %w", err)
	}

	if err := a.configRepo.Teardown(ctx, a.profileMan.GetCurrentProfile(ctx)); err != nil {
		return fmt.Errorf("error deleting configs: %w", err)
	}

	log.Info("done")

	if err := a.sshConfigMan.Generate(); err != nil {
		log.Tracef("error generating ssh_config: %v", err)
	}
	return nil
}

func (a *ApplicationImpl) startWithRuntime(conf domain.Config) ([]ContainerManager, error) {
	kubernetesEnabled := conf.Kubernetes.Enabled

	switch conf.Runtime {
	case "docker", "containerd":
	default:
		kubernetesEnabled = false
	}

	return a.getContainerManagers(conf.Runtime, kubernetesEnabled)
}

func (a *ApplicationImpl) SSH(ctx context.Context, args ...string) error {
	if !a.vmMan.IsVMRunning(ctx, a.profileMan.GetCurrentProfile(ctx).ID) {
		return fmt.Errorf("%s not running", a.profileMan.GetCurrentProfile(ctx).DisplayName)
	}

	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("error retrieving current working directory: %w", err)
	}

	isMounted, err := a.pathValidator.IsMounted(workDir)
	if err != nil || !isMounted {
		log.Tracef("error checking if PWD is mounted: %v", err)
		// fallback to the user's homedir; empty string lets limactl shell use $HOME
		workDir = ""
	}

	return a.vmMan.SSH(workDir, args...)
}

func (a *ApplicationImpl) Watch(ctx context.Context, args []string) error {
	return a.vmMan.Watch(args...)
}

func (a *ApplicationImpl) Status(ctx context.Context, extended bool, jsonOutput bool) error {
	status, err := a.statusProvider.GetStatus(ctx)
	if err != nil {
		return err
	}

	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(status); err != nil {
			return fmt.Errorf("error encoding status as json: %w", err)
		}
	} else {
		log.Infof("%s is running using %s", status.DisplayName, status.Driver)
		log.Infof("arch: %s", status.Arch)
		log.Infof("runtime: %s", status.Runtime)
		if status.MountType != "" {
			log.Infof("mountType: %s", status.MountType)
		}

		if status.IPAddress != "" {
			log.Infof("address: %s", status.IPAddress)
		}

		if status.DockerSocket != "" {
			log.Infof("docker socket: %s", status.DockerSocket)
		}
		if status.ContainerdSocket != "" {
			log.Infof("containerd socket: %s", status.ContainerdSocket)
		}
		if status.BuildkitdSocket != "" {
			log.Infof("buildkitd socket: %s", status.BuildkitdSocket)
		}
		if status.IncusSocket != "" {
			log.Infof("incus socket: %s", status.IncusSocket)
		}

		if status.Kubernetes {
			log.Info("kubernetes: enabled")
		}

		if extended {
			if status.CPU > 0 {
				log.Infof("cpu: %d", status.CPU)
			}
			if status.Memory > 0 {
				log.Infof("mem: %s", units.BytesSize(float64(status.Memory)))
			}
			if status.Disk > 0 {
				log.Infof("disk: %s", units.BytesSize(float64(status.Disk)))
			}
		}
	}
	return nil
}

func (a *ApplicationImpl) Health(ctx context.Context, jsonOutput bool) error {
	report, err := a.statusProvider.GetHealth(ctx)
	if err != nil {
		return err
	}

	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(report)
	}

	log.Infof("health: %s", report.Overall)
	for _, check := range report.Checks {
		switch check.Status {
		case "ok":
			log.Infof("  ✓ %s: %s", check.Component, check.Message)
		case "warning":
			log.Warnf("  ⚠ %s: %s", check.Component, check.Message)
		case "error":
			log.Errorf("  ✗ %s: %s", check.Component, check.Message)
		}
	}
	return nil
}

func (a *ApplicationImpl) Doctor(ctx context.Context, jsonOutput bool) error {
	diagnoses, err := a.doctorService.Diagnose(ctx)
	if err != nil {
		return err
	}

	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(diagnoses)
	}

	for _, d := range diagnoses {
		switch d.Status {
		case "ok":
			log.Infof("  ✓ %s", d.Check)
		case "warn":
			log.Warnf("  ⚠ %s: %s", d.Check, d.Message)
		case "fail":
			log.Errorf("  ✗ %s: %s", d.Check, d.Message)
			if d.Fix != "" {
				log.Infof("    fix: %s", d.Fix)
			}
		}
	}
	return nil
}

func (a *ApplicationImpl) Version(ctx context.Context) error {
	if !a.vmMan.IsVMRunning(ctx, a.profileMan.GetCurrentProfile(ctx).ID) {
		return nil
	}

	containerRuntimes, err := a.currentContainerEnvironments(ctx)
	if err != nil {
		return err
	}

	var kube ContainerManager
	for _, cont := range containerRuntimes {
		if cont.Name() == "kubernetes" {
			kube = cont
			continue
		}

		log.Infof("\nruntime: %s", cont.Name())
		log.Infof("arch: %s", a.vmMan.Arch().String())
		version, err := cont.Version(ctx)
		if err != nil {
			log.Warnf("%v", err)
			continue
		}
		log.Info(version)
	}

	if kube != nil {
		version, err := kube.Version(ctx)
		if err == nil && version != "" {
			log.Info("\nkubernetes")
			log.Info(version)
		}
	}

	return nil
}

func (a *ApplicationImpl) Runtime(ctx context.Context) (string, error) {
	return a.contMan.CurrentRuntime(ctx)
}

func (a *ApplicationImpl) Update(ctx context.Context) error {
	if !a.vmMan.IsVMRunning(ctx, a.profileMan.GetCurrentProfile(ctx).ID) {
		return fmt.Errorf("runtime cannot be updated, %s is not running", a.profileMan.GetCurrentProfile(ctx).DisplayName)
	}

	updated, oldVersion, err := a.contMan.Update(ctx)
	if err != nil {
		return err
	}

	if updated {
		fmt.Println()
		fmt.Println("Previous")
		fmt.Println(oldVersion)
		fmt.Println()
		fmt.Println("Current")
		currentVersion, _ := a.contMan.Version(ctx)
		fmt.Println(currentVersion)
	}

	return nil
}

func (a *ApplicationImpl) setRuntime(runtime string) error {
	return a.configRepo.SetRuntime(context.Background(), runtime)
}

func (a *ApplicationImpl) setKubernetes(conf domain.Kubernetes) error {
	return a.configRepo.SetKubernetes(context.Background(), conf)
}

func (a *ApplicationImpl) getContainerManagers(runtime string, kubernetesEnabled bool) ([]ContainerManager, error) {
	var containers []ContainerManager

	// runtime
	{
		env, err := a.contManFactory.Get(runtime)
		if err != nil {
			return nil, err
		}
		containers = append(containers, env)
	}

	// kubernetes should come after required runtime
	if kubernetesEnabled {
		env, err := a.contManFactory.Get("kubernetes")
		if err != nil {
			return nil, err
		}
		containers = append(containers, env)
	}

	return containers, nil
}

func (a *ApplicationImpl) currentContainerEnvironments(ctx context.Context) ([]ContainerManager, error) {
	profile := a.profileMan.GetCurrentProfile(ctx)
	conf, err := a.configRepo.LoadInstanceState(ctx, profile)
	if err != nil {
		return nil, fmt.Errorf("error loading instance state: %w", err)
	}

	if conf.Runtime == "none" {
		return nil, nil
	}

	k, err := a.contManFactory.Get("kubernetes")
	if err != nil {
		return nil, err
	}
	kubernetesEnabled := k.IsKubernetesRunning(ctx)

	return a.getContainerManagers(conf.Runtime, kubernetesEnabled)
}

func (a *ApplicationImpl) Active() bool {
	return a.vmMan.IsVMRunning(context.Background(), a.profileMan.GetCurrentProfile(context.Background()).ID)
}

func (a *ApplicationImpl) Kubernetes(ctx context.Context) (ContainerManager, error) {
	return a.contManFactory.Get("kubernetes")
}

func (a *ApplicationImpl) Profile() *domain.Profile {
	return a.profileMan.GetCurrentProfile(context.Background())
}

func (a *ApplicationImpl) LoadConfig(ctx context.Context) (domain.Config, error) {
	return a.configRepo.Load(ctx, a.profileMan.GetCurrentProfile(ctx))
}

func (a *ApplicationImpl) SaveConfig(ctx context.Context, conf domain.Config) error {
	return a.configRepo.Save(ctx, a.profileMan.GetCurrentProfile(ctx), conf)
}
