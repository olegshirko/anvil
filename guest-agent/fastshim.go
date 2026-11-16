package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
)

// Fast-shim gating (stage 2): the fast shim is the default for eligible
// containers, the runc shim stays the fallback for everything else and on
// any fast-shim failure. Eligibility is decided once, at container-create
// time from the create request: containerd carries the runtime on the
// container record and the field is immutable, so there is no later decision
// point. ANVIL_FAST_SHIM=0 disables the fast shim wholesale. A fast-shim
// failure at task-create/start is never surfaced: startNativeTask recreates
// the record with the default runtime and retries (recreateWithDefaultRuntime
// below).

// fastShimRuntimeName is the runtime name containerd resolves for eligible
// containers. An absolute binary path is accepted as a runtime name and used
// as the shim binary directly (the initramfs installs it there).
const fastShimRuntimeName = "/bin/anvil-fast-shim"

// fastShimDisabled reports the wholesale kill switch: ANVIL_FAST_SHIM=0
// (or false/off) makes every container ineligible again.
func fastShimDisabled() bool {
	switch strings.ToLower(os.Getenv("ANVIL_FAST_SHIM")) {
	case "0", "false", "off":
		return true
	}
	return false
}

// fastShimEligible reports whether a container created with this request may
// run on the fast shim. Everything the fast shim does not implement yet
// keeps the runc shim, silently, per the builder-check resolution:
//   - privileged (device/permission model beyond the spec's own),
//   - custom --runtime (an explicit runtime request is honored by refusing
//     the fast shim; validateHostConfig refuses unknown runtimes outright),
//   - non-default seccomp profile (the built-in default and unconfined are
//     fine; a client-supplied JSON profile is not),
//   - --pid host and --pid container: (the init helper always creates a
//     fresh pid namespace for the container init; a host namespace cannot
//     be joined by the two-stage clone),
//   - subpath volume mounts (staging mountpoints re-armed per start),
//   - secondary networks (extra CNI endpoints beyond the first one).
func fastShimEligible(req dockerCreateRequest, meta *containerMeta) bool {
	if meta == nil || fastShimDisabled() {
		return false
	}
	hc := &req.HostConfig
	if hc.Privileged {
		return false
	}
	switch hc.Runtime {
	case "", "runc", "io.containerd.runc.v2":
	default:
		return false
	}
	if customSeccompProfile(hc.SecurityOpt) {
		return false
	}
	// The init helper is born inside a fresh pid namespace unconditionally
	// (clone(CLONE_NEWPID) is how the container init becomes PID 1), so any
	// pid-namespace-sharing mode must take the runc path: --pid host would
	// silently get a private namespace, --pid container: a new one instead
	// of the peer's.
	if hc.PidMode == "host" || strings.HasPrefix(hc.PidMode, "container:") {
		return false
	}
	if len(meta.SubpathMounts) > 0 {
		return false
	}
	if len(meta.Networks) > 1 {
		return false
	}
	return true
}

// customSeccompProfile reports a security-opt seccomp=<profile> whose profile
// is a client-supplied JSON document rather than the default profile or an
// explicit opt-out — the same parsing seccompSpecOpt applies at spec build.
func customSeccompProfile(securityOpts []string) bool {
	profile := ""
	for _, opt := range securityOpts {
		if v, ok := strings.CutPrefix(opt, "seccomp="); ok {
			profile = v
			continue
		}
		if v, ok := strings.CutPrefix(opt, "seccomp:"); ok {
			profile = v
		}
	}
	switch profile {
	case "", "builtin", "unconfined", "false":
		return false
	}
	return true
}

// withFastShim is the NewContainer opt pointing eligible containers at the
// fast shim.
func withFastShim() client.NewContainerOpts {
	return client.WithRuntime(fastShimRuntimeName, nil)
}

// usesFastShim reports whether the container record carries the fast shim.
func usesFastShim(ctx context.Context, c client.Container) bool {
	info, err := c.Info(ctx)
	if err != nil {
		return false
	}
	return info.Runtime.Name == fastShimRuntimeName
}

