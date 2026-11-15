// anvil-fast-shim is a lightweight containerd runtime-v2 shim. It implements
// the shim TaskService contract (Create/Start/Wait/Kill/Delete/exec/stats)
// without runc: the container init is created with a single clone3 into a
// prepared cgroup (CLONE_INTO_CGROUP), the rootfs mounts prepared by the
// agent are applied by a small init helper, and the user process is exec'd
// as PID 1 of the container's pid namespace.
//
// The wire protocol is the standard shim v2 one: containerd starts the
// binary with action "start" and a BootstrapParams blob on stdin; we fork
// ourselves into a long-running server that serves the ttrpc TaskService on
// a unix socket under /run/containerd/s and answer with a BootstrapResult.
// The agent selects this binary per container via containerd's WithRuntime
// (an absolute binary path is accepted as the runtime name).
//
// Linux-only at runtime (clone3, namespaces, cgroup v2); the process
// scaffolding here is portable so the package still builds for darwin
// (go vet / go test on the dev Mac).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	apitypes "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
)

// shimName identifies the shim in logs and runtime info. The binary is
// invoked by containerd via its absolute path, so the name is informational.
const shimName = "anvil-fast-shim"

func main() {
	// The helper processes exec this binary before the user process starts;
	// they bypass the shim server flow entirely.
	if isInitHelper() {
		os.Exit(runInit(helperArg("id"), helperArg("bundle")))
	}
	if isInitApply() {
		os.Exit(runInitApply(helperArg("id"), helperArg("bundle")))
	}
	if isExecHelper() {
		os.Exit(runExecHelper(helperArg("initpid"), helperArg("bundle")))
	}
	// NoReaper: every child here (init helpers, exec helpers) is joined by
	// its own exec.Cmd watcher; the shared reaper would steal the waitpid.
	shim.RunShim(context.Background(), NewManager(shimName), func(cfg *shim.Config) {
		cfg.NoReaper = true
	})
}

// helperArg reads "-<name> value" from the helper invocation argv.
func helperArg(name string) string {
	args := os.Args[2:]
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "-"+name {
			return args[i+1]
		}
	}
	return ""
}

// NewManager returns the shim manager (the process-level shim.Shim).
func NewManager(name string) shim.Shim {
	return &manager{name: name}
}

type manager struct {
	name string
}

func (m *manager) Name() string {
	return m.name
}

// Info answers containerd's runtime-info probe.
func (m *manager) Info(ctx context.Context, optionsR io.Reader) (*apitypes.RuntimeInfo, error) {
	return &apitypes.RuntimeInfo{
		Name: shimName,
		Version: &apitypes.RuntimeVersion{
			Version: "1",
		},
	}, nil
}

// Start is the "start" action of the shim binary (already inside RunShim's
// flag/stdin handling): fork the long-running server process that serves the
// ttrpc TaskService, hand it the pre-bound listener on fd 3, and answer with
// the bootstrap address. Mirrors the runc shim's manager without grouping or
// runc options.
func (m *manager) Start(ctx context.Context, opts *bootapi.BootstrapParams) (_ *bootapi.BootstrapResult, retErr error) {
	params := &bootapi.BootstrapResult{
		Version:  3,
		Protocol: "ttrpc",
	}

	id := opts.GetInstanceID()
	if id == "" {
		return nil, fmt.Errorf("bootstrap params carry no instance id")
	}

	cmd, err := serverCommand(ctx, id, opts.GetContainerdGrpcAddress(), opts.GetLogLevel() <= bootapi.LogLevel_LOG_LEVEL_DEBUG)
	if err != nil {
		return nil, err
	}

	socketDir := opts.GetSocketDir()
	if socketDir == "" {
		socketDir = filepath.Join(defaults.DefaultStateDir, "s")
	}
	address, err := shim.CreateSocketAddress(ctx, socketDir, opts.GetContainerdGrpcAddress(), id, false)
	if err != nil {
		return nil, err
	}
	listener, err := shim.NewSocket(address)
	if err != nil {
		// A connectable socket for this id means an existing shim instance
		// already serves the container (containerd's grouping): point at it
		// instead of failing. A stale one is removed and rebound.
		if !shim.SocketEaddrinuse(err) {
			return nil, fmt.Errorf("create shim socket: %w", err)
		}
		if shim.CanConnect(address) {
			params.Address = address
			return params, nil
		}
		if err := shim.RemoveSocket(address); err != nil {
			return nil, fmt.Errorf("remove stale shim socket: %w", err)
		}
		if listener, err = shim.NewSocket(address); err != nil {
			return nil, fmt.Errorf("re-create shim socket: %w", err)
		}
	}
	defer func() {
		if retErr != nil {
			listener.Close()
			shim.RemoveSocket(address) //nolint:errcheck — cleanup of a failed start
		}
	}()

	f, err := listener.File()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start shim server: %w", err)
	}
	go cmd.Wait() //nolint:errcheck — the server logs its own failures

	// The container should outlive the shim's own memory pressure, like the
	// runc shim does.
	if err := shim.AdjustOOMScore(cmd.Process.Pid); err != nil {
		log.G(ctx).WithError(err).Warn("failed to adjust shim oom score")
	}

	params.Address = address
	return params, nil
}

// serverCommand builds the self re-exec of the long-running server: same
// binary, no action, flags identifying the shim instance. The ttrpc server
// then serves on the inherited listener (fd 3).
func serverCommand(ctx context.Context, id, containerdAddress string, debug bool) (*exec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-namespace", ns,
		"-id", id,
		"-address", containerdAddress,
	}
	if debug {
		args = append(args, "-debug")
	}
	cmd := exec.Command(self, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GOMAXPROCS=2")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

// Stop is the "delete" action: best-effort cleanup of a shim whose server
// process died without a proper TaskService Delete (crash, kill -9). It runs
// with cwd = the bundle directory. The regular teardown lives in the task
// service; this only unmounts the rootfs and drops the cgroup so a dead
// container's resources do not leak.
func (m *manager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	return cleanupDeadShim(ctx, id)
}

// init registers the TaskService plugin with pkg/shim's plugin graph. The
// graph also provides the event publisher (ttrpc back to containerd) and the
// shutdown service this service depends on — the same pattern the runc shim
// and containerd's example shim use.
func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			plugins.EventPlugin,
			plugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			pp, err := ic.GetByID(plugins.EventPlugin, "publisher")
			if err != nil {
				return nil, err
			}
			ss, err := ic.GetByID(plugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			return newTaskService(ic.Context, pp.(shim.Publisher), ss.(shutdown.Service))
		},
	})
}
