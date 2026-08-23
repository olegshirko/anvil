package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"net/url"

	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Native task lifecycle: start (CNI attach + json-file logging), stop
// (signal escalation), delete (full cleanup) and a one-shot exec primitive.
// Companion to container_runtime.go.

// --- runtime state ----------------------------------------------------------

// containerNetInfo is the persisted CNI result for a running container.
type containerNetInfo struct {
	IP      string `json:"IP"`
	Mac     string `json:"Mac,omitempty"`
	Network string `json:"Network"`
}

func saveNetInfo(ns, id string, ni containerNetInfo) error {
	data, err := json.Marshal(ni)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(containerMetaDir(ns, id), "net.json"), data, 0o644)
}

func loadNetInfo(ns, id string) (containerNetInfo, bool) {
	data, err := os.ReadFile(filepath.Join(containerMetaDir(ns, id), "net.json"))
	if err != nil {
		return containerNetInfo{}, false
	}
	var ni containerNetInfo
	if json.Unmarshal(data, &ni) != nil {
		return containerNetInfo{}, false
	}
	return ni, true
}

func removeNetInfo(ns, id string) {
	os.Remove(filepath.Join(containerMetaDir(ns, id), "net.json"))
}

// usesHostNetworkName reports whether a logical network name means
// host-networking (no CNI attachment).
func usesHostNetworkName(netName string) bool {
	return netName == "" || netName == "host"
}

// --- start ------------------------------------------------------------------

// startNativeTask attaches CNI networking, creates the task with json-file
// logging and starts it. A leftover stopped task from a previous run is
// deleted first.
func startNativeTask(ctx context.Context, ns, id string) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)

	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return fmt.Errorf("load container: %w", err)
	}

	// A stopped task left over from a previous run/restart must go first.
	if old, terr := c.Task(nsCtx, nil); terr == nil {
		if st, serr := old.Status(nsCtx); serr == nil && st.Status == "running" {
			return fmt.Errorf("container is already running")
		}
		dctx, cancel := context.WithTimeout(nsCtx, 5*time.Second)
		old.Delete(dctx, client.WithProcessKill) //nolint:errcheck
		cancel()
	}

	meta, merr := loadContainerMeta(ns, id)
	netName := ""
	var ports []cniPortMapping
	if merr == nil && len(meta.Networks) > 0 {
		netName = meta.Networks[0]
		ports = meta.Ports
	}

	if !usesHostNetworkName(netName) {
		ip, mac, aerr := attachNetwork(ctx, netName, ns, id, netnsPathFor(id), ports)
		if aerr != nil {
			return fmt.Errorf("cni attach: %w", aerr)
		}
		saveNetInfo(ns, id, containerNetInfo{IP: ip, Mac: mac, Network: netName})
		defer func() {
			if err != nil {
				dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				detachNetwork(dctx, netName, ns, id, netnsPathFor(id), ports) //nolint:errcheck
				cancel()
				removeNetInfo(ns, id)
			}
		}()
	}

	uri, lerr := taskLogURI(containerLogPath(ns, id))
	if lerr != nil {
		err = lerr
		return err
	}
	// TTY tasks need Terminal set in the IO config: the shim then allocates
	// the pty itself (runc refuses a terminal spec with no console socket
	// otherwise) and duplicates the console output into the log URI.
	tty := getContainerTTY(dockerID(ns, id))
	task, terr := c.NewTask(nsCtx, func(string) (cio.IO, error) {
		return &logOnlyIO{uri: uri, terminal: tty}, nil
	})
	if terr != nil {
		err = fmt.Errorf("new task: %w", terr)
		return err
	}
	if serr := task.Start(nsCtx); serr != nil {
		task.Delete(context.Background()) //nolint:errcheck
		err = fmt.Errorf("start task: %w", serr)
		return err
	}

	go watchTaskExit(context.Background(), ns, id, netName, ports)
	return nil
}

