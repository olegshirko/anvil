package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"anvil/internal/cli"
	"anvil/internal/domain"
	"anvil/internal/environment"
	"anvil/internal/environment/container/containerd"
	"anvil/internal/environment/container/docker"
)

const (
	Name           = "kubernetes"
	DefaultVersion = "v1.33.4+k3s1"
	ConfigKey      = "kubernetes_config"
)

func createK8sRuntime(host environment.HostActions, guest environment.GuestActions, profileConfigDir string, profileID string, cacheDir string) environment.ContainerRuntime {
	return &k3sEngine{
		host:             host,
		guest:            guest,
		profileConfigDir: profileConfigDir,
		profileID:        profileID,
		cacheDir:         cacheDir,
		CommandChain:     cli.New(Name),
	}
}

func init() {
	environment.RegisterRuntime(Name, createK8sRuntime, true)
}

var _ environment.ContainerRuntime = (*k3sEngine)(nil)

type k3sEngine struct {
	host             environment.HostActions
	guest            environment.GuestActions
	profileConfigDir string
	profileID        string
	cacheDir         string
	cli.CommandChain
}

func (k k3sEngine) Name() string { return Name }

func (k k3sEngine) uninstallScriptExists() bool {
	return k.guest.RunQuiet("command", "-v", "k3s-uninstall.sh") == nil
}

func (k k3sEngine) matchesVersion(v string) bool {
	out, err := k.guest.RunOutput("k3s", "--version")
	if err != nil {
		return false
	}
	return strings.Contains(out, v)
}

func (k k3sEngine) Running(context.Context) bool {
	return k.guest.RunQuiet("sudo", "service", "k3s", "status") == nil
}

func (k k3sEngine) containerBackend() string {
	return k.guest.Setting(environment.ContainerRuntimeKey)
}

func (k k3sEngine) persistK8sConfig(conf domain.Kubernetes) error {
	b, err := json.Marshal(conf)
	if err != nil {
		return fmt.Errorf("cannot marshal kubernetes config: %w", err)
	}
	return k.guest.SetSetting(ConfigKey, string(b))
}

// Provision installs k3s and CNI if needed.
func (k *k3sEngine) Provision(ctx context.Context, appConf domain.Config) error {
	log := k.Logger(ctx)
	pipe := k.Init(ctx)
	if k.Running(ctx) {
		return nil
	}

	runtime := appConf.Runtime
	conf := appConf.Kubernetes
	if conf.Version == "" {
		conf.Version = DefaultVersion
	}

	if k.matchesVersion(conf.Version) {
		if current := k.containerBackend(); current != "" && current != runtime {
			pipe.Stagef("switching container runtime to %s", runtime)
			stageAirGapImages(k.host, k.guest, pipe, log, conf.Version, k.cacheDir)
			bootstrapCluster(k.host, k.guest, pipe, runtime, conf.Version, conf.K3sArgs, conf.Port, k.profileID, k.cacheDir)
		}
	} else {
		if k.uninstallScriptExists() {
			pipe.Stagef("upgrading to %s", conf.Version)
		} else {
			pipe.Stage("installing k3s")
		}
		provisionK3s(k.host, k.guest, pipe, log, runtime, conf.Version, conf.K3sArgs, conf.Port, k.profileID, k.cacheDir)
	}

	setupCNI(k.guest, pipe)
	pipe.Add(func() error { return k.persistK8sConfig(conf) })
	return pipe.Exec()
}

// Start launches the k3s service and waits for cluster-info.
func (k k3sEngine) Start(ctx context.Context, appConf domain.Config) error {
	log := k.Logger(ctx)
	pipe := k.Init(ctx)
	if k.Running(ctx) {
		log.Println("k3s is already running")
		return nil
	}

	pipe.Add(func() error {
		return k.guest.Run("sudo", "service", "k3s", "start")
	})
	pipe.Retry("", time.Second, 15, func(int) error {
		return k.guest.RunQuiet("kubectl", "cluster-info")
	})

	if err := pipe.Exec(); err != nil {
		return err
	}
	return k.syncKubeconfig(ctx, appConf)
}

// Stop halts k3s and kills remaining containers.
func (k k3sEngine) Stop(ctx context.Context) error {
	pipe := k.Init(ctx)
	pipe.Add(func() error {
		return k.guest.Run("k3s-killall.sh")
	})
	pipe.Add(k.killContainers)
	return pipe.Exec()
}

// Teardown uninstalls k3s and cleans up.
func (k k3sEngine) Teardown(ctx context.Context) error {
	pipe := k.Init(ctx)
	if k.uninstallScriptExists() {
		pipe.Add(func() error {
			return k.guest.Run("k3s-uninstall.sh")
		})
	}
	pipe.Add(k.purgeContainers)
	k.resetKubeconfig(pipe)
	return pipe.Exec()
}

func (k k3sEngine) purgeContainers() error {
	ids := k.listContainerIDs()
	if ids == "" {
		return nil
	}
	var args []string
	switch k.containerBackend() {
	case containerd.Name:
		args = []string{"nerdctl", "-n", "k8s.io", "rm", "-f"}
	case docker.Name:
		args = []string{"docker", "rm", "-f"}
	default:
		return nil
	}
	args = append(args, strings.Fields(ids)...)
	return k.guest.Run("sudo", "sh", "-c", strings.Join(args, " "))
}

func (k k3sEngine) killContainers() error {
	ids := k.listContainerIDs()
	if ids == "" {
		return nil
	}
	var args []string
	switch k.containerBackend() {
	case containerd.Name:
		args = []string{"nerdctl", "-n", "k8s.io", "kill"}
	case docker.Name:
		args = []string{"docker", "kill"}
	default:
		return nil
	}
	args = append(args, strings.Fields(ids)...)
	return k.guest.Run("sudo", "sh", "-c", strings.Join(args, " "))
}

func (k k3sEngine) listContainerIDs() string {
	var args []string
	switch k.containerBackend() {
	case containerd.Name:
		args = []string{"sudo", "nerdctl", "-n", "k8s.io", "ps", "-q"}
	case docker.Name:
		args = []string{"sudo", "sh", "-c", `docker ps --format '{{.Names}}'| grep "k8s_"`}
	default:
		return ""
	}
	out, _ := k.guest.RunOutput(args...)
	if out == "" {
		return ""
	}
	return strings.ReplaceAll(out, "\n", " ")
}

func (k k3sEngine) Dependencies() []string {
	return []string{"kubectl"}
}

func (k k3sEngine) Version(ctx context.Context, _ string) string {
	v, _ := k.host.RunOutput("kubectl", "--context", k.profileID, "version", "--short")
	return v
}

func (k *k3sEngine) Update(ctx context.Context) (bool, error) {
	return false, fmt.Errorf("in-place upgrade is not supported for %s", Name)
}

func (k k3sEngine) Host() environment.HostActions   { return k.host }
func (k k3sEngine) Guest() environment.GuestActions { return k.guest }
