//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
)

// Task lifecycle: Create mounts the rootfs snapshots, prepares the cgroup,
// wires the io and starts the init helper (a fresh process: plain fork+exec,
// the only fork shape that is safe around a Go runtime). The helper joins
// the namespaces, moves into the cgroup and applies the spec mounts before
// it reports ready, so Start costs a gate write, one fork+execve window and
// a pipe read. Everything below assumes that split.

// Create prepares the container world and holds the init at its gate.
func (s *taskService) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	s.mu.Lock()
	if _, exists := s.containers[r.ID]; exists {
		s.mu.Unlock()
		return nil, errgrpc.ToGRPC(errdefs.ErrAlreadyExists)
	}
	s.mu.Unlock()

	bundle := r.Bundle
	if bundle == "" {
		bundle = s.bundlePath
	}
	spec, err := readSpecFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrInvalidArgument, "read spec: %v", err)
	}
	if spec.Process == nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrInvalidArgument, "spec has no process")
	}

	// Everything acquired below must be undone on failure, in order.
	var cleanups []func()
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	fail := func(err error) (*taskAPI.CreateTaskResponse, error) {
		cleanup()
		return nil, errgrpc.ToGRPC(err)
	}

	// Rootfs: mount the snapshot layers onto <bundle>/rootfs, like the runc
	// shim does before runc create pivots into it.
	rootfs := filepath.Join(bundle, "rootfs")
	if err := os.Mkdir(rootfs, 0o711); err != nil && !os.IsExist(err) {
		return fail(err)
	}
	var snapshotMounts []mount.Mount
	for _, m := range r.Rootfs {
		snapshotMounts = append(snapshotMounts, mount.Mount{
			Type:    m.Type,
			Source:  m.Source,
			Target:  m.Target,
			Options: m.Options,
		})
	}
	if err := mount.All(snapshotMounts, rootfs); err != nil {
		return fail(fmt.Errorf("mount rootfs: %w", err))
	}
	cleanups = append(cleanups, func() {
		if err := mount.UnmountMounts(snapshotMounts, rootfs, 0); err != nil {
			log.G(ctx).WithError(err).Warn("unmount rootfs after failed create")
		}
	})

	// Cgroup v2 dir with the spec's limits; the init helper moves into it
	// and the container init it creates inherits the placement.
	cgPath, err := prepareCgroup(spec)
	if err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, func() { removeCgroup(cgPath) })

	// IO plumbing (binary-v2 logger spawn / fifo ends / console pty).
	terminal := r.Terminal || spec.Process.Terminal
	ioSet, err := newStdioSet(ctx, r.ID, r.Stdin, r.Stdout, r.Stderr, terminal)
	if err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, func() { ioSet.close() })

	// Gate (shim -> helper) and report (helper -> shim) pipes; kept ends
	// stay with the container.
	gateR, gateW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	reportR, reportW, err := os.Pipe()
	if err != nil {
		gateW.Close()
		gateR.Close()
		return fail(err)
	}

	self, err := os.Executable()
	if err != nil {
		gateW.Close()
		gateR.Close()
		reportR.Close()
		reportW.Close()
		return fail(err)
	}
	// The init helper: fresh process, io on fds 5/6/7, gate/report on 3/4.
	cmd := exec.Command(self, "init", "-id", r.ID, "-bundle", bundle)
	cmd.ExtraFiles = []*os.File{gateR, reportW, ioSet.childStdin, ioSet.childStdout, ioSet.childStderr}
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		gateW.Close()
		gateR.Close()
		reportR.Close()
		reportW.Close()
		return fail(fmt.Errorf("start init helper: %w", err))
	}
	// The child holds its copies now; parent drops the child-only ends.
	gateR.Close()
	reportW.Close()

	// Wait for the helper's setup report: "P <pid>" (the init process was
	// born and holds the gate) then "R" (mounts applied) — or "E <error>".
	// The watchdog kills a wedged helper; the pipe read then returns with
	// EOF.
	reportBuf := bufio.NewReader(reportR)
	watchdog := time.AfterFunc(15*time.Second, func() { cmd.Process.Kill() }) //nolint:errcheck
	var initPid int
	for {
		pid, ready, rerr := consumeSetupReport(reportBuf)
		if rerr != nil {
			watchdog.Stop()
			cmd.Process.Kill() //nolint:errcheck — a stuck helper must not leak
			cmd.Wait()         //nolint:errcheck — join it
			gateW.Close()
			reportR.Close()
			return fail(fmt.Errorf("init setup failed: %s", rerr.Error()))
		}
		if pid > 0 {
			initPid = pid
			continue
		}
		if ready {
			break
		}
	}
	watchdog.Stop()

	c := &container{
		id:        r.ID,
		bundle:    bundle,
		spec:      spec,
		term:      terminal,
		stdin:     r.Stdin,
		stdout:    r.Stdout,
		stderr:    r.Stderr,
		stdinOpen: r.Stdin != "",
		pid:       initPid,
		status:    task.Status_CREATED,
		waitCh:    make(chan struct{}),
		gateW:     gateW,
		reportR:   reportR,
		reportBuf: reportBuf,
		cgPath:    cgPath,
		io:        ioSet,
		cmd:       cmd,
		startedCh: make(chan startMsg, 1),
		oomStop:   make(chan struct{}),
		execs:     map[string]*execProcess{},
	}

	s.mu.Lock()
	s.containers[r.ID] = c
	s.mu.Unlock()

	// TaskOOM parity with the runc shim: watch the cgroup's memory.events
	// for oom_kill increases from here until delete.
	go s.watchOOM(c)

	s.send(&eventstypes.TaskCreate{
		ContainerID: r.ID,
		Bundle:      bundle,
		Rootfs:      r.Rootfs,
		IO: &eventstypes.TaskIO{
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: terminal,
		},
	})

	// Console copies start with the create (output only flows after start,
	// but the goroutines own the io lifecycle).
	ioSet.startCopies(ctx)

	// The report reader owns the report pipe from here on: start pid, exit
	// code, EOF fallback.
	go s.readReports(c)

	// The init process is alive (holding the gate), so the create response
	// carries its global pid — the runc shim's contract: containerd's task
	// pid, the agent's eof-marker pid file and created-state kill all key
	// off it.
	return &taskAPI.CreateTaskResponse{Pid: uint32(initPid)}, nil
}