// watchTaskExit waits for the task to die and performs teardown Docker
// semantics expect: exit-code cache, health monitor stop and CNI detach (the
// IP is released on stop, exactly like docker stop).
func watchTaskExit(ctx context.Context, ns, id, netName string, ports []cniPortMapping) {
	cl, err := pc.get(ctx)
	if err != nil {
		return
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return
	}
	task, err := c.Task(nsCtx, nil)
	if err != nil {
		return
	}
	exitCh, werr := task.Wait(nsCtx)
	if werr != nil {
		return
	}
	st := <-exitCh

	code := 0
	if serr := st.Error(); serr != nil {
		debugLog("[runtime] wait error for %s: %v", truncateID(id), serr)
	} else {
		code = int(st.ExitCode())
	}

	did := dockerID(ns, id)
	cacheContainerExitCode(did, code)
	stopHealthCheck(did)

	if !usesHostNetworkName(netName) {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if derr := detachNetwork(dctx, netName, ns, id, netnsPathFor(id), ports); derr != nil {
			debugLog("[cni] detach %s/%s: %v", ns, truncateID(id), derr)
		}
		cancel()
		removeNetInfo(ns, id)
	}
	debugLog("[runtime] task %s/%s exited code=%d", ns, truncateID(id), code)
}

// --- stop -------------------------------------------------------------------

// stopNativeTask sends the configured stop signal and escalates to SIGKILL
// after the grace timeout. The (stopped) task record is kept for status
// reporting until the container is started again or deleted.
func stopNativeTask(ctx context.Context, ns, id string, timeoutSec int) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return err
	}
	task, terr := c.Task(nsCtx, nil)
	if terr != nil {
		teardownNetwork(ctx, ns, id)
		return nil // no task: nothing to stop
	}
	st, serr := task.Status(nsCtx)
	if serr != nil {
		return serr
	}
	if st.Status != "running" && st.Status != "paused" {
		teardownNetwork(ctx, ns, id)
		return nil
	}

	sig := syscall.SIGTERM
	if meta, merr := loadContainerMeta(ns, id); merr == nil && meta.StopSignal != "" {
		if s, ok := signalValue(meta.StopSignal); ok {
			sig = s
		}
	}
	if kerr := task.Kill(nsCtx, sig); kerr != nil {
		return kerr
	}
	if timeoutSec <= 0 {
		timeoutSec = 10
	}
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		cur, err := task.Status(nsCtx)
		if err != nil {
			break // task gone
		}
		if cur.Status == "stopped" {
			break
		}
	}
	if stopped, serr := task.Status(nsCtx); serr == nil && stopped.Status != "stopped" {
		kctx, cancel := context.WithTimeout(nsCtx, 5*time.Second)
		if kerr := task.Kill(kctx, syscall.SIGKILL); kerr != nil {
			cancel()
			return fmt.Errorf("force kill: %w", kerr)
		}
		cancel()
		deadline = time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			cur, err := task.Status(nsCtx)
			if err != nil || cur.Status == "stopped" {
				break
			}
		}
	}
	// Detach CNI synchronously: the exit watcher may fire later, and a
	// leftover host-side veth makes the next start's CNI attach fail with
	// "veth ... already exists" (docker restart / compose restart).
	teardownNetwork(ctx, ns, id)
	return nil
}

