package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Stage 5a: the direct start path for eligible containers. The agent itself
// is the supervisor: it prepares the snapshot rootfs, writes the bundle, does
// the CNI attach, spawns the proven init helper (/bin/anvil-fast-shim init)
// once — no containerd task, no ttrpc shim server, so no task-create exec
// chain and no socket handshake — and joins its exit with cmd.Wait. The
// helper's namespace/mount/cgroup/seccomp code is unchanged; only who creates
// its cgroup moves into it (-cgroup), keeping that code in one place.
//
// IO is agent-owned: the helper's stdout/stderr are pipes read by this
// process and written to the json-file log, so the log's end (and thus
// attach's EOF) is deterministic — no shim logger, no pid-file marker, no
// quiet window.
//
// Every failure before the user process starts falls back to the existing
// containerd path (startNativeTask's task half), invisibly to the client.
// Non-eligible containers (privileged, custom runtime/seccomp, --pid
// container:, subpath mounts, secondary networks) and TTY containers keep
// the containerd fast-shim path in 5a.

// directBundleRoot is the tmpfs root of direct bundles: a per-container
// config.json plus the mounted rootfs the helper pivots into (the same shape
// containerd's runtime-v2 bundle has, but owned by the agent).
const directBundleRoot = "/run/anvil/direct"

func directBundleDir(ns, id string) string {
	return filepath.Join(directBundleRoot, ns, id)
}

func directRootfsPath(ns, id string) string {
	return filepath.Join(directBundleDir(ns, id), "rootfs")
}

// directEligible reports whether a container may take the direct path. It
// reuses the fast-shim eligibility decision (the record's runtime) and keeps
// TTY containers on the containerd fast-shim path: 5a does not redesign the
// TTY console copy, so their attach keeps the timer-based end exactly as
// today.
func directEligible(meta *containerMeta) bool {
	if fastShimDisabled() || meta == nil || meta.HostConfig == nil || meta.TTY {
		return false
	}
	return fastShimEligible(dockerCreateRequest{HostConfig: *meta.HostConfig}, meta)
}

// directCgroupPath is the container's cgroup v2 directory (empty when the
// spec places none), the same resolution the helper's cgroup.go does.
func directCgroupPath(spec *specs.Spec) string {
	if spec == nil || spec.Linux == nil || spec.Linux.CgroupsPath == "" {
		return ""
	}
	return filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(spec.Linux.CgroupsPath, "/"))
}

// --- start ------------------------------------------------------------------

// direct report protocol (guest-shim/reports.go is the helper's side; the
// agent implements the consumer here).
const (
	directReportReady   = "R"
	directReportSetup   = "E "
	directReportStart   = "X "
	directReportStarted = "S "
	directReportDone    = "D "
	directReportInit    = "P "
)

// directReportEvent is one parsed helper report line.
type directReportEvent struct {
	kind   byte // 'P', 'R', 'S', 'X', 'D', 'E', 'e' (EOF: code is the helper's)
	pid    int
	code   int
	detail string
}

