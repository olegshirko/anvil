//go:build linux && shimserver

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/emptypb"
)

// `docker exec` / healthchecks: an extra process forked into the init's
// namespaces. Same trampoline shape as the container init, minus the mount
// work (the init already pivoted): the exec helper joins every namespace of
// the running init via setns, moves into the container's cgroup, applies
// the exec spec's attributes and creates the process through the raw
// fork+execve window. The gate/report protocol is identical to the init's.

// Exec registers an exec process. The helper is started now (so io failures
// surface here) and holds at its gate until Start.
func (s *taskService) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if c.status != task.Status_RUNNING {
		s.mu.Unlock()
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "container %s is not running", c.id)
	}
	if c.pid <= 0 {
		s.mu.Unlock()
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "container %s init is not running", c.id)
	}
	if _, dup := c.execs[r.ExecID]; dup {
		s.mu.Unlock()
		return nil, errgrpc.ToGRPC(errdefs.ErrAlreadyExists)
	}
	initPid := c.pid
	s.mu.Unlock()

	var pspec specs.Process
	if err := json.Unmarshal(r.Spec.Value, &pspec); err != nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrInvalidArgument, "exec spec: %v", err)
	}
	merged := mergeExecProcess(c.spec, &pspec)
	specJSON, err := json.Marshal(merged)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}

	// exec io: fifo paths from the client's attach (terminal gets a pty).
	ioSet, err := newStdioSet(ctx, r.ExecID, r.Stdin, r.Stdout, r.Stderr, r.Terminal)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	reportR, reportW, err := os.Pipe()
	if err != nil {
		ioSet.close()
		return nil, errgrpc.ToGRPC(err)
	}
	gateR, gateW, err := os.Pipe()
	if err != nil {
		reportR.Close()
		reportW.Close()
		ioSet.close()
		return nil, errgrpc.ToGRPC(err)
	}

	self, err := os.Executable()
	if err != nil {
		gateW.Close()
		gateR.Close()
		reportR.Close()
		reportW.Close()
		ioSet.close()
		return nil, errgrpc.ToGRPC(err)
	}
	cmd := exec.Command(self, "exec-init", "-initpid", strconv.Itoa(initPid), "-bundle", c.bundle)
	cmd.ExtraFiles = []*os.File{gateR, reportW, ioSet.childStdin, ioSet.childStdout, ioSet.childStderr}
	cmd.Env = append(os.Environ(), "ANVIL_EXEC_SPEC="+string(specJSON))
	if err := cmd.Start(); err != nil {
		gateW.Close()
		gateR.Close()
		reportR.Close()
		reportW.Close()
		ioSet.close()
		return nil, errgrpc.ToGRPCf(errdefs.ErrInternal, "start exec helper: %v", err)
	}
	gateR.Close()
	reportW.Close()

	// Setup report (the helper joined the namespaces and holds the gate).
	reportBuf := bufio.NewReader(reportR)
	watchdog := time.AfterFunc(15*time.Second, func() { cmd.Process.Kill() }) //nolint:errcheck
	line, rerr := reportBuf.ReadString('\n')
	watchdog.Stop()
	if rerr != nil || line != reportReady {
		detail := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), reportSetupPrefix))
		if rerr != nil {
			detail = rerr.Error()
		}
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		gateW.Close()
		reportR.Close()
		ioSet.close()
		return nil, errgrpc.ToGRPCf(errdefs.ErrInternal, "exec setup failed: %s", detail)
	}

	e := &execProcess{
		id:        r.ExecID,
		status:    task.Status_CREATED,
		waitCh:    make(chan struct{}),
		cmd:       cmd,
		gateW:     gateW,
		reportR:   reportR,
		reportBuf: reportBuf,
		startedCh: make(chan startMsg, 1),
		io:        ioSet,
		term:      r.Terminal,
	}
	s.mu.Lock()
	c.execs[r.ExecID] = e
	s.mu.Unlock()

	s.send(&eventstypes.TaskExecAdded{
		ContainerID: c.id,
		ExecID:      r.ExecID,
	})
	if r.Terminal {
		ioSet.startCopies(ctx)
	}
	go s.readExecReports(c, e)
	return &emptypb.Empty{}, nil
}