// Start releases the created init from its gate. The mount work already
// happened at create; the helper applies the exec attributes, forks the
// container init (PID 1 of the namespace) and execs the user process.
func (s *taskService) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	if r.ExecID != "" {
		return s.startExec(ctx, r)
	}
	return s.startInit(ctx, c)
}

func (s *taskService) startInit(ctx context.Context, c *container) (*taskAPI.StartResponse, error) {
	s.mu.Lock()
	if c.status != task.Status_CREATED {
		st := c.status
		s.mu.Unlock()
		if st == task.Status_RUNNING {
			return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "process %s already started", c.id)
		}
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "process %s is not created", c.id)
	}
	s.mu.Unlock()

	// Release the gate; the helper creates the init and reports back.
	if _, err := c.gateW.WriteString(gateStartLine); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	var msg startMsg
	select {
	case msg = <-c.startedCh:
	case <-ctx.Done():
		return nil, errgrpc.ToGRPC(context.Canceled)
	case <-time.After(10 * time.Second):
		return nil, errgrpc.ToGRPCf(errdefs.ErrUnknown, "start timed out waiting for the init")
	}
	if msg.err != nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrUnknown, "failed to start: %v", msg.err)
	}
	s.mu.Lock()
	// A container that ran faster than this RPC keeps its STOPPED status:
	// its exit may already have been recorded by readReports.
	if !c.exited {
		c.status = task.Status_RUNNING
	}
	c.pid = msg.pid
	c.started = true
	pending, pid, code, at := c.exitPending, c.pid, c.exitCode, c.exitedAt
	c.exitPending = false
	s.mu.Unlock()
	// The runc shim publishes TaskStart; without it containerd never sees
	// the running transition and the agent's docker "start" event dies the
	// moment fast-shim containers are wired. Order matters: TaskStart goes
	// out before any held-back TaskExit.
	s.send(&eventstypes.TaskStart{
		ContainerID: c.id,
		Pid:         uint32(msg.pid),
	})
	if pending {
		publishTaskExit(s, c.id, "", pid, code, at)
	}
	log.G(ctx).WithFields(log.Fields{"id": c.id, "pid": msg.pid}).Debug("container init created")
	return &taskAPI.StartResponse{Pid: uint32(msg.pid)}, nil
}

