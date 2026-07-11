package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// execSpec holds the configuration for a created exec instance.
type execSpec struct {
	ID                string
	Namespace         string
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
func createDockerExec(containerID string, req dockerExecCreateRequest) (string, error) {
	ns, containerdID, name, err := resolveDockerID(containerID)
	if err != nil {
		return "", err
	}

	spec := &execSpec{
		ID:                newExecID(),
		Namespace:         ns,
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

// buildNerdctlExecArgs builds the nerdctl exec argument slice for a spec.
func buildNerdctlExecArgs(spec *execSpec, detach bool) []string {
	args := []string{"exec"}
	if detach {
		args = append(args, "-d")
	}
	if spec.Tty {
		args = append(args, "-t")
	}
	// Only request interactive when stdin is wanted and we are not detaching.
	if spec.AttachStdin && !detach {
		args = append(args, "-i")
	}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	if spec.User != "" {
		args = append(args, "-u", spec.User)
	}
	if spec.WorkingDir != "" {
		args = append(args, "-w", spec.WorkingDir)
	}
	if spec.Privileged {
		args = append(args, "--privileged")
	}
	args = append(args, spec.ContainerName)
	args = append(args, spec.Cmd...)
	return args
}

// startDetachedExec runs an exec instance in the background and returns immediately.
func startDetachedExec(id string) error {
	spec := execs.get(id)
	if spec == nil {
		return fmt.Errorf("No such exec instance: %s", id)
	}

	args := buildNerdctlExecArgs(spec, true)
	stdout, stderr, code, err := runNerdctl(spec.Namespace, args...)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl exec failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	return nil
}

// handleExecStart hijacks the HTTP connection, runs nerdctl exec, and streams
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

	args := buildNerdctlExecArgs(spec, false)
	cmd := exec.Command("/opt/containerd/bin/nerdctl", append([]string{"-n", spec.Namespace}, args...)...)
	cmd.Env = append(cmd.Env, "PATH=/bin:/sbin:/usr/bin:/usr/sbin")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		spec.setExit(126)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		spec.setExit(126)
		return
	}

	stdinPipe, stdinWriter := io.Pipe()
	if spec.AttachStdin || spec.Tty {
		cmd.Stdin = stdinPipe
		go func() {
			// In TTY mode the client sends raw bytes; in non-TTY mode it sends
			// multiplexed frames. For the first implementation we forward raw
			// bytes only in TTY mode and discard stdin otherwise to avoid sending
			// Docker frame headers to the container process.
			if spec.Tty {
				io.Copy(stdinWriter, conn)
			} else {
				io.Copy(io.Discard, conn)
			}
			stdinWriter.Close()
		}()
	} else {
		stdinWriter.Close()
		stdinPipe.Close()
	}

	if err := cmd.Start(); err != nil {
		spec.setExit(126)
		return
	}

	var wg sync.WaitGroup
	writeMu := &sync.Mutex{}
	var (
		parsedExit   = -1
		parsedExitMu sync.Mutex
		execExitRe   = regexp.MustCompile(`exec failed with exit code (\d+)`)
	)

	stream := func(rc io.ReadCloser, streamType byte) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := rc.Read(buf)
			if n > 0 {
				if spec.Tty {
					writeMu.Lock()
					bufrw.Write(buf[:n])
					bufrw.Flush()
					writeMu.Unlock()
				} else {
					writeMu.Lock()
					writeDockerStream(bufrw, streamType, buf[:n])
					bufrw.Flush()
					writeMu.Unlock()
				}
			}
			if err != nil {
				return
			}
		}
	}

	// Scan stderr line-by-line so we can extract the real command exit code
	// from nerdctl's fatal message while still streaming it to the client.
	scanStderr := func(rc io.ReadCloser) {
		defer wg.Done()
		scanner := bufio.NewScanner(rc)
		for scanner.Scan() {
			line := scanner.Bytes()
			out := append([]byte{}, line...)
			out = append(out, '\n')
			writeMu.Lock()
			writeDockerStream(bufrw, 2, out)
			bufrw.Flush()
			writeMu.Unlock()
			if m := execExitRe.FindSubmatch(line); len(m) > 1 {
				if c, err := strconv.Atoi(string(m[1])); err == nil {
					parsedExitMu.Lock()
					parsedExit = c
					parsedExitMu.Unlock()
				}
			}
		}
	}

	wg.Add(2)
	go stream(stdout, 1)
	go scanStderr(stderr)

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			parsedExitMu.Lock()
			if parsedExit >= 0 {
				exitCode = parsedExit
			}
			parsedExitMu.Unlock()
		} else {
			exitCode = 126
		}
	}
	spec.setExit(exitCode)

	wg.Wait()
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
