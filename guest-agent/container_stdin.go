package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"golang.org/x/sys/unix"
)

// Container stdin (`docker run -i`, `docker attach`, `docker start -ai`).
//
// A container created with OpenStdin gets a FIFO that its task's IO config
// names as stdin; the shim copies it into the process (or its pty). The
// agent keeps the FIFO open read-write for the container's lifetime, so the
// process does not see EOF between attaches and writes before the task
// starts (docker run attaches first) wait in the pipe buffer. Attach copies
// the client's stdin into it. With StdinOnce the holder is closed when the
// first attach's stdin ends: the shim then reads EOF and closes the
// process stdin, as Docker does for `echo x | docker run -i image cat`.

const stdinFifoDir = "/run/anvil/stdin"

type containerStdin struct {
	path   string // current FIFO: stdinFifoPath + "." + gen
	gen    int
	ns, id string

	mu sync.Mutex
	f  *os.File // read-write holder; nil once closed
	// taskOpen is set once a task exists on this FIFO, i.e. a shim holds
	// its read end. Before that the holder must not close: closing a FIFO's
	// last descriptor discards its buffered data, and the shim would then
	// wait for a writer forever. closePending defers such a close. A shim
	// keeps reading after its process exits (until the task is deleted at
	// the next start), so a FIFO with taskOpen set is never reused for a
	// new run: it would swallow input meant for it.
	taskOpen     bool
	closePending bool
}

var containerStdins = struct {
	sync.Mutex
	m map[string]*containerStdin
}{m: map[string]*containerStdin{}}

func stdinFifoPath(ns, id string) string {
	return filepath.Join(stdinFifoDir, ns, id)
}

// openContainerStdin returns the stdin for the container's next or current
// run. forNextRun (attach to a stopped container, task start) moves to a
// fresh FIFO when a shim already holds the current one; a closed holder
// (StdinOnce) is reopened.
func openContainerStdin(ns, id string, forNextRun bool) (*containerStdin, error) {
	key := ns + "/" + id
	containerStdins.Lock()
	cs := containerStdins.m[key]
	if cs == nil {
		cs = &containerStdin{ns: ns, id: id}
		containerStdins.m[key] = cs
	}
	containerStdins.Unlock()

	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.path != "" && cs.f != nil && !(forNextRun && cs.taskOpen) {
		return cs, nil
	}
	if cs.path == "" || cs.taskOpen {
		if cs.f != nil {
			cs.f.Close()
			cs.f = nil
		}
		if cs.path != "" {
			os.Remove(cs.path) //nolint:errcheck — the old shim keeps its open fd
		}
		cs.gen++
		cs.path = fmt.Sprintf("%s.%d", stdinFifoPath(ns, id), cs.gen)
		cs.taskOpen, cs.closePending = false, false
	}
	if err := os.MkdirAll(filepath.Dir(cs.path), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(cs.path); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		os.Remove(cs.path) //nolint:errcheck
		if err := unix.Mkfifo(cs.path, 0o600); err != nil {
			return nil, fmt.Errorf("stdin fifo: %w", err)
		}
	}
	// O_RDWR never blocks on a FIFO (Linux) and keeps a writer open.
	f, err := os.OpenFile(cs.path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("stdin fifo: %w", err)
	}
	cs.f = f
	return cs, nil
}

func (cs *containerStdin) Write(p []byte) (int, error) {
	cs.mu.Lock()
	f := cs.f
	cs.mu.Unlock()
	if f == nil {
		return 0, os.ErrClosed
	}
	return f.Write(p)
}

// close ends the container's stdin: the process reads EOF once the pipe
// buffer is drained. Before the task exists (`docker run -i` sends its
// input and EOF before start) the close waits for taskStarted.
func (cs *containerStdin) close() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if !cs.taskOpen {
		cs.closePending = true
		return
	}
	cs.closeLocked()
}

func (cs *containerStdin) closeLocked() {
	cs.closePending = false
	if cs.f != nil {
		cs.f.Close()
		cs.f = nil
	}
	if cs.taskOpen {
		// The shim keeps its own writer on the FIFO (for the CloseIO API),
		// so closing ours alone never reaches the process as EOF.
		go closeTaskStdin(cs.ns, cs.id)
	}
}

// closeTaskStdin asks the shim to close the task's stdin once the FIFO is
// drained.
func closeTaskStdin(ns, id string) {
	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), ns), 10*time.Second)
	defer cancel()
	cl, err := pc.get(ctx)
	if err != nil {
		return
	}
	c, err := cl.LoadContainer(ctx, id)
	if err != nil {
		return
	}
	task, err := c.Task(ctx, nil)
	if err != nil {
		return
	}
	if err := task.CloseIO(ctx, client.WithStdinCloser); err != nil {
		debugLog("[stdin] close %s: %v", truncateID(id), err)
	}
}

// taskStarting marks a new run whose shim has not opened the FIFO yet.
func (cs *containerStdin) taskStarting() {
	cs.mu.Lock()
	cs.taskOpen = false
	cs.mu.Unlock()
}