// startDirectContainer starts one eligible container without containerd in
// the task path. It returns an error only before anything irreversible
// happened to the client: startNativeTask then falls back to the containerd
// task path. Once the helper is released and the start event published, the
// run is the registry's.
func startDirectContainer(ctx context.Context, ns, id string, c client.Container, meta *containerMeta, nsCtx context.Context) (err error) {
	tr := newPhaseTrace("direct", truncateID(id))
	// The previous run's record (exited) must not answer /wait for this one.
	if old := supervisor.remove(ns, id); old != nil {
		old.release()
	}
	info, ierr := c.Info(nsCtx)
	if ierr != nil {
		return ierr
	}
	netName, ports, detachNet, nerr := startContainerNetwork(ctx, ns, id, meta, tr)
	if nerr != nil {
		return nerr
	}

	did := dockerID(ns, id)
	run := beginTaskRun(did)
	takeContainerExitCode(did)
	// A previous run's agent-side pid file (a containerd fast-shim marker)
	// must not survive into this log: the agent writes this run's marker.
	os.Remove(initPidPath(containerLogPath(ns, id))) //nolint:errcheck — stale file only

	if meta.HostConfig != nil {
		if jerr := joinContainerNamespaces(nsCtx, c, meta.HostConfig.PidMode, meta.HostConfig.IpcMode); jerr != nil {
			detachNet()
			return jerr
		}
	}
	spec, serr := c.Spec(nsCtx)
	if serr != nil {
		detachNet()
		return fmt.Errorf("container spec: %w", serr)
	}
	tr.mark("spec")

	bundle, rootfs := directBundleDir(ns, id), directRootfsPath(ns, id)
	if merr := os.MkdirAll(rootfs, 0o711); merr != nil {
		detachNet()
		return merr
	}
	cl, cerr := pc.get(ctx)
	if cerr != nil {
		detachNet()
		os.RemoveAll(bundle)
		return fmt.Errorf("containerd client: %w", cerr)
	}
	snapshotMounts, oerr := cl.SnapshotService(info.Snapshotter).Mounts(nsCtx, info.SnapshotKey)
	if oerr != nil {
		detachNet()
		os.RemoveAll(bundle)
		return fmt.Errorf("snapshot mounts: %w", oerr)
	}
	if merr := mountAll(snapshotMounts, rootfs); merr != nil {
		detachNet()
		os.RemoveAll(bundle)
		return fmt.Errorf("mount rootfs: %w", merr)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { unmountAll(rootfs) }) }
	committed := false
	defer func() {
		if !committed {
			release()
			detachNet()
		}
	}()
	specData, _ := json.Marshal(spec)
	if werr := os.WriteFile(filepath.Join(bundle, "config.json"), specData, 0o644); werr != nil {
		return werr
	}
	tr.mark("rootfs")

	rot := logRotation{maxSize: defaultLogMaxSize, maxFile: defaultLogMaxFile}
	logNone := false
	if meta.HostConfig != nil {
		if r, rerr := logRotationFor(meta.HostConfig.LogConfig.Config); rerr == nil {
			rot = r
		}
		logNone = meta.HostConfig.LogConfig.Type == "none"
	}
	sink, lerr := newDirectLogSink(containerLogPath(ns, id), rot, logNone)
	if lerr != nil {
		return lerr
	}

	// Child stdio: agent-owned pipes for stdout/stderr (the log sink reads
	// them), the container stdin FIFO for an OpenStdin container.
	childStdin, stdin, csErr := directChildStdin(ns, id, meta)
	if csErr != nil {
		sink.close()
		return csErr
	}
	stdoutR, stdoutW, perr := os.Pipe()
	if perr != nil {
		closeFile(childStdin)
		sink.close()
		return perr
	}
	stderrR, stderrW, perr := os.Pipe()
	if perr != nil {
		closeFile(childStdin)
		stdoutR.Close()
		stdoutW.Close()
		sink.close()
		return perr
	}
	gateR, gateW, perr := os.Pipe()
	if perr != nil {
		closeFile(childStdin)
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		sink.close()
		return perr
	}
	reportR, reportW, perr := os.Pipe()
	if perr != nil {
		closeFile(childStdin)
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		gateR.Close()
		gateW.Close()
		sink.close()
		return perr
	}

	cmd := exec.Command(fastShimRuntimeName, "init", "-id", id, "-bundle", bundle, "-cgroup")
	cmd.ExtraFiles = []*os.File{gateR, reportW, childStdin, stdoutW, stderrW}
	cmd.Env = os.Environ()
	if debugMode {
		// Debug builds record the helper's setup phases for `docker run`
		// latency traces (helperTrace in guest-shim/init.go).
		cmd.Env = append(cmd.Env, "ANVIL_HELPER_TRACE=1")
	}
	tr.mark("spawn")
	if serr := cmd.Start(); serr != nil {
		closeFile(childStdin)
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		gateR.Close()
		gateW.Close()
		reportR.Close()
		reportW.Close()
		sink.close()
		return fmt.Errorf("start init helper: %w", serr)
	}
	// The child holds its copies now (it inherits them across exec); the
	// parent drops the child-only ends.
	gateR.Close()
	reportW.Close()
	stdoutW.Close()
	stderrW.Close()
	closeFile(childStdin)

	task := &directTask{
		ns: ns, id: id, did: did, run: run,
		cmd: cmd, helperPID: cmd.Process.Pid,
		bundle: bundle, rootfs: rootfs, cgroupPath: directCgroupPath(spec),
		release: release, state: directStateCreated,
		waitCh: make(chan struct{}),
	}
	events := make(chan directReportEvent, 16)
	go readDirectReports(cmd, reportR, events)

	// Setup: wait for the ready report (or a setup failure / helper death).
	watchdog := time.AfterFunc(15*time.Second, func() { cmd.Process.Kill() }) //nolint:errcheck
	var setupErr error
	ready := false
	for !ready && setupErr == nil {
		select {
		case ev, ok := <-events:
			if !ok {
				setupErr = fmt.Errorf("init helper exited during setup")
				break
			}
			switch ev.kind {
			case 'P':
				if ev.pid > 0 {
					task.mu.Lock()
					task.pid = ev.pid
					task.mu.Unlock()
				}
				tr.mark("init-born")
			case 'R':
				tr.mark("init-ready")
				copyHelperTrace(id)
				ready = true
			case 'E':
				setupErr = fmt.Errorf("%s", ev.detail)
			case 'e':
				setupErr = fmt.Errorf("init helper exited during setup (code %d)", ev.code)
			}
		case <-time.After(15 * time.Second):
			setupErr = fmt.Errorf("init setup timed out")
		case <-ctx.Done():
			setupErr = ctx.Err()
		}
	}
	watchdog.Stop()
	if setupErr != nil {
		cmd.Process.Kill() //nolint:errcheck — a stuck helper must not leak; the report reader joins it
		gateW.Close()
		stdoutR.Close()
		stderrR.Close()
		sink.close()
		if task.cgroupPath != "" {
			os.RemoveAll(task.cgroupPath) //nolint:errcheck — best effort on a failed setup
		}
		return setupErr
	}

	// The helper is holding the container at its gate, rootfs mounted and
	// cgroup prepared. Register before releasing it: /wait may already be
	// polling, and an instant container must find its record.
	supervisor.put(task)
	if stdin != nil {
		stdin.taskCreated()
	}
	startDirectLog(sink, stdoutR, stderrR)

	if _, werr := gateW.WriteString("s\n"); werr != nil {
		gateW.Close()
		sink.close()
		supervisor.remove(ns, id)
		task.failStart(fmt.Errorf("release init gate: %w", werr))
		cmd.Process.Kill() //nolint:errcheck
		if task.cgroupPath != "" {
			os.RemoveAll(task.cgroupPath) //nolint:errcheck — the killed helper cannot remove it
		}
		return fmt.Errorf("release init gate: %w", werr)
	}
	gateW.Close()
	tr.mark("gate")

	// Wait for the started report: the helper confirms the user exec landed.
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	perr = waitDirectStarted(startCtx, events, task, tr)
	cancel()
	if perr != nil {
		sink.close()
		supervisor.remove(ns, id)
		task.failStart(perr)
		cmd.Process.Kill() //nolint:errcheck
		if task.cgroupPath != "" {
			os.RemoveAll(task.cgroupPath) //nolint:errcheck — the killed helper cannot remove it
		}
		return fmt.Errorf("start: %w", perr)
	}
	tr.mark("task-start")

	updateContainerMeta(ns, id, func(m *containerMeta) { //nolint:errcheck
		m.StartedAt, m.FinishedAt = time.Now().UTC(), time.Time{}
	})
	task.mu.Lock()
	initPID := task.pid
	task.mu.Unlock()
	publishContainerEvent("start", ns, id, map[string]string{"pid": strconv.Itoa(initPID)})
	// Re-attach the health monitor on every start, exactly as the containerd
	// task path does.
	if hc := getHealthcheckConfig(did); hc != nil {
		startHealthCheck(did, ns, id, hc, getHealthcheckUser(did))
	}
	committed = true

	// The rest of the run is supervised here: exit finalization (log marker,
	// die event, registry exit, meta, CNI detach) in the report consumer.
	go finishDirectRun(context.Background(), task, events, sink, netName, ports, run)
	return nil
}

