//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

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

// isExecHelperDispatched reports the exec helper entry; the real work is in
// runExecHelper below.
//
// runExecHelper joins the running init's namespaces, moves into its cgroup,
// applies the exec attributes and creates the user process via the raw
// fork+execve window. Reports: R (holding at the gate), S pid, X detail,
// D code — same protocol as the init helper.
func runExecHelper(initpidArg, bundle string) int {
	initpid, err := strconv.Atoi(initpidArg)
	if err != nil {
		reportLine(reportSetupPrefix+"bad init pid %q: %v", initpidArg, err)
		return 1
	}
	specJSON := os.Getenv("ANVIL_EXEC_SPEC")
	var pspec specs.Process
	if err := json.Unmarshal([]byte(specJSON), &pspec); err != nil {
		reportLine(reportSetupPrefix+"exec spec: %v", err)
		return 1
	}
	initSpec, serr := readSpecFile(filepath.Join(bundle, "config.json"))
	if serr != nil {
		reportLine(reportSetupPrefix+"init spec: %v", serr)
		return 1
	}

	// Join everything the running init has. Namespace fds are opened while
	// paths still resolve (host view), then setns: cgroup join before the
	// mount switch (the procs file is a host path), mount namespace last.
	if err := joinExecNamespaces(initpid, initSpec); err != nil {
		reportLine(reportSetupPrefix+"%v", err)
		return 1
	}

	// Final stdio (fds 4/5/6 were handed over by the shim).
	tty := pspec.Terminal
	if tty {
		if _, err := unix.Setsid(); err != nil {
			reportLine(reportSetupPrefix+"setsid: %v", err)
			return 1
		}
		if err := unix.IoctlSetInt(fdExecStdin, unix.TIOCSCTTY, 0); err != nil {
			reportLine(reportSetupPrefix+"TIOCSCTTY: %v", err)
			return 1
		}
	}
	for fd, target := range map[int]int{fdExecStdin: 0, fdExecStdout: 1, fdExecStderr: 2} {
		if err := unix.Dup3(fd, target, 0); err != nil {
			reportLine(reportSetupPrefix+"dup %d: %v", fd, err)
			return 1
		}
	}
	unix.Close(fdExecStdin)  //nolint:errcheck
	unix.Close(fdExecStdout) //nolint:errcheck
	unix.Close(fdExecStderr) //nolint:errcheck

	// Resolve the exec path before the process attributes are applied —
	// the seccomp filter is part of those attributes and is active on this
	// process (and the forked child doing the lookup) from its install
	// until the exec. Resolution needs stat under the container's PATH,
	// which the default profile denies (runc's ordering is the same for a
	// reason). The namespaces are already joined above, so the lookup sees
	// the container's rootfs.
	argv0, err := resolveExecPath(&specs.Spec{Process: &pspec})
	if err != nil {
		reportLine(reportStartPrefix+"%v", err)
		return 127
	}

	// Attributes: the merged exec process (falls back to the init's caps,
	// rlimits and oom score) plus the container's seccomp filter — the
	// filter is inherited only from the parent, and this process is a
	// fresh child of the shim, so it must install it explicitly.
	if err := applyProcessAttrs(&specs.Spec{Process: &pspec, Linux: initSpec.Linux}); err != nil {
		reportLine(reportStartPrefix+"%v", err)
		return 126
	}
	reportLine(reportReady)

	if !waitGate() {
		return 0
	}
	code, pid, detail, err := forkAndExecUser(&pspec, fdExecGate, argv0)
	if err != nil {
		reportLine(reportStartPrefix+"%s", detail)
		return 127
	}
	reportLine(reportStartedPrefix+"%d", pid)

	// Wait for the exec'd process. The helper never entered the pid
	// namespace (only its raw child does, inside the fork window), so this
	// pid IS the global one — the same number TaskExecStarted carried.
	var ws unix.WaitStatus
	if _, waitErr := unix.Wait4(pid, &ws, 0, nil); waitErr != nil {
		code = 137
	} else {
		code = int(exitStatus(ws))
	}
	reportLine(reportDonePrefix+"%d", code)
	return code
}