// taskCreated marks the shim as holding the FIFO and applies a deferred
// close.
func (cs *containerStdin) taskCreated() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.taskOpen = true
	if cs.closePending {
		cs.closeLocked()
	}
}

// forgetContainerStdin closes and removes a deleted container's stdin.
func forgetContainerStdin(ns, id string) {
	key := ns + "/" + id
	containerStdins.Lock()
	cs := containerStdins.m[key]
	delete(containerStdins.m, key)
	containerStdins.Unlock()
	if cs != nil {
		cs.mu.Lock()
		cs.taskOpen = false // the task is gone with the container
		cs.closeLocked()
		if cs.path != "" {
			os.Remove(cs.path) //nolint:errcheck
		}
		cs.mu.Unlock()
	}
	os.Remove(stdinFifoPath(ns, id))                        //nolint:errcheck
	os.RemoveAll(filepath.Join(stdinFifoDir, ns, id+".io")) //nolint:errcheck
}

// parseDetachKeys parses Docker's detach-key spec ("ctrl-p,ctrl-q", "a,b");
// an empty spec is the default ctrl-p,ctrl-q.
func parseDetachKeys(spec string) ([]byte, error) {
	if spec == "" {
		spec = "ctrl-p,ctrl-q"
	}
	var out []byte
	for _, k := range strings.Split(spec, ",") {
		k = strings.TrimSpace(k)
		switch {
		case len(k) == 1:
			out = append(out, k[0])
		case strings.HasPrefix(strings.ToLower(k), "ctrl-") && len(k) == 6:
			c := k[5] | 0x20 // lower case
			switch {
			case c >= 'a' && c <= 'z':
				out = append(out, c-'a'+1)
			case k[5] == '@':
				out = append(out, 0)
			case k[5] == '[':
				out = append(out, 27)
			case k[5] == '\\':
				out = append(out, 28)
			case k[5] == ']':
				out = append(out, 29)
			case k[5] == '^':
				out = append(out, 30)
			case k[5] == '_':
				out = append(out, 31)
			default:
				return nil, fmt.Errorf("invalid detach key %q", k)
			}
		default:
			return nil, fmt.Errorf("invalid detach key %q", k)
		}
	}
	return out, nil
}

// copyStdinUntilDetach copies src to dst until src ends or the detach
// sequence is typed; it reports whether the client detached. Bytes of a
// partially matched sequence are held back and sent if the match fails.
func copyStdinUntilDetach(dst interface{ Write([]byte) (int, error) }, src interface{ Read([]byte) (int, error) }, keys []byte) (detached bool, err error) {
	buf := make([]byte, 32<<10)
	matched := 0
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			out := make([]byte, 0, n+matched)
			for _, b := range buf[:n] {
				if len(keys) > 0 && b == keys[matched] {
					matched++
					if matched == len(keys) {
						if len(out) > 0 {
							if _, werr := dst.Write(out); werr != nil {
								return false, werr
							}
						}
						return true, nil
					}
					continue
				}
				if matched > 0 {
					out = append(out, keys[:matched]...)
					matched = 0
					if len(keys) > 0 && b == keys[0] {
						matched = 1
						continue
					}
				}
				out = append(out, b)
			}
			if len(out) > 0 {
				if _, werr := dst.Write(out); werr != nil {
					return false, werr
				}
			}
		}
		if rerr != nil {
			return false, nil // client stdin ended
		}
	}
}

// fifoIO is the task IO of a stdin-attached, non-TTY container: plain FIFO
// paths, which make the shim copy all three streams itself.
type fifoIO struct {
	stdin, stdout, stderr string
}

func (f *fifoIO) Config() cio.Config {
	return cio.Config{Stdin: f.stdin, Stdout: f.stdout, Stderr: f.stderr}
}

func (f *fifoIO) Cancel()      {}
func (f *fifoIO) Wait()        {}
func (f *fifoIO) Close() error { return nil }

// startFifoLogger creates the stream FIFOs for one run of a stdin-attached
// container and starts the json logger on their read ends (the same logger
// the shim spawns in binary mode, reading by path). The logger exits when
// the shim closes the FIFOs at the process exit; stop kills it when the
// task could not be created.
func startFifoLogger(ns, id, logPath string, rot logRotation, stdinPath string) (*fifoIO, func(), error) {
	dir := filepath.Join(stdinFifoDir, ns, id+".io")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	out, errp := filepath.Join(dir, "stdout"), filepath.Join(dir, "stderr")
	for _, p := range []string{out, errp} {
		os.Remove(p) //nolint:errcheck — a previous run's FIFO
		if err := unix.Mkfifo(p, 0o600); err != nil {
			return nil, nil, fmt.Errorf("log fifo: %w", err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(self, "--log-json", logPath,
		"--max-size", strconv.FormatInt(rot.maxSize, 10), "--max-file", strconv.Itoa(rot.maxFile),
		"--fifo-out", out, "--fifo-err", errp)
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start logger: %w", err)
	}
	go cmd.Wait()                         //nolint:errcheck — PID 1's reaper may collect it first
	stop := func() { cmd.Process.Kill() } //nolint:errcheck
	return &fifoIO{stdin: stdinPath, stdout: out, stderr: errp}, stop, nil
}