// copyHelperTrace moves the helper's setup-phase trace out of the guest
// tmpfs into the shared .anvil-run directory (debug builds only). The helper
// writes to tmpfs so the marks themselves cost microseconds; a virtiofs write
// there can block for tens of milliseconds and distort the phases.
func copyHelperTrace(id string) {
	if !debugMode {
		return
	}
	src := "/tmp/anvil-helper-trace-" + id
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	os.Remove(src) //nolint:errcheck — best effort
	f, err := os.OpenFile(filepath.Join(anvilRunDir, "direct-helper-trace.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(data) //nolint:errcheck — diagnostics only
}

// waitDirectStarted consumes reports until the started one (S), a start
// failure (X/D) or helper EOF.
func waitDirectStarted(ctx context.Context, events <-chan directReportEvent, task *directTask, tr *phaseTrace) error {
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return fmt.Errorf("init helper exited before the user process started")
			}
			switch ev.kind {
			case 'S':
				if ev.pid > 0 {
					task.mu.Lock()
					task.pid = ev.pid
					task.mu.Unlock()
				}
				task.markStarted(ev.pid)
				tr.mark("init-exec")
				return nil
			case 'X', 'E':
				return fmt.Errorf("%s", ev.detail)
			case 'D':
				return fmt.Errorf("container exited during start (code %d)", ev.code)
			case 'e':
				return fmt.Errorf("init helper exited during start (code %d)", ev.code)
			}
		case <-ctx.Done():
			return fmt.Errorf("start timed out waiting for the init")
		}
	}
}

