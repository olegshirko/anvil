package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"anvil/internal/core"
	"anvil/internal/domain"
	"anvil/internal/embedded"
	"anvil/internal/environment"
	"anvil/internal/usecase"
	"anvil/internal/util"

	log "github.com/sirupsen/logrus"
)

const (
	defaultCPU               = 2
	defaultMemory            = 2
	defaultDisk              = 100
	defaultRootDisk          = 20
	defaultMountTypeQEMU     = "sshfs"
	defaultMountTypeVZ       = "virtiofs"
	defaultKubernetesVersion = domain.KubernetesDefaultVersion
)

var defaultK3sArgs = []string{"--disable=traefik"}

// startCmd defines all flags for the start command.
type startCmd struct {
	ProfileArg string `arg:"" optional:"" help:"Profile name"`

	Runtime       string  `short:"r" help:"Container runtime"`
	CPU           int     `short:"c" help:"Number of CPUs"`
	CPUType       string  `help:"CPU type"`
	Memory        float32 `short:"m" help:"Memory in GiB"`
	Disk          int     `short:"d" help:"Disk size in GiB"`
	RootDisk      int     `help:"Disk size in GiB for the root filesystem"`
	Arch          string  `short:"a" help:"Architecture (aarch64, x86_64)"`
	VMType        string  `short:"t" help:"Virtual machine type (krunkit, qemu, vz)"`
	Hostname      string  `help:"Custom hostname for the virtual machine"`
	DiskImage     string  `short:"i" help:"File path to a custom disk image"`
	PortForwarder string  `help:"Port forwarder to use (ssh, grpc)"`

	Foreground bool   `short:"f" help:"Keep anvil in the foreground"`
	Edit       bool   `short:"e" help:"Edit the configuration file before starting"`
	SaveConfig *bool  `help:"Persist and overwrite config file with specified flags"`
	Template   *bool  `help:"Use the template file for initial configuration"`
	Editor     string `help:"Editor to use for edit"`

	ActivateRuntime      *bool `help:"Set as active Docker/Kubernetes/Incus context on startup"`
	Binfmt               *bool `help:"Use binfmt for foreign architecture emulation"`
	NestedVirtualization bool  `short:"z" help:"Enable nested virtualization"`
	VZRosetta            bool  `help:"Enable Rosetta for amd64 emulation"`

	MountType    string   `help:"Volume driver for the mount"`
	MountINotify *bool    `name:"mount-inotify" help:"Propagate inotify file events to the VM"`
	Mounts       []string `short:"V" help:"Directories to mount, suffix ':w' for writable"`

	ForwardAgent bool  `short:"s" help:"Forward SSH agent to the VM"`
	SSHConfig    *bool `help:"Generate SSH config in ~/.ssh/config"`
	SSHPort      int   `help:"SSH server port"`

	NetworkAddress        bool     `help:"Assign reachable IP address to the VM"`
	NetworkMode           string   `help:"Network mode (shared, bridged)"`
	NetworkBridgeIf       string   `help:"Host network interface to use for bridged mode"`
	NetworkPreferredRoute bool     `help:"Use the assigned IP address as the preferred route"`
	NetworkHostAddresses  bool     `help:"Support port forwarding to specific host IP addresses"`
	GatewayAddress        net.IP   `help:"Gateway address"`
	DNSResolvers          []net.IP `short:"n" help:"DNS resolvers for the VM (e.g. 8.8.8.8)"`
	DNSHosts              []string `help:"Custom DNS host entries as HOST=IP (e.g. myhost=192.168.1.1)"`

	KubernetesEnabled *bool    `short:"k" help:"Start with Kubernetes"`
	KubernetesVersion string   `help:"Kubernetes (k3s) version"`
	K3sArgs           []string `name:"k3s-args" help:"Additional args to pass to k3s"`
	K3sListenPort     int      `name:"k3s-listen-port" help:"k3s server listen port"`

	RegistryMirrors    []string `help:"Docker registry mirrors"`
	InsecureRegistries []string `help:"Docker insecure registries"`

	Env map[string]string `help:"Environment variables for the VM"`

	LegacyCPU        int   `help:"Legacy: number of CPUs"`
	LegacyKubernetes *bool `help:"Legacy: start with Kubernetes"`
}

