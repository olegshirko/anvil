package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

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
}

func (s *execSpec) setExit(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.exitCode = code
}

func (s *execSpec) state() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running, s.exitCode
}

// execStore keeps pending and finished exec instances keyed by Docker exec ID.
type execStore struct {
	mu   sync.RWMutex
	byID map[string]*execSpec
}

func newExecStore() *execStore {
	return &execStore{byID: make(map[string]*execSpec)}
}

func (s *execStore) add(spec *execSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[spec.ID] = spec
}

func (s *execStore) get(id string) *execSpec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byID[id]
}

func newExecID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback to a timestamp-based ID if randomness fails.
		return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano()))))[:32]
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
	ExitCode      int    `json:"ExitCode"`
	OpenStdin     bool   `json:"OpenStdin"`
	OpenStdout    bool   `json:"OpenStdout"`
	OpenStderr    bool   `json:"OpenStderr"`
	CanRemove     bool   `json:"CanRemove"`
	ContainerID   string `json:"ContainerID"`
	ProcessConfig struct {
		Tty        bool     `json:"tty"`
		Entrypoint string   `json:"entrypoint"`
		Arguments  []string `json:"arguments"`
	} `json:"ProcessConfig"`
}

// createDockerExec creates an exec instance and returns its Docker-compatible ID.
func createDockerExec(ctx context.Context, containerID string, req dockerExecCreateRequest) (string, error) {
	ns, containerdID, name, err := resolveDockerID(ctx, containerID)
	if err != nil {
		return "", err
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
		res, err := runSimpleExecStdin(context.Background(), spec.Namespace,
			spec.ContainerdID, spec.Cmd, spec.User, spec.WorkingDir, nil, time.Hour)
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
		http.Error(w, fmt.Sprintf(`{"message":"No such exec instance: %s"}`, id), http.StatusNotFound)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, `{"message":"hijacking not supported"}`, http.StatusInternalServerError)
		return
	}

	conn, bufrw, err := hj.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	// Write the upgrade response. Docker CLI expects 101 UPGRADED for attach.
	fmt.Fprintf(bufrw, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	if err := bufrw.Flush(); err != nil {
		return
	}

	spec.mu.Lock()
	spec.running = true
	spec.exitCode = 0
	spec.mu.Unlock()

	// Native exec: task.Exec inside the container's task, with the hijacked
	// connection wired to the process stdio. Docker's hijacked attach
	// protocol sends client stdin as a raw byte stream in both TTY and
	// non-TTY modes (only the output direction is multiplexed).
	cl, cerr := pc.get(context.Background())
	if cerr != nil {
		spec.setExit(126)
		return
	}
	nsCtx := namespaces.WithNamespace(context.Background(), spec.Namespace)
	container, lerr := cl.LoadContainer(nsCtx, spec.ContainerdID)
	if lerr != nil {
		spec.setExit(126)
		return
	}
	task, terr := container.Task(nsCtx, nil)
	if terr != nil {
		spec.setExit(126)
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
	if spec.AttachStdin || spec.Tty {
		pr, pw := io.Pipe()
		stdinR = pr
		stdinWriteCloser = pw
		go func() {
			io.Copy(pw, conn)
			pw.Close()
		}()
	}

	pspec := &specs.Process{
		Args: spec.Cmd,
		Env:  mergeEnv([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, spec.Env),
		Cwd:  defaultString(spec.WorkingDir, "/"),
		User: execUserFor(nsCtx, container, spec.User),
		Terminal: spec.Tty,
	}

	execID := newExecID()
	process, xerr := task.Exec(nsCtx, execID, pspec, cio.NewCreator(cio.WithStreams(stdinR, stdoutW, stderrW)))
	if xerr != nil {
		stdoutR.Close(); stdoutW.Close(); stderrR.Close(); stderrW.Close()
		spec.setExit(126)
		return
	}

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
		wg.Wait()
		stdoutR.Close()
		stderrR.Close()
		spec.setExit(126)
		return
	}

	exitCh, werr := process.Wait(nsCtx)
	if werr != nil {
		wg.Wait()
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
	process.Delete(context.Background()) //nolint:errcheck
	stdoutR.Close()
	stderrR.Close()
	spec.setExit(exitCode)

	bufrw.Flush()
	time.Sleep(50 * time.Millisecond)
}

// inspectDockerExec returns the running/exit state of an exec instance.
func inspectDockerExec(id string) (*dockerExecInspectResponse, error) {
	spec := execs.get(id)
	if spec == nil {
		return nil, fmt.Errorf("No such exec instance: %s", id)
	}
	running, code := spec.state()
	resp := &dockerExecInspectResponse{
		ID:          id,
		Running:     running,
		ExitCode:    code,
		OpenStdin:   spec.AttachStdin,
		OpenStdout:  spec.AttachStdout,
		OpenStderr:  spec.AttachStderr,
		CanRemove:   !running,
		ContainerID: spec.ContainerDockerID,
	}
	resp.ProcessConfig.Tty = spec.Tty
	if len(spec.Cmd) > 0 {
		resp.ProcessConfig.Entrypoint = spec.Cmd[0]
		resp.ProcessConfig.Arguments = spec.Cmd[1:]
	}
	return resp, nil
}