// finishDirectRun consumes the remaining reports, joins the helper and records
// the exit. Ordering matters: the log's eof marker (attach's deterministic
// end) and the exit state are written before the registry's waitCh closes, so
// `docker run` returns on final output, and auto-remove waits on a container
// whose state is already final.
func finishDirectRun(ctx context.Context, task *directTask, events <-chan directReportEvent, sink *directLogSink, netName string, ports []cniPortMapping, run uint64) {
	doneCode := -1
	for ev := range events {
		switch ev.kind {
		case 'D':
			doneCode = ev.code
		case 'e':
			if doneCode < 0 {
				doneCode = ev.code
			}
		}
	}

	// Output is final: the helper is gone, so both pipes are at EOF and the
	// copiers end on their own. Flush, mark and close the log.
	sink.finish()

	did := task.did
	if !isCurrentTaskRun(did, run) {
		// The container was started again before this run's watcher woke:
		// the new run owns the exit code, the health monitor and the network.
		debugLog("[direct] task %s/%s exited code=%d (superseded run)", task.ns, truncateID(task.id), doneCode)
		finishDirectRecord(task, doneCode)
		task.release()
		return
	}
	// docker kill caches the mapped 137 before the task dies; never
	// overwrite it with the helper's 0.
	final := doneCode
	if cached, ok := peekContainerExitCode(did); !ok || doneCode != 0 || cached == 0 {
		cacheContainerExitCode(did, doneCode)
	} else {
		final = cached
	}
	updateContainerMeta(task.ns, task.id, func(m *containerMeta) { //nolint:errcheck
		m.FinishedAt, m.ExitCode = time.Now().UTC(), final
	})
	publishContainerEvent("die", task.ns, task.id, map[string]string{"exitCode": strconv.Itoa(final)})
	debugLog("[direct] task %s/%s exited code=%d", task.ns, truncateID(task.id), final)

	// Claim the endpoint BEFORE the registry exit: a restart-policy start
	// woken by the exit then waits on this release (awaitNetRelease) instead
	// of racing the detach with its CNI ADD.
	var ni containerNetInfo
	haveNet := false
	if !usesHostNetworkName(netName) {
		ni, haveNet = claimNetInfo(task.ns, task.id)
	}

	// Close the registry wait last: /wait, auto-remove and the restart
	// monitor only act on a fully recorded exit.
	finishDirectRecord(task, final)

	stopHealthCheck(did)
	if haveNet {
		releaseEndpoints(ctx, task.ns, task.id, ni, ports)
	}
	task.release()
}

// finishDirectRecord publishes the exit state to the registry.
func finishDirectRecord(task *directTask, code int) {
	task.finished(code)
}

// waitDirectHelperExit mirrors the shim's waitExitCode: 128+sig for a signal
// death, the exit status otherwise.
func waitDirectHelperExit(cmd *exec.Cmd) int {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 0
}