// newStartedTask creates the task for c and starts it. When the record
// carries the fast shim and either step fails, the record is recreated on
// the default runtime and the attempt is retried there — the fallback the
// builder-check resolution requires: the client never sees a fast-shim
// failure, only a start that took the slower path. Any error from the second
// (default-runtime) attempt is real and surfaces as before.
func newStartedTask(nsCtx context.Context, cl *client.Client, c client.Container, io cio.IO, tty bool, stdin *containerStdin, logPath string, tr *phaseTrace) (client.Task, error) {
	task, err := attemptNewStartedTask(nsCtx, c, io, tty, stdin, logPath, tr)
	if err == nil || !usesFastShim(nsCtx, c) {
		return task, err
	}
	debugLog("[fastshim] %s: task attempt failed (%v); falling back to the default shim", truncateID(c.ID()), err)
	c, rerr := recreateWithDefaultRuntime(nsCtx, cl, c)
	if rerr != nil {
		// The record is already gone: surface the recreate failure, the
		// original error alone would point at a container that no longer
		// exists.
		debugLog("[fastshim] %s: fallback recreate failed: %v", truncateID(c.ID()), rerr)
		return nil, fmt.Errorf("fast-shim fallback: %w (original failure: %v)", rerr, err)
	}
	return attemptNewStartedTask(nsCtx, c, io, tty, stdin, logPath, tr)
}

// attemptNewStartedTask is one task-create + start attempt: stale pid file
// out, task in, eof-marker pid posted, stdin handed to the shim, start.
func attemptNewStartedTask(nsCtx context.Context, c client.Container, io cio.IO, tty bool, stdin *containerStdin, logPath string, tr *phaseTrace) (client.Task, error) {
	// A previous attempt's (or run's) pid file must not survive into this
	// one: the logger would watch the old (dead) pid and write its marker
	// into the fresh log before this task ever ran.
	os.Remove(initPidPath(logPath)) //nolint:errcheck — stale file only
	task, err := c.NewTask(nsCtx, func(string) (cio.IO, error) {
		return io, nil
	})
	tr.mark("new-task")
	if err != nil {
		return nil, fmt.Errorf("new task: %w", err)
	}
	// Post the init pid for the task logger's eof marker — the pid comes
	// with the create RPC, before the init even runs (non-TTY only: TTY
	// containers keep the timer-based follow end to end).
	if !tty {
		writeInitPidFile(logPath, int(task.Pid()))
	}
	if stdin != nil {
		stdin.taskCreated() // the shim holds the FIFO now
	}
	if serr := task.Start(nsCtx); serr != nil {
		task.Delete(context.WithoutCancel(nsCtx)) //nolint:errcheck — namespaced: a bare context is rejected and leaks the record
		return nil, fmt.Errorf("start task: %w", serr)
	}
	return task, nil
}

// recreateWithDefaultRuntime rebuilds the container record on the default
// (runc) runtime, keeping everything else: the existing rootfs snapshot (no
// WithSnapshotCleanup on the delete), image, labels and the built spec.
// Used when a fast-shim task fails to create or start — the client never
// sees the fallback, it only costs a second attempt on the proven path.
func recreateWithDefaultRuntime(ctx context.Context, cl *client.Client, c client.Container) (client.Container, error) {
	info, err := c.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("container info: %w", err)
	}
	spec, err := c.Spec(ctx)
	if err != nil {
		return nil, fmt.Errorf("container spec: %w", err)
	}
	// A failed task attempt may have left a record (start failure); the old
	// task goes first, as in startNativeTask's stale-task cleanup.
	if t, terr := c.Task(ctx, nil); terr == nil {
		tctx, tcancel := context.WithTimeout(ctx, 5*time.Second)
		t.Delete(tctx, client.WithProcessKill) //nolint:errcheck — best effort
		tcancel()
	}
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	if derr := c.Delete(dctx); derr != nil {
		dcancel()
		return nil, fmt.Errorf("delete container record: %w", derr)
	}
	dcancel()
	opts := []client.NewContainerOpts{
		client.WithSnapshotter(info.Snapshotter),
		client.WithSnapshot(info.SnapshotKey),
		client.WithContainerLabels(info.Labels),
		client.WithSpec(spec),
	}
	if info.Image != "" {
		opts = append(opts, client.WithImageName(info.Image))
	}
	return cl.NewContainer(ctx, info.ID, opts...)
}
