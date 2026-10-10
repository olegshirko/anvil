package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// execSpec holds the configuration for a created exec instance.
type execSpec struct {
	ID                string
	Namespace         string
	ContainerdID      string
	ContainerName     string
	ContainerDockerID string
	Cmd               []string
	Env               []string
	User              string
	WorkingDir        string
	AttachStdin       bool
	AttachStdout      bool
	AttachStderr      bool
	Tty               bool
	Privileged        bool

	mu       sync.Mutex
	running  bool
	exitCode int
	// finished is when the exec exited; finished execs are pruned from the
	// store after execRetention.
	finished time.Time
	// proc is the containerd exec process while it runs; kept for TTY resize
	// (POST /exec/{id}/resize) between start and exit.
	proc client.Process
	// pid is the exec process's pid (kept after exit, as Docker does).
	pid uint32
}

// setProcess records (or clears with nil) the live containerd exec process.
func (s *execSpec) setProcess(p client.Process) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proc = p
	if p != nil {
		s.pid = p.Pid()
	}
}

// currentProcess returns the live exec process, or nil when not running.
func (s *execSpec) currentProcess() client.Process {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc
}

func (s *execSpec) setExit(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.exitCode = code
	s.finished = time.Now()
}

// finishedBefore reports whether the exec exited before t.
func (s *execSpec) finishedBefore(t time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.running && !s.finished.IsZero() && s.finished.Before(t)
}

// inspectState is what exec inspect reports: the exit code only once the
// process has exited (null before, as in Docker), and its pid.
func (s *execSpec) inspectState() (running bool, exitCode *int, pid uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running && !s.finished.IsZero() {
		code := s.exitCode
		exitCode = &code
	}
	return s.running, exitCode, s.pid
}

// execStore keeps pending and finished exec instances keyed by Docker exec ID.
type execStore struct {
	mu   sync.RWMutex
	byID map[string]*execSpec
}

func newExecStore() *execStore {
	return &execStore{byID: make(map[string]*execSpec)}
}

// execRetention is how long a finished exec stays inspectable. Every
// `docker exec` and every healthcheck probe adds one; without pruning the
// store grew for the agent's whole lifetime.
const execRetention = 10 * time.Minute

func (s *execStore) add(spec *execSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-execRetention)
	for id, e := range s.byID {
		if e.finishedBefore(cutoff) {
			delete(s.byID, id)
		}
	}
	s.byID[spec.ID] = spec
}

// forgetContainer drops every exec of a removed container.
func (s *execStore) forgetContainer(ns, containerdID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.byID {
		if e.Namespace == ns && e.ContainerdID == containerdID {
			delete(s.byID, id)
		}
	}
}

func (s *execStore) get(id string) *execSpec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byID[id]
}

// newExecID returns a 64-hex-digit ID, the length Docker's exec IDs have.
func newExecID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// Fallback to a timestamp-based ID if randomness fails.
		return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano()))))
	}
	return fmt.Sprintf("%x", b)
}

var execs = newExecStore()

// dockerExecCreateRequest mirrors Docker's POST /containers/{id}/exec body.
type dockerExecCreateRequest struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env"`
	User         string   `json:"User"`
	WorkingDir   string   `json:"WorkingDir"`
	Privileged   bool     `json:"Privileged"`
}

// dockerExecCreateResponse mirrors Docker's exec create response.
type dockerExecCreateResponse struct {
	Id string `json:"Id"`
}

// dockerExecStartRequest mirrors Docker's POST /exec/{id}/start body.
type dockerExecStartRequest struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