// readDirectReports parses the helper's report stream into events, then
// closes the channel. On EOF it sends the helper's own exit code ('e').
func readDirectReports(cmd *exec.Cmd, r io.Reader, events chan<- directReportEvent) {
	defer close(events)
	buf := bufio.NewReader(r)
	for {
		line, err := buf.ReadString('\n')
		if err != nil {
			events <- directReportEvent{kind: 'e', code: waitDirectHelperExit(cmd)}
			return
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == directReportReady:
			events <- directReportEvent{kind: 'R'}
		case strings.HasPrefix(line, directReportInit):
			pid, _ := strconv.Atoi(strings.TrimPrefix(line, directReportInit))
			events <- directReportEvent{kind: 'P', pid: pid}
		case strings.HasPrefix(line, directReportStarted):
			pid, _ := strconv.Atoi(strings.TrimPrefix(line, directReportStarted))
			events <- directReportEvent{kind: 'S', pid: pid}
		case strings.HasPrefix(line, directReportDone):
			code, _ := strconv.Atoi(strings.TrimPrefix(line, directReportDone))
			events <- directReportEvent{kind: 'D', code: code}
		case strings.HasPrefix(line, directReportStart):
			events <- directReportEvent{kind: 'X', detail: strings.TrimSpace(strings.TrimPrefix(line, directReportStart))}
		case strings.HasPrefix(line, directReportSetup):
			events <- directReportEvent{kind: 'E', detail: strings.TrimSpace(strings.TrimPrefix(line, directReportSetup))}
		}
	}
}

// directChildStdin prepares the helper's stdin: the container's stdin FIFO
// for an OpenStdin container, /dev/null otherwise.
func directChildStdin(ns, id string, meta *containerMeta) (*os.File, *containerStdin, error) {
	if meta != nil && meta.OpenStdin {
		cs, err := openContainerStdin(ns, id, true)
		if err != nil {
			return nil, nil, err
		}
		cs.taskStarting()
		// O_NONBLOCK so the open does not wait for a writer; the helper turns
		// blocking mode back on for the user process.
		f, err := os.OpenFile(cs.path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("stdin fifo: %w", err)
		}
		return f, cs, nil
	}
	f, err := os.Open("/dev/null")
	if err != nil {
		return nil, nil, err
	}
	return f, nil, nil
}

func closeFile(f *os.File) {
	if f != nil {
		f.Close()
	}
}

// --- stop / delete -----------------------------------------------------------

// stopDirectTask is stopNativeTask's direct branch: signal escalation on the
// container init, then the same CNI teardown.
func stopDirectTask(ctx context.Context, ns, id string, t *directTask, timeoutSec int, signal string) error {
	state, _, running := t.stateInfo()
	if !running && state != directStateCreated {
		teardownNetwork(ctx, ns, id)
		return nil
	}
	t.mu.Lock()
	pid := t.pid
	helper := t.helperPID
	t.mu.Unlock()
	_ = pid // the signal goes through t.signal (init pid, else the helper)

	sig := syscall.SIGTERM
	if meta, merr := loadContainerMeta(ns, id); merr == nil && meta.StopSignal != "" {
		if s, ok := signalValue(meta.StopSignal); ok {
			sig = s
		}
	}
	if signal != "" {
		if s, ok := signalValue(signal); ok {
			sig = s
		}
	}
	publishContainerEvent("kill", ns, id, map[string]string{"signal": strconv.Itoa(int(sig))}) // as Docker's stop
	if err := t.signal(sig); err != nil {
		return err
	}
	// Docker: -t 0 kills at once, -t -1 waits for as long as it takes.
	var wait time.Duration
	if timeoutSec < 0 {
		wait = 100 * 365 * 24 * time.Hour
	} else {
		wait = time.Duration(timeoutSec) * time.Second
	}
	if _, ok := waitDirectExit(t, wait); !ok {
		publishContainerEvent("kill", ns, id, map[string]string{"signal": strconv.Itoa(int(syscall.SIGKILL))})
		if err := t.signal(syscall.SIGKILL); err != nil {
			return fmt.Errorf("force kill: %w", err)
		}
		if helper > 0 {
			syscall.Kill(helper, syscall.SIGKILL) //nolint:errcheck — the helper supervises the init
		}
		if _, ok := waitDirectExit(t, 5*time.Second); !ok {
			log.Printf("[direct] stop %s: init did not exit after SIGKILL; deleting anyway", truncateID(id))
		}
	}
	teardownNetwork(ctx, ns, id)
	return nil
}