// exec helper fd layout (ExtraFiles start at 3): gate, report, then stdio.
const (
	fdExecGate   = 3
	fdExecStdin  = 5
	fdExecStdout = 6
	fdExecStderr = 7
)

// joinExecNamespaces setns's into the running init's namespaces (every ns
// type the init has, mount last) and moves into the init's cgroup.
func joinExecNamespaces(initpid int, initSpec *specs.Spec) error {
	// The init's cgroup from its /proc entry (works from any mount view).
	joinCgroupOf(initpid)

	types := []specs.LinuxNamespaceType{
		specs.UserNamespace, specs.IPCNamespace, specs.UTSNamespace,
		specs.NetworkNamespace, specs.CgroupNamespace, specs.PIDNamespace,
	}
	// The PID namespace must be joined from a single-threaded process
	// (setns(pid) is EINVAL for a multithreaded one — the Go runtime is
	// always multi-threaded), so pid+mount join later in the raw fork
	// window; the rest joins here.
	var pidFd int
	for _, typ := range types {
		fd, err := unix.Open(fmt.Sprintf("/proc/%d/ns/%s", initpid, nsTypeName(typ)), unix.O_RDONLY, 0)
		if err != nil {
			if typ == specs.UserNamespace {
				continue // no private user namespace: fine
			}
			return fmt.Errorf("open %s ns: %w", typ, err)
		}
		if err := unix.Setns(fd, 0); err != nil {
			unix.Close(fd) //nolint:errcheck
			if typ == specs.UserNamespace {
				// Containers without a private user namespace share the
				// host's; setns into the namespace we are already in
				// returns EINVAL. Best effort, like runc's nsenter.
				continue
			}
			return fmt.Errorf("setns %s: %w", typ, err)
		}
		if typ == specs.PIDNamespace {
			pidFd = fd // kept open for the raw window (close-on-exec)
			continue
		}
		unix.Close(fd) //nolint:errcheck
	}
	mntFd, err := unix.Open(fmt.Sprintf("/proc/%d/ns/mnt", initpid), unix.O_RDONLY, 0)
	if err != nil {
		if pidFd > 0 {
			unix.Close(pidFd) //nolint:errcheck
		}
		return fmt.Errorf("open mnt ns: %w", err)
	}
	execPidNsFd, execMntNsFd = pidFd, mntFd
	return nil
}

// execPidNsFd / execMntNsFd carry the namespace fds into the raw fork
// window (single-threaded there, so pid+mount setns is legal).
var execPidNsFd, execMntNsFd = -1, -1

// nsTypeName maps a namespace type to its /proc/<pid>/ns directory name.
func nsTypeName(t specs.LinuxNamespaceType) string {
	switch t {
	case specs.PIDNamespace:
		return "pid"
	case specs.UserNamespace:
		return "user"
	case specs.IPCNamespace:
		return "ipc"
	case specs.UTSNamespace:
		return "uts"
	case specs.MountNamespace:
		return "mnt"
	case specs.CgroupNamespace:
		return "cgroup"
	case specs.NetworkNamespace:
		return "net"
	case specs.TimeNamespace:
		return "time"
	}
	return string(t)
}

// joinCgroupOf moves the caller into the init process's cgroup (v2: the
// "0::/path" line of its /proc/<pid>/cgroup).
func joinCgroupOf(initpid int) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", initpid))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		rel, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		rel = strings.TrimSpace(rel)
		if rel == "" || rel == "/" {
			return // root cgroup: nothing to join
		}
		os.WriteFile(filepath.Join(cgroupRoot, strings.TrimPrefix(rel, "/"), "cgroup.procs"), //nolint:errcheck
			[]byte(strconv.Itoa(os.Getpid())), 0o600)
		return
	}
}

// forkAndExecUser is the exec trampoline: the shared forkExecUser with the
// exec helper's gate/report fd layout. Returns once the execve landed
// (confirm pipe EOF) or failed (message came through instead).
// execCwdPtr carries the exec spec's working directory into the raw
// window (set by runExecHelper before the fork).
var execCwdPtr *byte

