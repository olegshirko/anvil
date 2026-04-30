package lima

import (
	"context"
	"fmt"
	"time"

	"anvil/internal/domain"
	"anvil/internal/environment/container/incus"
	"anvil/internal/environment/vm/lima/limaconfig"
	"anvil/internal/service"
	"anvil/internal/service/process/inotify"
	"anvil/internal/service/process/vmnet"
	"anvil/internal/util"
)

// ensureHostServices prepares background host-side services (network and
// filesystem watchers) before the VM starts.
func (l *limaVM) ensureHostServices(ctx context.Context, conf domain.Config) context.Context {
	requiresVmnet := conf.VMType == limaconfig.QEMU || conf.Runtime == incus.Name || conf.Network.Mode == "bridged"
	conf.Network.Address = conf.Network.Address && requiresVmnet

	if !util.IsMacOS() || (!conf.MountINotify && !conf.Network.Address) {
		return ctx
	}

	pipeline := l.Init(ctx)
	log := pipeline.Logger()

	var netDepsOk bool
	var svcHealth service.HealthReport

	vmnetCtxKey := service.ContextKey(vmnet.Name)
	inotifyCtxKey := service.ContextKey(inotify.Name)

	if requiresVmnet {
		pipeline.Add(func() error {
			if conf.Network.Address {
				pipeline.Stage("preparing network")
				ctx = context.WithValue(ctx, vmnetCtxKey, true)
			}
			deps, root := l.svcMgr.FindDeps(ctx, conf, vmnet.Name)
			if deps.Present() {
				netDepsOk = true
				return nil
			}
			if root {
				log.Println("configuring reachable IP address")
				log.Println("sudo password may be required")
			}
			if err := deps.Apply(l.host); err != nil {
				netDepsOk = false
				return err
			}
			netDepsOk = true
			return nil
		})
	}

	if conf.MountINotify {
		pipeline.Add(func() error {
			ctx = context.WithValue(ctx, inotifyCtxKey, true)
			deps, _ := l.svcMgr.FindDeps(ctx, conf, inotify.Name)
			if err := deps.Apply(l.host); err != nil {
				return fmt.Errorf("inotify setup failed: %w", err)
			}
			return nil
		})
	}

	pipeline.Add(func() error {
		return l.svcMgr.Launch(ctx, conf)
	})

	if conf.Network.Address || conf.MountINotify {
		pipeline.Retry("", 200*time.Millisecond, 75, func(_ int) error {
			var err error
			svcHealth, err = l.svcMgr.CheckHealth(ctx, conf)
			if err != nil {
				return err
			}
			if !svcHealth.Active {
				return fmt.Errorf("host services not running")
			}
			for _, p := range svcHealth.Processes {
				if !p.Running {
					return p.Error
				}
			}
			return nil
		})
	}

	if err := pipeline.Exec(); err != nil {
		if !requiresVmnet {
			return ctx
		}
		switch {
		case !netDepsOk:
			log.Warnln("network dependency setup failed:", err)
		case !svcHealth.Active:
			log.Warnln("network daemon failed to start:", err)
		default:
			for _, p := range svcHealth.Processes {
				if p.Name == inotify.Name {
					continue
				}
				if !p.Running {
					ctx = context.WithValue(ctx, service.ContextKey(p.Name), false)
					log.Warnf("%s failed to start: %v", p.Name, err)
				}
			}
		}
	}

	if conf.MountINotify {
		if ok, _ := ctx.Value(inotifyCtxKey).(bool); !ok {
			log.Warnln("inotify daemon could not be enabled")
		}
	}

	if active, _ := ctx.Value(vmnetCtxKey).(bool); active {
		l.host = l.host.WithEnv(vmnet.SubProcessEnvVar + "=1")
	}

	return ctx
}