// killDirectTask signals a running direct task (docker kill).
func killDirectTask(ctx context.Context, ns, id string, t *directTask, signal string) error {
	_, _, running := t.stateInfo()
	if !running {
		return errConflict("cannot kill container: %s: container %s is not running", dockerID(ns, id), truncateID(id))
	}
	sig := syscall.SIGKILL
	if signal != "" {
		if s, ok := signalValue(signal); ok {
			sig = s
		}
	}
	if err := t.signal(sig); err != nil {
		return err
	}
	publishContainerEvent("kill", ns, id, map[string]string{"signal": strconv.Itoa(int(sig))})
	return nil
}

// directDelete tears a direct container's task down before the containerd
// record goes: kill anything left, wait for the exit, unmount the rootfs and
// drop the bundle. Idempotent (the exit watcher may have done most of it).
func directDelete(t *directTask, force bool) error {
	state, _, running := t.stateInfo()
	if (running || state == directStateCreated) && !force {
		return errConflict("cannot remove container %q: container is running: stop the container before removing or force remove", truncateID(t.did))
	}
	if running || state == directStateCreated {
		t.signal(syscall.SIGKILL)                  //nolint:errcheck — force path
		syscall.Kill(t.helperPID, syscall.SIGKILL) //nolint:errcheck — created: the helper holds the gate
		waitDirectExit(t, 5*time.Second)
	}
	supervisor.remove(t.ns, t.id)
	t.release()
	if t.cgroupPath != "" {
		os.RemoveAll(t.cgroupPath) //nolint:errcheck — the helper removes it; this is the crash fallback
	}
	// The next run mounts a fresh rootfs; keep the bundle dir itself, its
	// config.json is rewritten at every direct start.
	os.MkdirAll(t.rootfs, 0o711) //nolint:errcheck
	os.RemoveAll(directBundleDir(t.ns, t.id))
	os.Remove(initPidPath(containerLogPath(t.ns, t.id))) //nolint:errcheck
	return nil
}

// cleanupDirectBundle removes a bundle left by a cold boot (the mount is gone
// with the old VM; only the tmpfs path would remain in this boot, so this is
// mostly a guard for a start failure).
func cleanupDirectBundle(ns, id string) {
	dir := directBundleDir(ns, id)
	if _, err := os.Stat(dir); err == nil {
		unmountAll(directRootfsPath(ns, id))
		os.RemoveAll(dir)
	}
}

// --- log sink ----------------------------------------------------------------

// directLogSink writes the container's json-file log from the helper's
// agent-owned output pipes. Because this process owns the write end of those
// pipes, their EOF is deterministic: finish() flushes the partial tails and
// appends the eof marker the attach/log follow loops end on.
type directLogSink struct {
	out     *rotatingLog
	path    string
	streams []*directStreamWriter
	wg      sync.WaitGroup
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newDirectLogSink(path string, rot logRotation, none bool) (*directLogSink, error) {
	s := &directLogSink{path: path, stop: make(chan struct{}), done: make(chan struct{})}
	if none {
		close(s.done)
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	out, err := openRotatingLog(path, rot)
	if err != nil {
		return nil, err
	}
	s.out = out
	s.streams = []*directStreamWriter{
		{out: out, stream: "stdout"},
		{out: out, stream: "stderr"},
	}
	go s.flushLoop()
	return s, nil
}

// startDirectLog begins copying the helper's output pipes into the log.
func startDirectLog(s *directLogSink, stdoutR, stderrR io.Reader) {
	if s.out == nil {
		// Log driver "none": drain the pipes so the container never blocks
		// on a full pipe.
		go func() { io.Copy(io.Discard, stdoutR) }() //nolint:errcheck
		go func() { io.Copy(io.Discard, stderrR) }() //nolint:errcheck
		return
	}
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.streams[0].copyFrom(stdoutR) }()
	go func() { defer s.wg.Done(); s.streams[1].copyFrom(stderrR) }()
}

func (s *directLogSink) flushLoop() {
	defer close(s.done)
	t := time.NewTicker(partialFlushAfter / 2)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			for _, w := range s.streams {
				w.flushQuiet(now)
			}
		}
	}
}