// dockerExecInspectResponse mirrors Docker's GET /exec/{id}/json response.
type dockerExecInspectResponse struct {
	ID            string `json:"ID"`
	Running       bool   `json:"Running"`
	ExitCode      *int   `json:"ExitCode"`
	Pid           uint32 `json:"Pid"`
	DetachKeys    string `json:"DetachKeys"`
	OpenStdin     bool   `json:"OpenStdin"`
	OpenStdout    bool   `json:"OpenStdout"`
	OpenStderr    bool   `json:"OpenStderr"`
	CanRemove     bool   `json:"CanRemove"`
	ContainerID   string `json:"ContainerID"`
	ProcessConfig struct {
		Tty        bool     `json:"tty"`
		Entrypoint string   `json:"entrypoint"`
		Arguments  []string `json:"arguments"`
		User       string   `json:"user,omitempty"`
		Privileged bool     `json:"privileged"`
	} `json:"ProcessConfig"`
}

// createDockerExec creates an exec instance and returns its Docker-compatible ID.
func createDockerExec(ctx context.Context, containerID string, req dockerExecCreateRequest) (string, error) {
	if len(req.Cmd) == 0 {
		return "", errInvalid("No exec command specified")
	}
	ns, containerdID, name, err := resolveDockerID(ctx, containerID)
	if err != nil {
		return "", err
	}
	if st, known := currentTaskStatus(ctx, ns, containerdID); known && st != "running" {
		if st == "paused" {
			return "", errConflict("Container %s is paused, unpause the container before exec", truncateID(dockerID(ns, containerdID)))
		}
		return "", errConflict("container %s is not running", truncateID(dockerID(ns, containerdID)))
	}

	spec := &execSpec{
		ID:                newExecID(),
		Namespace:         ns,
		ContainerdID:      containerdID,
		ContainerName:     name,
		ContainerDockerID: dockerID(ns, containerdID),
		Cmd:               req.Cmd,
		Env:               req.Env,
		User:              req.User,
		WorkingDir:        req.WorkingDir,
		AttachStdin:       req.AttachStdin,
		AttachStdout:      req.AttachStdout,
		AttachStderr:      req.AttachStderr,
		Tty:               req.Tty,
		Privileged:        req.Privileged,
	}
	execs.add(spec)
	publishContainerEvent("exec_create: "+strings.Join(spec.Cmd, " "), spec.Namespace, spec.ContainerdID,
		map[string]string{"execID": spec.ID})
	return spec.ID, nil
}

// startDetachedExec runs an exec instance in the background and returns
// immediately (the process keeps running inside the container).
func startDetachedExec(id string) error {
	spec := execs.get(id)
	if spec == nil {
		return fmt.Errorf("No such exec instance: %s", id)
	}
	go func() {
		res, err := runSimpleExecEnv(context.Background(), spec.Namespace,
			spec.ContainerdID, spec.Cmd, spec.User, spec.WorkingDir, spec.Env, nil, -1)
		if err != nil {
			spec.setExit(126)
			return
		}
		spec.setExit(res.exitCode)
	}()
	return nil
}