// readReports consumes the helper's report lines for the life of the
// container: the start pid, the exit code, and EOF (helper gone — fall back
// to its own exit status).
func (s *taskService) readReports(c *container) {
	var started bool
	for {
		line, err := c.reportBuf.ReadString('\n')
		if err != nil {
			// Helper gone. A start still pending resolves as failed; the
			// exit code comes from the helper's own exit status (the stub
			// exits with the container's code).
			log.L.WithField("id", c.id).Info("report EOF")
			code := waitExitCode(c.cmd)
			if !started {
				c.startedCh <- startMsg{err: fmt.Errorf("init helper exited (code %d)", code)}
			}
			s.finalizeExit(c, code)
			return
		}
		line = strings.TrimRight(line, "\n")
		log.L.WithField("id", c.id).Debugf("report line: %q", line)
		switch {
		case strings.HasPrefix(line, reportStartedPrefix):
			pid, _ := strconv.Atoi(strings.TrimPrefix(line, reportStartedPrefix))
			s.mu.Lock()
			c.pid = pid
			s.mu.Unlock()
			started = true
			c.startedCh <- startMsg{pid: pid}
		case strings.HasPrefix(line, reportStartPrefix), strings.HasPrefix(line, reportSetupPrefix):
			detail := strings.TrimSpace(line[2:])
			if !started {
				c.startedCh <- startMsg{err: fmt.Errorf("%s", detail)}
			} else {
				log.L.WithField("id", c.id).Warnf("init report after start: %s", detail)
			}
		case strings.HasPrefix(line, reportDonePrefix):
			code, _ := strconv.Atoi(strings.TrimPrefix(line, reportDonePrefix))
			s.finalizeExit(c, uint32(code))
		}
	}
}

// finalizeExit records the container init's exit once: exit code cached for
// late waiters, wait channel closed, TaskExit published, and the fifo/file
// output sinks closed (the shim's last copies — the agent-side fifo logger
// ends on their EOF and writes its marker now, while any attach is live).
func (s *taskService) finalizeExit(c *container, code uint32) {
	s.mu.Lock()
	if c.exited {
		s.mu.Unlock()
		return
	}
	c.exited = true
	c.exitCode = code
	c.exitedAt = time.Now()
	c.status = task.Status_STOPPED
	pid, at := c.pid, c.exitedAt
	// The exit can land before the Start RPC published TaskStart (a
	// container that runs faster than the round trip). containerd drops a
	// TaskExit for a task it never saw start, so the event waits for
	// startInit to publish TaskStart first.
	started := c.started
	if !started {
		c.exitPending = true
	}
	s.mu.Unlock()
	close(c.waitCh)
	c.io.closeSinks()
	if started {
		publishTaskExit(s, c.id, "", pid, code, at)
	}
	log.L.WithFields(log.Fields{"id": c.id, "code": code, "deferred": !started}).Debug("container init exited")
}

// waitExitCode joins the helper process and returns its exit code (128+sig
// for signal deaths — the helper mirrors the container init's exit, so a
// SIGKILLed helper reports the kill convention, not a wrapped -1). Called
// exactly once, by the report reader.
func waitExitCode(cmd *exec.Cmd) uint32 {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return uint32(128 + int(ws.Signal()))
		}
		return uint32(ee.ExitCode())
	}
	return 0
}

// cleanupDeadShim is the "delete" action of the shim binary: best-effort
// cleanup after the shim server died without a proper Delete. cwd is the
// bundle; unmount the rootfs and drop the cgroup so a dead container does
// not leak its snapshot mounts or limits dir.
func cleanupDeadShim(ctx context.Context, id string) (shim.StopStatus, error) {
	if _, err := os.Stat("rootfs"); err == nil {
		if err := mount.UnmountRecursive("rootfs", 0); err != nil {
			return shim.StopStatus{}, err
		}
	}
	if spec, err := readSpecFile("config.json"); err == nil && spec.Linux != nil && spec.Linux.CgroupsPath != "" {
		removeCgroup(cgroupAbsPath(spec.Linux.CgroupsPath))
	}
	return shim.StopStatus{}, nil
}

// jsonDecodeAny decodes a typeurl Any payload (JSON for non-proto types).
func jsonDecodeAny(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
