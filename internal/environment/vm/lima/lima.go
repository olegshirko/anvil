package lima

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"anvil/internal/cli"
	"anvil/internal/core"
	"anvil/internal/domain"
	"anvil/internal/environment"
	"anvil/internal/environment/vm/lima/limaconfig"
	"anvil/internal/environment/vm/lima/limautil"
	"anvil/internal/service"
	"anvil/internal/store"
	"anvil/internal/usecase" // Added import
	"anvil/internal/util"
	"anvil/internal/util/osutil"
	"anvil/internal/util/yamlutil"

	"github.com/sirupsen/logrus"
)

// New creates a new virtual machine.
func New(host environment.HostActions, configService usecase.ConfigService, configRepo usecase.ConfigRepository, configHelper *usecase.ConfigHelper) environment.VirtualMachine {
	// lima config directory
	limaHome := configService.LimaDir()

	// environment variables for the subprocesses
	var envs []string
	envHome := limautil.EnvLimaHome + "=" + limaHome
	envLimaInstance := envLimaInstance + "=" + configService.Profile().ID
	envBinary := osutil.BinaryEnv + "=" + osutil.SelfPath()
	envs = append(envs, envHome, envLimaInstance, envBinary)

	// consider making this truly flexible to support other VMs
	return &limaVM{
		host:          host.WithEnv(envs...),
		limaHome:      limaHome,
		configService: configService,
		CommandChain:  cli.New("vm"),
		svcMgr:        service.NewSupervisor(host, configHelper, configService.Profile().ShortName, configService.ProfileConfigDir(configService.Profile())),
	}
}

const (
	envLimaInstance = "LIMA_INSTANCE"
	lima            = "lima"
	limactl         = limautil.LimactlCommand
)

var _ environment.VirtualMachine = (*limaVM)(nil)

type limaVM struct {
	host environment.HostActions
	cli.CommandChain

	// keep config in case of restart
	conf domain.Config

	// lima config
	limaConf limaconfig.Config

	// lima config directory
	limaHome string

	// network between host and the vm
	svcMgr service.Supervisor

	// config service replaces global config package
	configService usecase.ConfigService

	// kernel path for direct kernel boot (QEMU only)
	kernelPath string
}

func (l limaVM) Dependencies() []string {
	return []string{
		"lima",
	}
}

func (l *limaVM) Start(ctx context.Context, conf domain.Config) error {
	a := l.Init(ctx)

	l.prepareHost(conf)

	if l.Created() {
		return l.resume(ctx, conf)
	}

	a.Add(func() error {
		ctx = l.ensureHostServices(ctx, conf)
		return nil
	})

	a.Stage("creating and starting")
	confFile := filepath.Join(os.TempDir(), l.configService.Profile().ID+".yaml")

	a.Add(func() error {
		l.downloadKernel(conf.Runtime, environment.Arch(conf.Arch).Value(), conf.ImageVersion)
		return nil
	})

	a.Add(func() (err error) {
		l.limaConf, err = buildLimaConfig(ctx, conf, l.configService.Profile(), l.configService.CacheDir(), l.configService.ProfileConfigDir(l.configService.Profile()))
		return err
	})

	a.Add(l.assertQemu)

	a.Add(func() error {
		var diskErr, imageErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			diskErr = l.createRuntimeDisk(conf)
		}()
		go func() {
			defer wg.Done()
			imageErr = l.downloadDiskImage(conf)
		}()
		wg.Wait()
		if diskErr != nil {
			return diskErr
		}
		return imageErr
	})

	a.Add(func() error {
		return yamlutil.MarshalToFile(l.limaConf, confFile)
	})

	a.Add(func() error { return l.writeNetworkFile(conf) })

	a.Add(l.removeStaleSockets)

	a.Add(func() error {
		return l.host.Run(limactl, "start", "--tty=false", confFile)
	})
	a.Add(func() error {
		return os.Remove(confFile)
	})

	// adding it to command chain to execute only after successful startup.
	a.Add(func() error {
		l.conf = conf
		return nil
	})

	l.addPostStartActions(ctx, a, conf)

	return a.Exec()
}

