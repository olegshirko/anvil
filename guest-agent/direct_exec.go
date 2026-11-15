package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/console"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// docker exec / healthchecks on direct (stage 5a) containers. There is no
// containerd task to exec through, so the agent runs the same helper the shim
// uses for execs — `anvil-fast-shim exec-init -initpid <init> -bundle
// <bundle>` with the exec spec in ANVIL_EXEC_SPEC — against the registry's
// init pid. The helper's namespace join, raw fork window and attribute
// application are unchanged; only the process that owns its gate/report/io
// pipes is the agent now.

// containerProcess is the subset of containerd's client.Process the exec
// paths use. Both a containerd exec process and a direct one satisfy it.
type containerProcess interface {
	ID() string
	Pid() uint32
	Start(context.Context) error
	Delete(context.Context, ...client.ProcessDeleteOpts) (*client.ExitStatus, error)
	Kill(context.Context, syscall.Signal, ...client.KillOpts) error
	Wait(context.Context) (<-chan client.ExitStatus, error)
	CloseIO(context.Context, ...client.IOCloserOpts) error
	Resize(ctx context.Context, w, h uint32) error
	IO() cio.IO
	Status(context.Context) (client.Status, error)
}

// mergeDirectExecProcess applies the container defaults an exec inherits
// (runc exec semantics: caps, rlimits, oom score), mirroring the shim's
// mergeExecProcess.
func mergeDirectExecProcess(init *specs.Spec, pspec *specs.Process) *specs.Process {
	merged := *pspec
	if merged.Capabilities == nil && init != nil && init.Process != nil {
		merged.Capabilities = init.Process.Capabilities
	}
	if len(merged.Rlimits) == 0 && init != nil && init.Process != nil {
		merged.Rlimits = init.Process.Rlimits
	}
	if merged.OOMScoreAdj == nil && init != nil && init.Process != nil && init.Process.OOMScoreAdj != nil {
		adj := *init.Process.OOMScoreAdj
		merged.OOMScoreAdj = &adj
	}
	return &merged
}

// newDirectExec starts an exec helper for a direct container and holds it at
// its gate. It returns once the helper reported its setup complete (same
// contract as the shim's Exec RPC: io failures surface here).
func newDirectExec(ns, id string, t *directTask, pspec *specs.Process, terminal bool, stdin io.Reader, stdout, stderr io.Writer) (*directExecProcess, error) {
	initPid, ok := t.pidFor()
	if !ok {
		return nil, fmt.Errorf("container is not running")
	}
	cl, err := pc.get(context.Background())
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(context.Background(), ns)
	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return nil, err
	}
	initSpec, err := c.Spec(nsCtx)
	if err != nil {
		return nil, fmt.Errorf("container spec: %w", err)
	}
	merged := mergeDirectExecProcess(initSpec, pspec)
	merged.Terminal = terminal
	specJSON, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}

	p := &directExecProcess{
		id:        newExecID(),
		initPid:   initPid,
		bundle:    t.bundle,
		stdinSrc:  stdin,
		stdoutDst: stdout,
		stderrDst: stderr,
		state:     client.Created,
		waitCh:    make(chan struct{}),
		startedCh: make(chan error, 1),
		term:      terminal,
	}
	p.io = &directExecIO{p: p}

	gateR, gateW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	reportR, reportW, err := os.Pipe()
	if err != nil {
		gateR.Close()
		gateW.Close()
		return nil, err
	}
	stdinF, stdoutF, stderrF, err := p.childStdio()
	if err != nil {
		gateR.Close()
		gateW.Close()
		reportR.Close()
		reportW.Close()
		p.closeStdio()
		return nil, err
	}
	cmd := exec.Command(fastShimRuntimeName, "exec-init", "-initpid", strconv.Itoa(initPid), "-bundle", t.bundle)
	cmd.ExtraFiles = []*os.File{gateR, reportW, stdinF, stdoutF, stderrF}
	cmd.Env = append(os.Environ(), "ANVIL_EXEC_SPEC="+string(specJSON))
	if err := cmd.Start(); err != nil {
		gateR.Close()
		gateW.Close()
		reportR.Close()
		reportW.Close()
		p.closeStdio()
		return nil, fmt.Errorf("start exec helper: %w", err)
	}
	p.cmd = cmd
	p.pid = uint32(cmd.Process.Pid)
	gateR.Close()
	reportW.Close()
	closeFile(stdinF)
	closeFile(stdoutF)
	closeFile(stderrF)

	// Setup: the helper joins the namespaces and holds at the gate.
	watchdog := time.AfterFunc(15*time.Second, func() { cmd.Process.Kill() }) //nolint:errcheck
	buf := bufio.NewReader(reportR)
	line, rerr := buf.ReadString('\n')
	watchdog.Stop()
	if rerr != nil || strings.TrimRight(line, "\n") != directReportReady {
		detail := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), directReportSetup))
		if rerr != nil {
			detail = rerr.Error()
		}
		cmd.Process.Kill() //nolint:errcheck — the report reader joins it
		gateW.Close()
		p.closeStdio()
		go func() {
			cmd.Wait() //nolint:errcheck
			reportR.Close()
		}()
		return nil, fmt.Errorf("exec setup failed: %s", detail)
	}
	p.gateW = gateW
	p.reportR = reportR
	p.reportBuf = buf
	go p.readReports()
	return p, nil
}