// handleExecStart hijacks the HTTP connection, runs a containerd exec process, and streams
// stdout/stderr using Docker's raw-stream multiplexing format.
func handleExecStart(w http.ResponseWriter, r *http.Request, id string) {
	spec := execs.get(id)
	if spec == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("No such exec instance: %s", id))
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "hijacking not supported")
		return
	}

	conn, bufrw, err := hj.Hijack()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer conn.Close()

	if err := writeHijackHeader(bufrw, r, spec.Tty); err != nil {
		return
	}

	spec.mu.Lock()
	spec.running = true
	spec.exitCode = 0
	spec.mu.Unlock()

	// failExec reports a failure to start the process the way Docker does:
	// the reason on the client's stderr ("OCI runtime exec failed: ...")
	// and in the log, exit code 126.
	failExec := func(stage string, err error) {
		msg := fmt.Sprintf("OCI runtime exec failed: %s: %v\n", stage, err)
		log.Printf("[exec] %s in %s: %s", truncateID(id), truncateID(spec.ContainerdID), strings.TrimSpace(msg))
		if spec.Tty {
			bufrw.WriteString(msg)
		} else {
			writeDockerStream(bufrw, 2, []byte(msg))
		}
		bufrw.Flush()
		spec.setExit(126)
	}

	// Native exec: task.Exec inside the container's task, with the hijacked
	// connection wired to the process stdio. Docker's hijacked attach
	// protocol sends client stdin as a raw byte stream in both TTY and
	// non-TTY modes (only the output direction is multiplexed).
	cl, cerr := pc.get(context.Background())
	if cerr != nil {
		failExec("containerd", cerr)
		return
	}
	nsCtx := namespaces.WithNamespace(context.Background(), spec.Namespace)
	container, lerr := cl.LoadContainer(nsCtx, spec.ContainerdID)
	if lerr != nil {
		failExec("load container", lerr)
		return
	}
	task, terr := container.Task(nsCtx, nil)
	if terr != nil {
		failExec("container is not running", terr)
		return
	}

	stdoutR, stdoutW, perr := os.Pipe()
	if perr != nil {
		spec.setExit(126)
		return
	}
	stderrR, stderrW, perr := os.Pipe()
	if perr != nil {
		stdoutR.Close()
		stdoutW.Close()
		spec.setExit(126)
		return
	}

	var stdinR io.Reader
	var stdinWriteCloser io.WriteCloser
	// started is closed once the process runs, finished when this handler
	// returns: the stdin copier needs the process for CloseIO.
	started, finished := make(chan struct{}), make(chan struct{})
	defer close(finished)
	var proc client.Process
	// Stdin only with -i, as dockerd does. The CLI half-closes the
	// connection of an exec without -i at once; with a stdin pipe that EOF
	// reached the shim through CloseIO, and for a TTY the shim then shuts
	// the pty down: `docker exec -t c tty` died of SIGHUP (exit 129) when
	// the EOF came before the process finished.
	if spec.AttachStdin {
		pr, pw := io.Pipe()
		stdinR = pr
		stdinWriteCloser = pw
		go func() {
			// bufrw, not conn: bytes the client sent right behind the
			// request are already in its buffer.
			io.Copy(pw, eagerReader(bufrw.Reader, stdinBufferLimit))
			pw.Close()
			// The client half-closed (stdin EOF). Closing our pipe ends the
			// FIFO copy, but the shim keeps its own writer on the FIFO, so
			// the process sees EOF only after CloseIO — what dockerd does.
			// Without it `echo x | docker exec -i c cat` never returned.
			select {
			case <-started:
			case <-finished:
				return
			}
			ctx, cancel := context.WithTimeout(nsCtx, 10*time.Second)
			defer cancel()
			if err := proc.CloseIO(ctx, client.WithStdinCloser); err != nil {
				debugLog("[exec] close stdin of %s: %v", truncateID(id), err)
			}
		}()
	}

	user, uerr := execUserFor(nsCtx, container, task, spec.User)
	if uerr != nil {
		failExec("exec", uerr)
		return
	}
	ctrEnv, ctrCwd := containerProcessDefaults(nsCtx, container)
	pspec := &specs.Process{
		Args:     spec.Cmd,
		Env:      mergeEnv(ctrEnv, spec.Env),
		Cwd:      defaultString(spec.WorkingDir, ctrCwd),
		User:     user,
		Terminal: spec.Tty,
	}

	execID := newExecID()
	// A TTY exec needs Terminal in the IO config as well as in the spec:
	// only then does the shim hand runc a console socket and allocate the
	// pty. With the spec flag alone runc started the process on plain pipes
	// (`docker exec -it` shells reported "not a tty").
	ioOpts := []cio.Opt{cio.WithStreams(stdinR, stdoutW, stderrW)}
	if spec.Tty {
		ioOpts = append(ioOpts, cio.WithTerminal)
	}
	process, xerr := task.Exec(nsCtx, execID, pspec, cio.NewCreator(ioOpts...))
	if xerr != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		failExec("exec", xerr)
		return
	}
	spec.setProcess(process)

	var wg sync.WaitGroup
	writeMu := &sync.Mutex{}
	stream := func(rc io.ReadCloser, streamType byte) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, rerr := rc.Read(buf)
			if n > 0 {
				writeMu.Lock()
				if spec.Tty {
					bufrw.Write(buf[:n])
				} else {
					writeDockerStream(bufrw, streamType, buf[:n])
				}
				bufrw.Flush()
				writeMu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}
	wg.Add(2)
	go stream(stdoutR, 1)
	go stream(stderrR, 2)

	if serr := process.Start(nsCtx); serr != nil {
		process.Delete(context.WithoutCancel(nsCtx)) //nolint:errcheck
		stdoutW.Close()
		stderrW.Close()
		wg.Wait()
		stdoutR.Close()
		stderrR.Close()
		failExec("start", serr)
		return
	}
	spec.setProcess(process) // again: the pid exists only now
	proc = process
	close(started)
	publishContainerEvent("exec_start: "+strings.Join(spec.Cmd, " "), spec.Namespace, spec.ContainerdID,
		map[string]string{"execID": spec.ID})

	exitCh, werr := process.Wait(nsCtx)
	if werr != nil {
		process.Kill(context.WithoutCancel(nsCtx), syscall.SIGKILL)          //nolint:errcheck
		process.Delete(context.WithoutCancel(nsCtx), client.WithProcessKill) //nolint:errcheck
		// The stream readers end only on EOF of their pipes.
		stdoutW.Close()
		stderrW.Close()
		if stdinWriteCloser != nil {
			stdinWriteCloser.Close()
		}
		wg.Wait()
		stdoutR.Close()
		stderrR.Close()
		spec.setExit(126)
		return
	}
	st := <-exitCh

	// Wait for the client-side fifo copy goroutines to drain the process
	// output into our pipes, then close the write ends so the stream
	// readers see EOF and finish.
	process.IO().Wait()
	stdoutW.Close()
	stderrW.Close()
	if stdinWriteCloser != nil {
		stdinWriteCloser.Close()
	}
	wg.Wait()

	exitCode := 0
	if serr := st.Error(); serr != nil {
		exitCode = 126
	} else {
		exitCode = int(st.ExitCode())
	}
	spec.setProcess(nil)
	process.Delete(context.WithoutCancel(nsCtx)) //nolint:errcheck — namespaced: a bare context is rejected and leaks the record
	stdoutR.Close()
	stderrR.Close()
	spec.setExit(exitCode)
	publishContainerEvent("exec_die", spec.Namespace, spec.ContainerdID,
		map[string]string{"execID": spec.ID, "exitCode": strconv.Itoa(exitCode)})

	bufrw.Flush()
	time.Sleep(50 * time.Millisecond)
}

// inspectDockerExec returns the running/exit state of an exec instance.
func inspectDockerExec(id string) (*dockerExecInspectResponse, error) {
	spec := execs.get(id)
	if spec == nil {
		return nil, fmt.Errorf("No such exec instance: %s", id)
	}
	running, code, pid := spec.inspectState()
	resp := &dockerExecInspectResponse{
		ID:          id,
		Running:     running,
		ExitCode:    code,
		Pid:         pid,
		OpenStdin:   spec.AttachStdin,
		OpenStdout:  spec.AttachStdout,
		OpenStderr:  spec.AttachStderr,
		CanRemove:   !running,
		ContainerID: spec.ContainerDockerID,
	}
	resp.ProcessConfig.Tty = spec.Tty
	resp.ProcessConfig.User = spec.User
	resp.ProcessConfig.Privileged = spec.Privileged
	if len(spec.Cmd) > 0 {
		resp.ProcessConfig.Entrypoint = spec.Cmd[0]
		resp.ProcessConfig.Arguments = spec.Cmd[1:]
	}
	return resp, nil
}