func (l *limaVM) resume(ctx context.Context, conf domain.Config) error {
	log := l.Logger(ctx)
	a := l.Init(ctx)

	if l.Running(ctx) {
		log.Println("already running")
		return nil
	}

	a.Add(func() error {
		ctx = l.ensureHostServices(ctx, conf)
		return nil
	})

	a.Add(func() (err error) {
		// disk must be resized before starting
		conf = l.syncDiskSize(ctx, conf)
		l.conf = conf

		l.downloadKernel(conf.Runtime, environment.Arch(conf.Arch).Value(), conf.ImageVersion)

		l.limaConf, err = buildLimaConfig(ctx, conf, l.configService.Profile(), l.configService.CacheDir(), l.configService.ProfileConfigDir(l.configService.Profile()))
		return err
	})

	a.Add(l.assertQemu)

	a.Add(func() error {
		return l.useRuntimeDisk(conf)
	})

	a.Add(l.setDiskImage)

	a.Add(func() error {
		l.setKernelImage()
		return nil
	})

	a.Add(func() error {
		err := yamlutil.MarshalToFile(l.limaConf, l.configService.ProfileLimaFile(l.configService.Profile()))
		return err
	})

	a.Add(func() error { return l.writeNetworkFile(conf) })

	a.Add(l.removeStaleSockets)

	a.Stage("starting")
	a.Add(func() error {
		return l.host.Run(limactl, "start", l.configService.Profile().ID)
	})

	l.addPostStartActions(ctx, a, conf)

	return a.Exec()
}

func (l limaVM) Running(_ context.Context) bool {
	i, err := limautil.Instance(l.configService.Profile().ID)
	if err != nil {
		logrus.Tracef("error retrieving running instance: %v", err)
		return false
	}
	return i.Running()
}

func (l limaVM) Stop(ctx context.Context, force bool) error {
	log := l.Logger(ctx)
	a := l.Init(ctx)
	if !l.Running(ctx) && !force {
		log.Println("not running")
		return nil
	}

	a.Stage("stopping")

	if util.IsMacOS() {
		conf, _ := limautil.LoadInstance(l.configService.Profile(), l.configService.ProfileStateFile(l.configService.Profile()))
		a.Retry("", time.Second*1, 10, func(retryCount int) error {
			err := l.svcMgr.Shutdown(ctx, conf)
			if err != nil {
				err = cli.MarkNonFatal(err)
			}
			return err
		})
	}

	a.Add(l.removeHostAddresses)

	a.Add(func() error {
		if force {
			return l.host.Run(limactl, "stop", "--force", l.configService.Profile().ID)
		}
		return l.host.Run(limactl, "stop", l.configService.Profile().ID)
	})

	return a.Exec()
}

func (l limaVM) Teardown(ctx context.Context) error {
	a := l.Init(ctx)

	if util.IsMacOS() {
		conf, _ := limautil.LoadInstance(l.configService.Profile(), l.configService.ProfileStateFile(l.configService.Profile()))
		a.Retry("", time.Second*1, 10, func(retryCount int) error {
			return l.svcMgr.Shutdown(ctx, conf)
		})
	}

	a.Add(func() error {
		return l.host.Run(limactl, "delete", "--force", l.configService.Profile().ID)
	})

	a.Add(l.removeStaleSockets)

	return a.Exec()
}

func (l limaVM) removeStaleSockets() error {
	profileDir := l.configService.ProfileConfigDir(l.configService.Profile())
	sockets := []string{
		filepath.Join(profileDir, "docker.sock"),
		filepath.Join(profileDir, "containerd.sock"),
		filepath.Join(profileDir, "buildkitd.sock"),
		filepath.Join(profileDir, "incus.sock"),
	}
	for _, sock := range sockets {
		_ = l.host.RunQuiet("rm", "-f", sock)
	}
	if l.configService.Profile().ShortName == "default" {
		_ = l.host.RunQuiet("rm", "-f", filepath.Join(filepath.Dir(profileDir), "docker.sock"))
	}
	return nil
}

func (l limaVM) Restart(ctx context.Context) error {
	if usecase.ConfigEmpty(l.conf) { // Changed line
		return fmt.Errorf("cannot restart, VM not previously started")
	}

	if err := l.Stop(ctx, false); err != nil {
		return err
	}

	// poll until VM is fully stopped to prevent race condition
	for i := 0; i < 30 && l.Running(ctx); i++ {
		time.Sleep(100 * time.Millisecond)
	}

	if err := l.Start(ctx, l.conf); err != nil {
		return err
	}

	return nil
}

func (l limaVM) Host() environment.HostActions {
	return l.host
}

func (l *limaVM) Guest() environment.GuestActions {
	return l
}

func (l limaVM) Env(s string) (string, error) {
	ctx := context.Background()
	if !l.Running(ctx) {
		return "", fmt.Errorf("not running")
	}
	return l.RunOutput("echo", "$"+s)
}

func (l limaVM) Created() bool {
	stat, err := os.Stat(l.configService.ProfileLimaFile(l.configService.Profile()))
	return err == nil && !stat.IsDir()
}