// teardownNetwork detaches the container's CNI endpoint if one is recorded.
// Best effort and idempotent (a late exit-watcher detach on an already
// detached endpoint is harmless).
func teardownNetwork(ctx context.Context, ns, id string) {
	ni, ok := loadNetInfo(ns, id)
	if !ok || usesHostNetworkName(ni.Network) {
		return
	}
	meta, _ := loadContainerMeta(ns, id)
	var ports []cniPortMapping
	if meta != nil {
		ports = meta.Ports
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	detachNetwork(dctx, ni.Network, ns, id, netnsPathFor(id), ports) //nolint:errcheck
	cancel()
	removeNetInfo(ns, id)
}

// logOnlyIO mirrors cio.LogURI but can flag the IO config as a terminal,
// which the stock LogURI creator cannot do.
type logOnlyIO struct {
	uri      *url.URL
	terminal bool
}

func (l *logOnlyIO) Config() cio.Config {
	return cio.Config{
		Terminal: l.terminal,
		Stdout:   l.uri.String(),
		Stderr:   l.uri.String(),
	}
}

func (l *logOnlyIO) Cancel()      {}
func (l *logOnlyIO) Wait()        {}
func (l *logOnlyIO) Close() error { return nil }

// --- delete -----------------------------------------------------------------

// deleteNativeContainer removes the task (if any), the container record, its
// rootfs snapshot, CNI attachment, named netns and anvil metadata. Anonymous
// volumes are removed with the container (--rm semantics).
func deleteNativeContainer(ctx context.Context, ns, id string, force bool) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)

	meta, _ := loadContainerMeta(ns, id)

	if c, cerr := cl.LoadContainer(nsCtx, id); cerr == nil {
		if task, terr := c.Task(nsCtx, nil); terr == nil {
			if st, serr := task.Status(nsCtx); serr == nil && st.Status == "running" {
				if !force {
					return fmt.Errorf("cannot remove running container without force")
				}
				kctx, kcancel := context.WithTimeout(nsCtx, 5*time.Second)
				task.Kill(kctx, syscall.SIGKILL) //nolint:errcheck
				kcancel()
				waitDeadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(waitDeadline) {
					time.Sleep(50 * time.Millisecond)
					if cur, err := task.Status(nsCtx); err != nil || cur.Status == "stopped" {
						break
					}
				}
			}
			dctx, dcancel := context.WithTimeout(nsCtx, 5*time.Second)
			task.Delete(dctx) //nolint:errcheck — best effort; record removal proceeds
			dcancel()
		}
		rmctx, rmcancel := context.WithTimeout(nsCtx, 10*time.Second)
		if derr := c.Delete(rmctx, client.WithSnapshotCleanup); derr != nil {
			rmcancel()
			return fmt.Errorf("delete container: %w", derr)
		}
		rmcancel()
	}

	// CNI teardown if the exit watcher lost the race (or never ran).
	if ni, ok := loadNetInfo(ns, id); ok && !usesHostNetworkName(ni.Network) {
		var ports []cniPortMapping
		if meta != nil {
			ports = meta.Ports
		}
		dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
		detachNetwork(dctx, ni.Network, ns, id, netnsPathFor(id), ports) //nolint:errcheck
		dcancel()
		removeNetInfo(ns, id)
	}

	releaseNamedNetNS(id)
	deleteContainerMeta(ns, id)
	if meta != nil {
		for _, v := range meta.AnonymousVolumes {
			os.RemoveAll(volumeDataDir(ns, v))
		}
	}
	return nil
}

// --- exec primitive -----------------------------------------------------------

// simpleExecResult carries a one-shot exec outcome.
type simpleExecResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// runSimpleExec runs argv inside a running container and captures output.
// It is the shared foundation for healthchecks and cp/archive operations.
func runSimpleExec(ctx context.Context, ns, id string, argv []string, user, cwd string, timeout time.Duration) (*simpleExecResult, error) {
	return runSimpleExecStdin(ctx, ns, id, argv, user, cwd, nil, timeout)
}