// finish stops the flusher, drains both copiers (the helper is gone, so
// their pipes are at EOF), flushes the trailing partials and writes the eof
// marker.
func (s *directLogSink) finish() {
	if s.out == nil {
		return
	}
	close(s.stop)
	<-s.done
	waitDone(&s.wg, 2*time.Second)
	for _, w := range s.streams {
		w.drain()
	}
	writeEOFMarker(s.out)
	s.out.Close() //nolint:errcheck
}

// close aborts the sink (a start that never ran): copiers stop with their
// pipes, no marker is written.
func (s *directLogSink) close() {
	if s.out == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		<-s.done
		waitDone(&s.wg, time.Second)
		s.out.Close() //nolint:errcheck
	})
}

// directStreamWriter is one output stream's json-file writer with a local
// partial-line buffer (the process-global partials in logging.go belong to
// the logger subcommands, where one process writes one container's log).
type directStreamWriter struct {
	out    *rotatingLog
	stream string

	mu        sync.Mutex
	buf       []byte
	lastWrite time.Time
}

func (w *directStreamWriter) copyFrom(r io.Reader) {
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			w.write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (w *directStreamWriter) write(data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(data) > 0 {
		idx := indexByte(data, '\n')
		if idx < 0 {
			w.buf = append(w.buf, data...)
			w.lastWrite = time.Now()
			for len(w.buf) >= maxPartialRecord {
				writeLogRecord(w.out, w.stream, w.buf[:maxPartialRecord])
				w.buf = append(w.buf[:0], w.buf[maxPartialRecord:]...)
			}
			return
		}
		line := make([]byte, 0, len(w.buf)+idx+1)
		line = append(line, w.buf...)
		line = append(line, data[:idx+1]...)
		w.buf = w.buf[:0]
		writeLogRecord(w.out, w.stream, line)
		data = data[idx+1:]
	}
}

func (w *directStreamWriter) flushQuiet(now time.Time) {
	w.mu.Lock()
	if len(w.buf) == 0 || now.Sub(w.lastWrite) < partialFlushAfter {
		w.mu.Unlock()
		return
	}
	tail := append([]byte(nil), w.buf...)
	w.buf = w.buf[:0]
	w.mu.Unlock()
	writeLogRecord(w.out, w.stream, tail)
}

func (w *directStreamWriter) drain() {
	w.mu.Lock()
	tail := append([]byte(nil), w.buf...)
	w.buf = w.buf[:0]
	w.mu.Unlock()
	if len(tail) > 0 {
		writeLogRecord(w.out, w.stream, tail)
	}
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// waitDone waits for wg with a bound: a copier stuck on a pipe the helper
// never closed must not hold the exit forever.
func waitDone(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// updateDirectCgroup applies a live `docker update` to a direct task's cgroup
// v2 directory. The init helper owns the creation-time translation
// (guest-shim/cgroup.go writeResourceLimits); with no containerd task there is
// no task.Update, so the same v2 files are written here. A missing file means
// the controller is not enabled for this cgroup: skipped, like the helper.
func updateDirectCgroup(dir string, res *specs.LinuxResources) error {
	if dir == "" || res == nil {
		return nil
	}
	write := func(file, val string) error {
		p := filepath.Join(dir, file)
		if _, err := os.Stat(p); err != nil {
			return nil
		}
		return os.WriteFile(p, []byte(val), 0o600)
	}
	if m := res.Memory; m != nil {
		if m.Limit != nil {
			val := "max"
			if *m.Limit > 0 {
				val = strconv.FormatInt(*m.Limit, 10)
			}
			if err := write("memory.max", val); err != nil {
				return err
			}
		}
		if m.Swap != nil {
			val := "max"
			if *m.Swap > 0 {
				// Docker's --memory-swap is memory+swap; cgroup v2 wants the
				// swap-only value (runc's v1->v2 conversion).
				swap := *m.Swap
				if m.Limit != nil && *m.Limit > 0 {
					swap -= *m.Limit
					if swap < 0 {
						swap = 0
					}
				}
				val = strconv.FormatInt(swap, 10)
			}
			if err := write("memory.swap.max", val); err != nil {
				return err
			}
		}
		if m.Reservation != nil && *m.Reservation > 0 {
			if err := write("memory.low", strconv.FormatInt(*m.Reservation, 10)); err != nil {
				return err
			}
		}
	}
	if c := res.CPU; c != nil {
		period, quota := uint64(100000), int64(0)
		if c.Period != nil && *c.Period != 0 {
			period = *c.Period
		}
		if c.Quota != nil {
			quota = *c.Quota
		}
		cpuMax := "max " + strconv.FormatUint(period, 10)
		if quota > 0 {
			cpuMax = strconv.FormatInt(quota, 10) + " " + strconv.FormatUint(period, 10)
		}
		if err := write("cpu.max", cpuMax); err != nil {
			return err
		}
		if c.Shares != nil && *c.Shares > 0 {
			// runc's shares(2..262144) -> weight(1..10000) conversion.
			weight := 1 + (((*c.Shares - 2) * 9999) / 262142)
			if err := write("cpu.weight", strconv.FormatUint(weight, 10)); err != nil {
				return err
			}
		}
		if c.Cpus != "" {
			if err := write("cpuset.cpus", c.Cpus); err != nil {
				return err
			}
		}
		if c.Mems != "" {
			if err := write("cpuset.mems", c.Mems); err != nil {
				return err
			}
		}
	}
	if p := res.Pids; p != nil && p.Limit != nil {
		// docker's -1 means unlimited: pids.max = max (the creation path
		// skips -1 because "max" is the default, an update must write it).
		val := "max"
		if *p.Limit > 0 {
			val = strconv.FormatInt(*p.Limit, 10)
		}
		if err := write("pids.max", val); err != nil {
			return err
		}
	}
	for k, v := range res.Unified {
		if err := write(k, v); err != nil {
			return err
		}
	}
	return nil
}

// pauseDirectTask freezes/unfreezes a direct container through its cgroup v2
// freezer — the same mechanism the fast shim's Pause/Resume use.
func pauseDirectTask(ns, id string, t *directTask, pause bool) error {
	state, _, _ := t.stateInfo()
	switch {
	case pause && state == directStatePaused:
		return errConflict("container %s is already paused", truncateID(id))
	case !pause && state != directStatePaused:
		return errConflict("container %s is not paused", truncateID(id))
	case state != directStateRunning && state != directStatePaused:
		return errConflict("container %s is not running", truncateID(id))
	}
	if t.cgroupPath == "" {
		return fmt.Errorf("no cgroup for freeze")
	}
	val := "0"
	action := "unpause"
	if pause {
		val, action = "1", "pause"
	}
	if err := os.WriteFile(filepath.Join(t.cgroupPath, "cgroup.freeze"), []byte(val), 0o600); err != nil {
		return err
	}
	t.mu.Lock()
	if pause {
		t.state = directStatePaused
	} else {
		t.state = directStateRunning
	}
	t.mu.Unlock()
	publishContainerEvent(action, ns, id, nil)
	return nil
}

// pidNamespacePids lists every process sharing initPid's pid namespace
// (docker top / stats on direct containers, whose processes containerd does
// not know).
func pidNamespacePids(initPid int) []int {
	want, err := nsLinkInode("/proc/" + strconv.Itoa(initPid) + "/ns/pid")
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil || pid == initPid {
			continue
		}
		if ino, ierr := nsLinkInode("/proc/" + e.Name() + "/ns/pid"); ierr == nil && ino == want {
			out = append(out, pid)
		}
	}
	out = append(out, initPid)
	sort.Ints(out)
	return out
}

// nsLinkInode extracts the namespace inode from a /proc/<pid>/ns/<type>
// symlink target ("pid:[4026531836]").
func nsLinkInode(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	i := strings.IndexByte(target, '[')
	if i < 0 || !strings.HasSuffix(target, "]") {
		return "", fmt.Errorf("unexpected ns link %s", target)
	}
	return target[i+1 : len(target)-1], nil
}