func (l limaVM) User() (string, error) {
	return l.RunOutput("whoami")
}

func (l limaVM) Arch() environment.Arch {
	a, _ := l.RunOutput("uname", "-m")
	return environment.Arch(a)
}

func (l *limaVM) addPostStartActions(ctx context.Context, a *cli.ActiveCommandChain, conf domain.Config) {
	// If the user didn't specify custom DNS resolvers, pull the host's DNS
	// servers so the guest can use them directly instead of relying on Lima's
	// hostResolver DNS proxy which can drop or timeout UDP packets.
	if len(conf.Network.DNSResolvers) == 0 {
		conf.Network.DNSResolvers = util.HostDNSResolvers()
	}

	// guest-side fatal operations (parallel)
	a.Add(func() error {
		var wg sync.WaitGroup
		var dnsErr, certErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := l.setupDNS(conf); err != nil {
				dnsErr = fmt.Errorf("error setting up DNS: %w", err)
			}
		}()
		go func() {
			defer wg.Done()
			certErr = l.copyCerts()
		}()
		wg.Wait()
		if dnsErr != nil {
			return dnsErr
		}
		return certErr
	})

	// guest-side non-fatal operations (parallel)
	a.Add(func() error {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// use binfmt when emulation is disabled i.e. host arch
			if conf.Binfmt != nil && *conf.Binfmt {
				if arch := environment.HostArch(); arch == environment.Arch(conf.Arch).Value() {
					if err := core.ConfigureBinfmt(l.host, l, environment.Arch(conf.Arch)); err != nil {
						logrus.Warnf("unable to enable qemu %s emulation: %v", arch, err)
					}
				}
			}

			if l.limaConf.VMOpts.VZOpts.Rosetta.Enabled {
				// enable rosetta
				err := l.Run("sudo", "sh", "-c", `stat /proc/sys/fs/binfmt_misc/rosetta || echo ':rosetta:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:/mnt/lima-rosetta/rosetta:OCF' > /proc/sys/fs/binfmt_misc/register`)
				if err != nil {
					logrus.Warnf("unable to enable rosetta: %v", err)
					return
				}

				// disable qemu
				if err := l.RunQuiet("stat", "/proc/sys/fs/binfmt_misc/qemu-x86_64"); err == nil {
					err = l.Run("sudo", "sh", "-c", `echo 0 > /proc/sys/fs/binfmt_misc/qemu-x86_64`)
					if err != nil {
						logrus.Warnf("unable to disable qemu x86_84 emulation: %v", err)
					}
				}
			}
		}()
		go func() {
			defer wg.Done()
			if err := l.replicateHostAddresses(conf); err != nil {
				logrus.Warnln("unable to assign host IP addresses to the VM:", err)
			}
		}()
		wg.Wait()
		return nil
	})

	// host-side persistence (parallel)
	a.Add(func() error {
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			if err := l.configService.SaveConfig(ctx, l.conf); err != nil {
				logrus.Warnln("error persisting anvil state:", err)
			}
		}()
		go func() {
			defer wg.Done()
			stateFile := l.configService.ProfileStateFile(l.configService.Profile())
			if err := l.configService.SaveTemplate(l.conf, stateFile); err != nil {
				logrus.Warnln("error persisting anvil state to lima directory:", err)
			}
		}()
		go func() {
			defer wg.Done()
			if len(l.limaConf.AdditionalDisks) == 0 {
				return
			}
			// startup is successful
			// if additional disk is present, then it must've been formatted correctly.
			if err := store.Mutate(l.configService.ProfileStoreFile(l.configService.Profile()), func(s *store.State) {
				s.DiskFormatted = true
			}); err != nil {
				// not fatal, but should be logged
				logrus.Warnln(fmt.Errorf("error persisting store settings: %v", err))
			}
		}()
		wg.Wait()
		return nil
	})

}

func (l *limaVM) assertQemu() error {
	// assert qemu requirement
	sameArchitecture := environment.HostArch() == l.limaConf.Arch
	if err := util.RequireQemuImg(); err != nil && l.limaConf.VMType == limaconfig.QEMU {
		if !sameArchitecture {
			return fmt.Errorf("qemu is required to emulate %s: %w", l.limaConf.Arch, err)
		}
		return err
	}
	return nil
}

const envLimaSSHPortForwarder = "LIMA_SSH_PORT_FORWARDER"

func (l *limaVM) prepareHost(conf domain.Config) {
	useSSHPortForwarder := conf.PortForwarder != "grpc"

	l.host = l.host.WithEnv(envLimaSSHPortForwarder + "=" + strconv.FormatBool(useSSHPortForwarder))
}