// forkExecUser is the raw fork+execve trampoline shared with the exec
// helper: pre-built pointers, syscalls only in the child. argv0 arrives
// already resolved to an absolute path — the caller resolves it before the
// container's seccomp filter is installed, since the lookup's stat calls
// are not on the profile's allowlist. Returns the parent-side read end of
// the confirm pipe plus the child's pid (namespace view of the caller).
func forkExecUser(p *specs.Process, gateFD, reportFD int, argv0 string) (confirmR int, pid int, detail string, err error) {
	pathPtr, err := unix.BytePtrFromString(argv0)
	if err != nil {
		return -1, 0, "", err
	}
	argvPtrs, err := argvBytePtrs(p.Args)
	if err != nil {
		return -1, 0, "", err
	}
	envPtrs, err := envBytePtrs(p.Env)
	if err != nil {
		return -1, 0, "", err
	}
	confirm := make([]int, 2)
	if err := unix.Pipe(confirm); err != nil {
		return -1, 0, "", fmt.Errorf("confirm pipe: %w", err)
	}
	// Everything the exec'd process must not inherit: the gate (consumed),
	// the report pipe (stage exit goes through Wait4, and a live write end
	// would hold the shim's report-EOF fallback open), the confirm read
	// end and the namespace fds (consumed by the setns above).
	closeFDs := []int{gateFD, reportFD, confirm[0]}
	if execPidNsFd >= 0 {
		closeFDs = append(closeFDs, execPidNsFd)
	}
	if execMntNsFd >= 0 {
		closeFDs = append(closeFDs, execMntNsFd)
	}
	args := pid0Args{
		confirmFD:  confirm[1],
		pathPtr:    pathPtr,
		argvPtrs:   argvPtrs,
		envPtrs:    envPtrs,
		failMsg:    &failExecMessage[0],
		failMsgLen: len(failExecMessage),
		pidNsFd:    execPidNsFd,
		mntNsFd:    execMntNsFd,
		cwdPtr:     execCwdPtr,
		closeFDs:   closeFDs,
	}
	pid, errno := forkRaw()
	if errno != 0 {
		unix.Close(confirm[0])
		unix.Close(confirm[1])
		return -1, 0, "", fmt.Errorf("fork: %w", errno)
	}
	if pid == 0 {
		pid0RawChild(args)
		// unreached
	}
	unix.Close(confirm[1]) //nolint:errcheck — parent keeps the read end
	return confirm[0], pid, "", nil
}

// forkAndExecUser is the exec trampoline: the raw fork+execve of the user
// process inside the joined namespaces. Returns once the execve landed
// (confirm pipe EOF) or failed (message came through instead). The returned
// pid is the exec'd process's global pid: the helper never joins the pid
// namespace (setns(pid) is reserved for the raw child), so the fork's
// return value in the helper is already the host-view pid — the one
// TaskExecStarted, exec state and the shim's kill need.
func forkAndExecUser(pspec *specs.Process, gateFD int, argv0 string) (code int, pid int, detail string, err error) {
	defer func() {
		if execPidNsFd > 0 {
			unix.Close(execPidNsFd) //nolint:errcheck
			execPidNsFd = -1
		}
		if execMntNsFd > 0 {
			unix.Close(execMntNsFd) //nolint:errcheck
			execMntNsFd = -1
		}
		execCwdPtr = nil
	}()
	var cwdErr error
	if pspec.Cwd != "" {
		execCwdPtr, cwdErr = unix.BytePtrFromString(pspec.Cwd)
		if cwdErr != nil {
			return 127, 0, "", cwdErr
		}
	}
	confirmR, childPid, detail, err := forkExecUser(pspec, gateFD, fdReport, argv0)
	if err != nil {
		return 127, 0, detail, err
	}
	defer unix.Close(confirmR) //nolint:errcheck
	buf := make([]byte, 128)
	n, rerr := unix.Read(confirmR, buf)
	if rerr != nil || n > 0 {
		msg := strings.TrimSpace(string(buf[:n]))
		if msg == "" {
			msg = "exec died before exec"
		}
		var ws unix.WaitStatus
		wp, werr := unix.Wait4(childPid, &ws, 0, nil)
		code = 127
		if werr == nil && wp == childPid {
			code = int(exitStatus(ws))
		}
		return code, 0, msg, fmt.Errorf("exec failed")
	}
	return 0, childPid, "", nil
}