// startExec releases a created exec process from its gate.
func (s *taskService) startExec(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	e := c.execs[r.ExecID]
	if e == nil {
		s.mu.Unlock()
		return nil, errgrpc.ToGRPC(errdefs.ErrNotFound)
	}
	if e.status != task.Status_CREATED {
		s.mu.Unlock()
		return nil, errgrpc.ToGRPCf(errdefs.ErrFailedPrecondition, "process %s is not created", r.ExecID)
	}
	s.mu.Unlock()

	if _, err := e.gateW.WriteString(gateStartLine); err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	var msg startMsg
	select {
	case msg = <-e.startedCh:
	case <-ctx.Done():
		return nil, errgrpc.ToGRPC(context.Canceled)
	case <-time.After(10 * time.Second):
		return nil, errgrpc.ToGRPCf(errdefs.ErrUnknown, "exec start timed out")
	}
	if msg.err != nil {
		return nil, errgrpc.ToGRPCf(errdefs.ErrUnknown, "failed to start exec: %v", msg.err)
	}
	s.mu.Lock()
	e.status = task.Status_RUNNING
	s.mu.Unlock()
	return &taskAPI.StartResponse{Pid: uint32(msg.pid)}, nil
}

// readExecReports mirrors readReports for one exec process.
func (s *taskService) readExecReports(c *container, e *execProcess) {
	var started bool
	for {
		line, err := e.reportBuf.ReadString('\n')
		if err != nil {
			code := waitExitCode(e.cmd)
			if !started {
				e.startedCh <- startMsg{err: fmt.Errorf("exec helper exited (code %d)", code)}
			}
			s.finalizeExecExit(c, e, code)
			return
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, reportStartedPrefix):
			pid, _ := strconv.Atoi(strings.TrimPrefix(line, reportStartedPrefix))
			e.pid = pid
			started = true
			e.startedCh <- startMsg{pid: pid}
			// Queue the started event here, in report order: for an instant
			// exec the D line follows immediately, and publishing from
			// startExec would race the exit event ahead of it.
			s.send(&eventstypes.TaskExecStarted{
				ContainerID: c.id,
				ExecID:      e.id,
				Pid:         uint32(pid),
			})
		case strings.HasPrefix(line, reportStartPrefix), strings.HasPrefix(line, reportSetupPrefix):
			detail := strings.TrimSpace(line[2:])
			if !started {
				e.startedCh <- startMsg{err: fmt.Errorf("%s", detail)}
			}
		case strings.HasPrefix(line, reportDonePrefix):
			code, _ := strconv.Atoi(strings.TrimPrefix(line, reportDonePrefix))
			s.finalizeExecExit(c, e, uint32(code))
		}
	}
}

// finalizeExecExit records one exec process's exit: status, exit code,
// TaskExit, and the fifo/file output sinks closed (the exec's readers EOF
// at process death, like the runc shim's io teardown).
func (s *taskService) finalizeExecExit(c *container, e *execProcess, code uint32) {
	s.mu.Lock()
	if e.exited {
		s.mu.Unlock()
		return
	}
	e.exited = true
	e.exitCode = code
	e.exitedAt = time.Now()
	e.status = task.Status_STOPPED
	pid := e.pid
	s.mu.Unlock()
	close(e.waitCh)
	e.io.closeSinks()
	publishTaskExit(s, c.id, e.id, pid, code, e.exitedAt)
	log.L.WithFields(log.Fields{"id": c.id, "exec": e.id, "code": code}).Debug("exec exited")
}

// mergeExecProcess applies the container defaults an exec inherits when its
// own spec does not carry them (runc exec semantics): capabilities,
// rlimits, oom score and the working directory stay the init's.
func mergeExecProcess(init *specs.Spec, pspec *specs.Process) *specs.Process {
	merged := *pspec
	if merged.Capabilities == nil && init.Process != nil {
		merged.Capabilities = init.Process.Capabilities
	}
	if len(merged.Rlimits) == 0 && init.Process != nil {
		merged.Rlimits = init.Process.Rlimits
	}
	if merged.OOMScoreAdj == nil && init.Process != nil && init.Process.OOMScoreAdj != nil {
		adj := *init.Process.OOMScoreAdj
		merged.OOMScoreAdj = &adj
	}
	return &merged
}