// childStdio builds the helper's stdio fds (5/6/7) and the agent-side copies.
func (p *directExecProcess) childStdio() (stdinF, stdoutF, stderrF *os.File, err error) {
	if p.term {
		// A terminal exec's child gets the pty slave as all three fds — its
		// controlling terminal, which stage B's setsid+TIOCSCTTY takes. The
		// agent-side copies run through the master; stdin is a copy into the
		// master, not a child fd.
		master, slavePath, cerr := console.NewPty()
		if cerr != nil {
			return nil, nil, nil, fmt.Errorf("openpty: %w", cerr)
		}
		slave, cerr := os.OpenFile(slavePath, os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if cerr != nil {
			master.Close()
			return nil, nil, nil, fmt.Errorf("open console slave: %w", cerr)
		}
		p.console = master
		p.consoleSlave = slave
		if p.stdinSrc != nil {
			// Input copy: not awaited by IO().Wait (a client that keeps its
			// stdin open after the process exits must not delay the exec).
			go func() {
				io.Copy(master, p.stdinSrc) //nolint:errcheck
			}()
		}
		p.addOutCopy(func() {
			io.Copy(p.stdoutDst, master) //nolint:errcheck
			master.Close()
		})
		return slave, slave, slave, nil
	}
	if p.stdinSrc != nil {
		r, w, perr := os.Pipe()
		if perr != nil {
			return nil, nil, nil, perr
		}
		p.stdinRead, p.stdinWrite = r, w
		p.addCopy(func() {
			io.Copy(w, p.stdinSrc) //nolint:errcheck
			w.Close()
		})
		stdinF = r
	} else if stdinF, err = os.Open("/dev/null"); err != nil {
		return nil, nil, nil, err
	}
	outR, outW, perr := os.Pipe()
	if perr != nil {
		return nil, nil, nil, perr
	}
	errR, errW, perr := os.Pipe()
	if perr != nil {
		outR.Close()
		outW.Close()
		return nil, nil, nil, perr
	}
	p.stdoutRead, p.stderrRead = outR, errR
	p.addOutCopy(func() {
		io.Copy(p.stdoutDst, outR) //nolint:errcheck
		outR.Close()
	})
	p.addOutCopy(func() {
		io.Copy(p.stderrDst, errR) //nolint:errcheck
		errR.Close()
	})
	return stdinF, outW, errW, nil
}

func (p *directExecProcess) addOutCopy(fn func()) {
	p.outCopies.Add(1)
	go func() {
		defer p.outCopies.Done()
		fn()
	}()
}

func (p *directExecProcess) addCopy(fn func()) {
	p.copies.Add(1)
	go func() {
		defer p.copies.Done()
		fn()
	}()
}

// readReports consumes the exec helper's reports after setup.
func (p *directExecProcess) readReports() {
	defer func() {
		if p.reportR != nil {
			p.reportR.Close() //nolint:errcheck
		}
	}()
	started := false
	for {
		line, err := p.reportBuf.ReadString('\n')
		if err != nil {
			code := waitDirectHelperExit(p.cmd)
			p.mu.Lock()
			if !started {
				started = true
				p.startedCh <- fmt.Errorf("exec helper exited (code %d)", code)
			}
			p.mu.Unlock()
			p.finalize(code)
			return
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, directReportStarted):
			pid, _ := strconv.Atoi(strings.TrimPrefix(line, directReportStarted))
			p.mu.Lock()
			if pid > 0 {
				p.pid = uint32(pid)
			}
			if !started {
				started = true
				p.startedCh <- nil
			}
			p.mu.Unlock()
		case strings.HasPrefix(line, directReportStart), strings.HasPrefix(line, directReportSetup):
			detail := strings.TrimSpace(strings.TrimPrefix(line, directReportStart))
			if strings.HasPrefix(line, directReportSetup) {
				detail = strings.TrimSpace(strings.TrimPrefix(line, directReportSetup))
			}
			p.mu.Lock()
			if !started {
				started = true
				p.startedCh <- fmt.Errorf("%s", detail)
			}
			p.mu.Unlock()
		case strings.HasPrefix(line, directReportDone):
			code, _ := strconv.Atoi(strings.TrimPrefix(line, directReportDone))
			p.mu.Lock()
			p.exitCode = uint32(code)
			p.mu.Unlock()
		}
	}
}