// runSimpleExecStdin additionally feeds the given bytes to the exec's stdin
// (nil = closed stdin). Mirrors the lifecycle of handleExecStart (exec.go),
// the battle-tested path: create with attached streams, Start, Wait, drain
// the cio copiers via IO().Wait, then close the parent write ends so the
// pipe readers see EOF.
func runSimpleExecStdin(ctx context.Context, ns, id string, argv []string, user, cwd string, stdin []byte, timeout time.Duration) (*simpleExecResult, error) {
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

	u := execUserFor(nsCtx, c, user)
	pspec := &specs.Process{
		Args: argv,
		Env:  []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		Cwd:  cwd,
		User: u,
	}
	if pspec.Cwd == "" {
		pspec.Cwd = "/"
	}

	execID := newContainerID()[:32]
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutW.Close()
		stdoutR.Close()
		return nil, err
	}

	var stdinR io.Reader
	if len(stdin) > 0 {
		stdinR = bytes.NewReader(stdin)
	}
	process, err := task.Exec(nsCtx, execID, pspec, cio.NewCreator(cio.WithStreams(stdinR, stdoutW, stderrW)))
	if err != nil {
		stdoutW.Close()
		stderrW.Close()
		stdoutR.Close()
		stderrR.Close()
		return nil, err
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	var outBuf, errBuf strings.Builder
	readAll := func(r *os.File, sb *strings.Builder, done chan struct{}) {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		close(done)
	}
	done1 := make(chan struct{})
	done2 := make(chan struct{})
	go readAll(stdoutR, &outBuf, done1)
	go readAll(stderrR, &errBuf, done2)

	if serr := process.Start(nsCtx); serr != nil {
		stdoutW.Close()
		stderrW.Close()
		<-done1
		<-done2
		process.Delete(context.Background()) //nolint:errcheck
		stdoutR.Close()
		stderrR.Close()
		return nil, fmt.Errorf("exec start: %w", serr)
	}

	exitCh, werr := process.Wait(nsCtx)
	if werr != nil {
		stdoutW.Close()
		stderrW.Close()
		<-done1
		<-done2
		process.Delete(context.Background()) //nolint:errcheck
		stdoutR.Close()
		stderrR.Close()
		return nil, werr
	}

	var st client.ExitStatus
	select {
	case s, ok := <-exitCh:
		if !ok {
			return nil, fmt.Errorf("exec wait cancelled")
		}
		st = s
	case <-time.After(timeout):
		kctx, kcancel := context.WithTimeout(nsCtx, 3*time.Second)
		process.Kill(kctx, syscall.SIGKILL) //nolint:errcheck
		kcancel()
		stdoutW.Close()
		stderrW.Close()
		<-done1
		<-done2
		process.Delete(context.Background()) //nolint:errcheck
		stdoutR.Close()
		stderrR.Close()
		return &simpleExecResult{stdout: outBuf.String(), stderr: errBuf.String(), exitCode: 124},
			fmt.Errorf("exec timed out after %s", timeout)
	}

	// Drain the client-side fifo copiers before closing our write ends,
	// exactly like handleExecStart.
	process.IO().Wait()
	stdoutW.Close()
	stderrW.Close()
	<-done1
	<-done2

	exitCode := 0
	if serr := st.Error(); serr != nil {
		debugLog("[exec] wait error: %v", serr)
	} else {
		exitCode = int(st.ExitCode())
	}
	process.Delete(context.Background()) //nolint:errcheck
	stdoutR.Close()
	stderrR.Close()
	return &simpleExecResult{stdout: outBuf.String(), stderr: errBuf.String(), exitCode: exitCode}, nil
}

// execUserFor maps an optional "uid[:gid]" user spec onto the OCI User of the
// container's own spec. Non-numeric names fall back to the container user.
func execUserFor(ctx context.Context, c client.Container, userstr string) specs.User {
	out := specs.User{}
	if spec, err := c.Spec(ctx); err == nil && spec.Process != nil {
		out = spec.Process.User
	}
	if userstr == "" {
		return out
	}
	parts := strings.SplitN(userstr, ":", 2)
	if uid, err := parseUint32(parts[0]); err == nil {
		out.UID = uid
	}
	if len(parts) == 2 {
		if gid, err := parseUint32(parts[1]); err == nil {
			out.GID = gid
		}
	}
	return out
}

func parseUint32(s string) (uint32, error) {
	var v uint64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not numeric")
		}
		v = v*10 + uint64(r-'0')
		if v > 0xFFFFFFFF {
			return 0, fmt.Errorf("overflow")
		}
	}
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return uint32(v), nil
}