func (c *startCmd) Run(g *Globals) error {
	g.resolveProfile(c.ProfileArg)
	ctx := context.Background()
	app := g.app

	// validate Lima version
	if err := core.ValidateLimaVersion(); err != nil {
		return fmt.Errorf("lima compatibility error: %w", err)
	}

	conf, err := c.prepareConfig(app)
	if err != nil {
		return err
	}

	if c.Edit {
		conf, err = c.editConfigFile(g.app)
		if err != nil {
			return err
		}
		if err := app.ConfigService().ValidateConfig(conf); err != nil {
			return fmt.Errorf("error in config file: %w", err)
		}
	}

	if !c.Edit {
		if app.Active() {
			log.Warnln("already running, ignoring")
			return nil
		}
		return c.startApp(app, conf)
	}

	if app.Active() {
		fmt.Println("anvil is currently running, restart to apply changes [y/N]")
		var resp string
		if _, err := fmt.Scanln(&resp); err != nil {
			log.Warnln("error reading response:", err)
			return nil
		}
		if resp != "y" && resp != "Y" {
			return nil
		}
		if err := app.Stop(ctx, false); err != nil {
			return fmt.Errorf("error stopping: %w", err)
		}
		for i := 0; i < 30 && app.Active(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}

	return c.startApp(app, conf)
}

func (c *startCmd) prepareConfig(app usecase.Application) (domain.Config, error) {
	var conf domain.Config
	ctx := context.Background()

	// start with current config if present
	current, err := app.LoadConfig(ctx)
	if err != nil {
		log.Warnln(fmt.Errorf("config load failed: %w", err))
		log.Warnln("reverting to default settings")
	}

	// handle legacy flags
	if c.LegacyKubernetes != nil {
		v := *c.LegacyKubernetes
		c.KubernetesEnabled = &v
	}
	if c.LegacyCPU > 0 && c.CPU == 0 {
		c.CPU = c.LegacyCPU
	}

	// convert cli flags to config
	if len(c.Mounts) > 0 {
		conf.Mounts = mountsFromFlag(c.Mounts)
	}
	if len(c.DNSHosts) > 0 {
		conf.Network.DNSHosts = dnsHostsFromFlag(c.DNSHosts)
	}
	if c.ActivateRuntime != nil {
		conf.ActivateRuntime = c.ActivateRuntime
	}
	if c.Binfmt != nil {
		conf.Binfmt = c.Binfmt
	}
	if c.KubernetesEnabled != nil {
		conf.Kubernetes.Enabled = *c.KubernetesEnabled
	}
	if c.KubernetesVersion != "" {
		conf.Kubernetes.Version = c.KubernetesVersion
	}
	if len(c.K3sArgs) > 0 {
		conf.Kubernetes.K3sArgs = c.K3sArgs
	} else {
		conf.Kubernetes.K3sArgs = defaultK3sArgs
	}
	if c.K3sListenPort > 0 {
		conf.Kubernetes.Port = c.K3sListenPort
	}

	// set flag-derived defaults
	c.setFlagDefaults()

	// if there is no existing settings, use template if enabled
	if usecase.ConfigEmpty(current) {
		templateUsed := false
		if c.Template == nil || *c.Template {
			if tmpl, err := app.ConfigService().LoadFromFile(templateFile(app)); err == nil {
				current = tmpl
				templateUsed = true
			}
		}
		if !templateUsed {
			conf = c.applyFlagValues(conf)
			c.setConfigDefaults(&conf, app)
			return conf, nil
		}
	}

	// set missing defaults in current config
	c.setConfigDefaults(&current, app)

	// carry over file-only settings
	conf.Docker = current.Docker
	conf.Provision = current.Provision

	// apply Docker-related CLI flags on top of current/file settings
	if len(c.RegistryMirrors) > 0 {
		if conf.Docker == nil {
			conf.Docker = make(map[string]any)
		}
		conf.Docker["registry-mirrors"] = c.RegistryMirrors
	}
	if len(c.InsecureRegistries) > 0 {
		if conf.Docker == nil {
			conf.Docker = make(map[string]any)
		}
		conf.Docker["insecure-registries"] = c.InsecureRegistries
	}

	// apply CLI flags
	conf = c.applyFlagValues(conf)

	// merge: use current settings for unchanged flags
	conf = c.mergeFromCurrent(conf, current)

	// apply remaining defaults
	c.setConfigDefaults(&conf, app)

	// apply fixed configs
	c.setFixedConfigs(&conf, app)

	// save if requested
	if c.SaveConfig != nil && *c.SaveConfig {
		if err := app.SaveConfig(ctx, conf); err != nil {
			return conf, fmt.Errorf("error preparing config file: %w", err)
		}
	}

	return conf, nil
}

func (c *startCmd) applyFlagValues(conf domain.Config) domain.Config {
	if c.Runtime != "" {
		conf.Runtime = c.Runtime
	}
	if c.CPU > 0 {
		conf.CPU = c.CPU
	}
	if c.CPUType != "" {
		conf.CPUType = c.CPUType
	}
	if c.Memory > 0 {
		conf.Memory = c.Memory
	}
	if c.Disk > 0 {
		conf.Disk = c.Disk
	}
	if c.RootDisk > 0 {
		conf.RootDisk = c.RootDisk
	}
	if c.Arch != "" {
		conf.Arch = c.Arch
	}
	if c.VMType != "" {
		conf.VMType = c.VMType
	}
	if c.Hostname != "" {
		conf.Hostname = c.Hostname
	}
	if c.DiskImage != "" {
		conf.DiskImage = c.DiskImage
	}
	if c.PortForwarder != "" {
		conf.PortForwarder = c.PortForwarder
	}
	if c.NestedVirtualization {
		conf.NestedVirtualization = true
	}
	if c.VZRosetta {
		conf.VZRosetta = true
	}
	if c.MountType != "" {
		conf.MountType = c.MountType
	}
	if c.MountINotify != nil {
		conf.MountINotify = *c.MountINotify
	}
	if c.ForwardAgent {
		conf.ForwardAgent = true
	}
	if c.SSHConfig != nil {
		conf.SSHConfig = *c.SSHConfig
	}
	if c.SSHPort > 0 {
		conf.SSHPort = c.SSHPort
	}
	if c.NetworkAddress {
		conf.Network.Address = true
	}
	if c.NetworkMode != "" {
		conf.Network.Mode = c.NetworkMode
	}
	if c.NetworkBridgeIf != "" {
		conf.Network.BridgeInterface = c.NetworkBridgeIf
	}
	if c.NetworkPreferredRoute {
		conf.Network.PreferredRoute = true
	}
	if c.NetworkHostAddresses {
		conf.Network.HostAddresses = true
	}
	if c.GatewayAddress != nil {
		conf.Network.GatewayAddress = c.GatewayAddress
	}
	if len(c.DNSResolvers) > 0 {
		conf.Network.DNSResolvers = c.DNSResolvers
	}
	if len(c.Env) > 0 {
		conf.Env = c.Env
	}
	if c.ActivateRuntime != nil {
		conf.ActivateRuntime = c.ActivateRuntime
	}
	if c.Binfmt != nil {
		conf.Binfmt = c.Binfmt
	}
	if len(c.RegistryMirrors) > 0 {
		if conf.Docker == nil {
			conf.Docker = make(map[string]any)
		}
		conf.Docker["registry-mirrors"] = c.RegistryMirrors
	}
	if len(c.InsecureRegistries) > 0 {
		if conf.Docker == nil {
			conf.Docker = make(map[string]any)
		}
		conf.Docker["insecure-registries"] = c.InsecureRegistries
	}
	return conf
}

func (c *startCmd) mergeFromCurrent(conf, current domain.Config) domain.Config {
	if c.Runtime == "" {
		conf.Runtime = current.Runtime
	}
	if c.CPU == 0 {
		conf.CPU = current.CPU
	}
	if c.CPUType == "" {
		conf.CPUType = current.CPUType
	}
	if c.Memory == 0 {
		conf.Memory = current.Memory
	}
	if c.Disk == 0 {
		conf.Disk = current.Disk
	}
	if c.RootDisk == 0 && current.RootDisk > 0 {
		conf.RootDisk = current.RootDisk
	}
	if c.Arch == "" {
		conf.Arch = current.Arch
	}
	if c.VMType == "" {
		conf.VMType = current.VMType
	}
	if c.Hostname == "" {
		conf.Hostname = current.Hostname
	}
	if c.DiskImage == "" {
		conf.DiskImage = current.DiskImage
	}
	if c.PortForwarder == "" {
		conf.PortForwarder = current.PortForwarder
	}
	if !c.NestedVirtualization {
		conf.NestedVirtualization = current.NestedVirtualization
	}
	if !c.VZRosetta {
		conf.VZRosetta = current.VZRosetta
	}
	if c.MountType == "" {
		conf.MountType = current.MountType
	}
	if c.MountINotify == nil {
		conf.MountINotify = current.MountINotify
	}
	if len(c.Mounts) == 0 {
		conf.Mounts = current.Mounts
	}
	if !c.ForwardAgent {
		conf.ForwardAgent = current.ForwardAgent
	}
	if c.SSHConfig == nil {
		conf.SSHConfig = current.SSHConfig
	}
	if c.SSHPort == 0 {
		conf.SSHPort = current.SSHPort
	}
	if !c.NetworkAddress {
		conf.Network.Address = current.Network.Address
	}
	if c.NetworkMode == "" {
		conf.Network.Mode = current.Network.Mode
	}
	if c.NetworkBridgeIf == "" {
		conf.Network.BridgeInterface = current.Network.BridgeInterface
	}
	if !c.NetworkPreferredRoute {
		conf.Network.PreferredRoute = current.Network.PreferredRoute
	}
	if !c.NetworkHostAddresses {
		conf.Network.HostAddresses = current.Network.HostAddresses
	}
	if c.GatewayAddress == nil {
		conf.Network.GatewayAddress = current.Network.GatewayAddress
	}
	if len(c.DNSResolvers) == 0 {
		conf.Network.DNSResolvers = current.Network.DNSResolvers
	}
	if len(c.DNSHosts) == 0 && len(current.Network.DNSHosts) > 0 {
		conf.Network.DNSHosts = current.Network.DNSHosts
	}
	if len(c.Env) == 0 {
		conf.Env = current.Env
	}
	if c.ActivateRuntime == nil {
		conf.ActivateRuntime = current.ActivateRuntime
	}
	if c.Binfmt == nil {
		conf.Binfmt = current.Binfmt
	}
	if c.KubernetesEnabled == nil {
		conf.Kubernetes.Enabled = current.Kubernetes.Enabled
	}
	if c.KubernetesVersion == "" && current.Kubernetes.Version != "" {
		conf.Kubernetes.Version = current.Kubernetes.Version
	}
	if len(c.K3sArgs) == 0 && len(current.Kubernetes.K3sArgs) > 0 {
		conf.Kubernetes.K3sArgs = current.Kubernetes.K3sArgs
	}
	if c.K3sListenPort == 0 && current.Kubernetes.Port > 0 {
		conf.Kubernetes.Port = current.Kubernetes.Port
	}
	if conf.ImageVersion == "" {
		conf.ImageVersion = current.ImageVersion
	}
	return conf
}

func (c *startCmd) setFlagDefaults() {
	if util.AtLeastVentura() {
		if c.VMType == "vz" && c.MountType == "" {
			c.MountType = defaultMountTypeVZ
		}
	}

	// convert mount type for qemu
	if c.VMType != "" && c.VMType != "vz" && c.VMType != "krunkit" && c.MountType == defaultMountTypeVZ {
		c.MountType = defaultMountTypeQEMU
	}
	// convert mount type for vz
	if c.VMType == "vz" && c.MountType == "9p" {
		c.MountType = defaultMountTypeVZ
	}

	// always enable nested virtualization for incus + vz if supported
	if util.SupportsNestedVirt() {
		if !c.NestedVirtualization && c.Runtime == domain.RuntimeIncus && c.VMType == "vz" {
			c.NestedVirtualization = true
		}
	}
}

func (c *startCmd) setConfigDefaults(conf *domain.Config, app usecase.Application) {
	if conf.Runtime == "" {
		conf.Runtime = domain.RuntimeDocker
	}
	if conf.VMType == "" {
		conf.VMType = environment.DefaultHypervisor()
		if err := util.RequireQemuImg(); err != nil && util.AtLeastVentura() {
			conf.VMType = "vz"
		}
	}
	if conf.MountType == "" {
		conf.MountType = defaultMountTypeQEMU
		if util.AtLeastVentura() && conf.VMType == "vz" {
			conf.MountType = defaultMountTypeVZ
		}
	} else {
		// convert incompatible mount types
		if conf.VMType != "vz" && conf.VMType != "krunkit" && conf.MountType == defaultMountTypeVZ {
			conf.MountType = defaultMountTypeQEMU
		}
		if conf.VMType == "vz" && conf.MountType == "9p" {
			conf.MountType = defaultMountTypeVZ
		}
	}
	if conf.Hostname == "" {
		conf.Hostname = app.ConfigService().Profile().ID
	}
	if conf.PortForwarder == "" {
		conf.PortForwarder = "ssh"
	}
	if conf.RootDisk == 0 {
		conf.RootDisk = defaultRootDisk
	}
}

func (c *startCmd) setFixedConfigs(conf *domain.Config, app usecase.Application) {
	svc := app.ConfigService()
	stateFile := svc.ProfileStateFile(svc.Profile())
	fixedConf, err := svc.LoadFromFile(stateFile)
	if err != nil {
		return
	}

	warnIfNotEqual := func(name, newVal, fixedVal string) {
		if newVal != fixedVal {
			log.Warnln(fmt.Errorf("'%s' cannot be updated after initial setup, discarded", name))
		}
	}

	if fixedConf.Arch != "" {
		warnIfNotEqual("architecture", conf.Arch, fixedConf.Arch)
		conf.Arch = fixedConf.Arch
	}
	if fixedConf.VMType != "" {
		warnIfNotEqual("virtual machine type", conf.VMType, fixedConf.VMType)
		conf.VMType = fixedConf.VMType
	}
	if fixedConf.Runtime != "" {
		warnIfNotEqual("runtime", conf.Runtime, fixedConf.Runtime)
		conf.Runtime = fixedConf.Runtime
	}
	if fixedConf.MountType != "" {
		warnIfNotEqual("volume mount type", conf.MountType, fixedConf.MountType)
		conf.MountType = fixedConf.MountType
	}
	if fixedConf.Network.Address && !conf.Network.Address {
		log.Warnln("network address cannot be disabled once enabled")
		conf.Network.Address = true
	}
	if fixedConf.Network.Mode != "" {
		warnIfNotEqual("network mode", conf.Network.Mode, fixedConf.Network.Mode)
		conf.Network.Mode = fixedConf.Network.Mode
	}
}

func (c *startCmd) editConfigFile(app usecase.Application) (domain.Config, error) {
	var conf domain.Config
	svc := app.ConfigService()

	profile := svc.Profile()
	profileFile := svc.ProfileFile(profile)
	currentFile, err := os.ReadFile(profileFile)
	if err != nil {
		return conf, fmt.Errorf("error reading config file: %w", err)
	}

	abort, err := embedded.ReadString("defaults/abort.yaml")
	if err != nil {
		log.Warnln(fmt.Errorf("unable to read embedded file: %w", err))
	}

	tmpFile, err := waitForUserEdit(c.Editor, []byte(abort+"\n"+string(currentFile)))
	if err != nil {
		return conf, fmt.Errorf("error editing config file: %w", err)
	}
	if tmpFile == "" {
		return conf, fmt.Errorf("empty file, startup aborted")
	}
	defer func() { _ = os.Remove(tmpFile) }()

	newConf, err := svc.LoadFromFile(tmpFile)
	if err != nil {
		return conf, fmt.Errorf("error in config file: %w", err)
	}

	if c.SaveConfig != nil && *c.SaveConfig {
		if err := svc.SaveConfig(context.Background(), newConf); err != nil {
			return conf, err
		}
	}
	return newConf, nil
}

func (c *startCmd) startApp(app usecase.Application, conf domain.Config) error {
	if err := app.Start(context.Background(), conf); err != nil {
		return err
	}
	if c.Foreground {
		return c.awaitForInterruption(app)
	}
	return nil
}

func (c *startCmd) awaitForInterruption(app usecase.Application) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	log.Println("keeping anvil in the foreground, press ctrl+c to exit...")

	sig := <-sigCh
	log.Infof("interrupted by: %v", sig)

	if err := app.Stop(context.Background(), false); err != nil {
		log.Errorf("error stopping: %v", err)
		return err
	}
	return nil
}