// finalize records the exec exit once.
func (p *directExecProcess) finalize(code int) {
	p.mu.Lock()
	if p.exited {
		p.mu.Unlock()
		return
	}
	p.exited = true
	p.exitCode = uint32(code)
	p.exitedAt = time.Now()
	p.state = client.Stopped
	close(p.waitCh)
	p.mu.Unlock()
	p.closeStdio()
}

func (p *directExecProcess) closeStdio() {
	if p.stdinWrite != nil {
		p.stdinWrite.Close() //nolint:errcheck
	}
	if p.stdinRead != nil {
		p.stdinRead.Close() //nolint:errcheck
	}
	if p.stdoutRead != nil {
		p.stdoutRead.Close() //nolint:errcheck
	}
	if p.stderrRead != nil {
		p.stderrRead.Close() //nolint:errcheck
	}
	if p.consoleSlave != nil {
		p.consoleSlave.Close() //nolint:errcheck
	}
}

// --- client.Process ----------------------------------------------------------

func (p *directExecProcess) ID() string { return p.id }

func (p *directExecProcess) Pid() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pid
}

// Start releases the exec gate and waits for the user exec confirmation.
func (p *directExecProcess) Start(ctx context.Context) error {
	p.mu.Lock()
	gate := p.gateW
	p.mu.Unlock()
	if gate == nil {
		return fmt.Errorf("exec is not created")
	}
	if _, err := gate.WriteString("s\n"); err != nil {
		syscall.Kill(int(p.Pid()), syscall.SIGKILL) //nolint:errcheck
		return err
	}
	select {
	case err := <-p.startedCh:
		if err != nil {
			return err
		}
		p.mu.Lock()
		p.state = client.Running
		p.mu.Unlock()
		return nil
	case <-ctx.Done():
		syscall.Kill(int(p.Pid()), syscall.SIGKILL) //nolint:errcheck
		return ctx.Err()
	case <-time.After(10 * time.Second):
		syscall.Kill(int(p.Pid()), syscall.SIGKILL) //nolint:errcheck
		return fmt.Errorf("exec start timed out")
	}
}

func (p *directExecProcess) Wait(ctx context.Context) (<-chan client.ExitStatus, error) {
	ch := make(chan client.ExitStatus, 1)
	go func() {
		select {
		case <-p.waitCh:
			p.mu.Lock()
			code, at := p.exitCode, p.exitedAt
			p.mu.Unlock()
			ch <- *client.NewExitStatus(code, at, nil)
		case <-ctx.Done():
			ch <- *client.NewExitStatus(client.UnknownExitStatus, time.Now(), ctx.Err())
		}
	}()
	return ch, nil
}

func (p *directExecProcess) Delete(ctx context.Context, _ ...client.ProcessDeleteOpts) (*client.ExitStatus, error) {
	p.mu.Lock()
	code, at, exited := p.exitCode, p.exitedAt, p.exited
	p.mu.Unlock()
	if exited {
		return client.NewExitStatus(code, at, nil), nil
	}
	return client.NewExitStatus(0, time.Now(), nil), nil
}

func (p *directExecProcess) Kill(ctx context.Context, sig syscall.Signal, _ ...client.KillOpts) error {
	p.mu.Lock()
	pid := p.pid
	helper := 0
	if p.cmd != nil && p.cmd.Process != nil {
		helper = p.cmd.Process.Pid
	}
	p.mu.Unlock()
	target := pid
	if target == 0 {
		target = uint32(helper)
	}
	if target == 0 {
		return nil
	}
	if err := syscall.Kill(int(target), sig); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func (p *directExecProcess) CloseIO(ctx context.Context, _ ...client.IOCloserOpts) error {
	if p.stdinWrite != nil {
		return p.stdinWrite.Close()
	}
	return nil
}

func (p *directExecProcess) Resize(ctx context.Context, w, h uint32) error {
	if p.console == nil {
		return fmt.Errorf("no console")
	}
	return unix.IoctlSetWinsize(int(p.console.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(h), Col: uint16(w),
	})
}

func (p *directExecProcess) IO() cio.IO { return p.io }

func (p *directExecProcess) Status(ctx context.Context) (client.Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return client.Status{Status: p.state, ExitStatus: p.exitCode, ExitTime: p.exitedAt}, nil
}

// directExecProcess is one exec run of a direct container.
type directExecProcess struct {
	id        string
	initPid   int
	bundle    string
	stdinSrc  io.Reader
	stdoutDst io.Writer
	stderrDst io.Writer
	term      bool

	cmd       *exec.Cmd
	gateW     *os.File
	reportR   *os.File
	reportBuf *bufio.Reader
	startedCh chan error
	waitCh    chan struct{}
	io        *directExecIO
	copies    sync.WaitGroup
	outCopies sync.WaitGroup

	console      console.Console
	consoleSlave *os.File
	stdinRead    *os.File
	stdinWrite   *os.File
	stdoutRead   *os.File
	stderrRead   *os.File

	mu       sync.Mutex
	pid      uint32
	state    client.ProcessStatus
	exitCode uint32
	exitedAt time.Time
	exited   bool
}

// directExecIO is the cio.IO the exec call sites drain after Wait.
type directExecIO struct {
	p *directExecProcess
}

func (d *directExecIO) Config() cio.Config { return cio.Config{} }
func (d *directExecIO) Cancel()            {}
func (d *directExecIO) Wait()              { waitDone(&d.p.outCopies, 2*time.Second) }
func (d *directExecIO) Close() error       { return nil }

// containerExecProcess returns an exec process for the container: a direct
// helper for an agent-supervised container, containerd's task.Exec otherwise.
func containerExecProcess(ctx context.Context, ns, id string, pspec *specs.Process, terminal bool, stdin io.Reader, stdout, stderr io.Writer) (containerProcess, error) {
	if t, ok := supervisor.lookup(ns, id); ok {
		if _, running := t.pidFor(); !running {
			return nil, fmt.Errorf("container is not running")
		}
		return newDirectExec(ns, id, t, pspec, terminal, stdin, stdout, stderr)
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return nil, err
	}
	task, err := c.Task(nsCtx, nil)
	if err != nil {
		return nil, fmt.Errorf("task: %w", err)
	}
	process, err := task.Exec(nsCtx, newExecID(), pspec, cio.NewCreator(cio.WithStreams(stdin, stdout, stderr)))
	if err != nil {
		return nil, err
	}
	return process, nil
}
