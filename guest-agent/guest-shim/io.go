//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/containerd/console"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

// Parent-side stdio plumbing. The containerd shim contract gives the task
// three paths per stream:
//
//   - "binary"/"binary-v2" URIs: spawn the URI binary (the agent's json-file
//     logger) with the container streams on fd3/fd4 and a ready pipe on fd5,
//     and copy nothing ourselves — the init process writes the pipe ends
//     directly (binary-v2 requires the logger to write one ready byte).
//   - plain paths (or "fifo"): open the FIFO ends and hand them to the init
//     directly (fifo-mode logging: the agent runs the logger itself).
//   - terminal: allocate a pty; the shim keeps the master, copies
//     master <-> stdout sink and stdin fifo -> master, and passes the slave
//     as the init's stdio.
//
// Per the anvil logging contract, binary-v2 logger pipes are NEVER closed at
// task exit (that logger never sees EOF; the attach layer decides the end of
// stream). FIFO write ends and the console master are closed with the task.

type stdioSet struct {
	childStdin  *os.File // read end for the init's fd0 (nil = /dev/null)
	childStdout *os.File // write end for the init's fd1
	childStderr *os.File // write end for the init's fd2
	console     console.Console
	stdinSrc    *os.File // console stdin source (the stdin fifo; tty only)
	logger      *exec.Cmd
	sinkEOF     bool // stdout sink should close when the console copy ends
	closeFns    []func()
}

// urlFromIO parses one request io path into scheme+rest. A plain path is a
// fifo (containerd's convention).
func urlFromIO(path string) (*url.URL, error) {
	if path == "" {
		return nil, nil
	}
	u, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("parse io uri %q: %w", path, err)
	}
	if u.Scheme == "" {
		u.Scheme = "fifo"
	}
	return u, nil
}

// newStdioSet prepares the parent-side io for one process (init or exec).
func newStdioSet(ctx context.Context, id string, stdin, stdout, stderr string, terminal bool) (*stdioSet, error) {
	s := &stdioSet{}
	ns, _ := namespaces.NamespaceRequired(ctx)

	outU, err := urlFromIO(stdout)
	if err != nil {
		return nil, err
	}
	errU, err := urlFromIO(stderr)
	if err != nil {
		return nil, err
	}

	// stdout/stderr sinks. With a terminal they both receive the console
	// copy from the pty master.
	var outSink, errSink *os.File
	if terminal {
		master, slavePath, err := console.NewPty()
		if err != nil {
			return nil, fmt.Errorf("openpty: %w", err)
		}
		slave, err := os.OpenFile(slavePath, os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if err != nil {
			master.Close()
			return nil, fmt.Errorf("open console slave: %w", err)
		}
		s.console = master
		s.childStdin, s.childStdout, s.childStderr = slave, slave, slave
		s.closeFns = append(s.closeFns, func() { master.Close() })
		if stdin != "" {
			// The console's input is the stdin fifo (docker run -it, attach):
			// the shim copies fifo -> master; the slave is the tty itself.
			src, err := openFifoRead(stdin)
			if err != nil {
				master.Close()
				slave.Close()
				return nil, err
			}
			s.stdinSrc = src
			s.closeFns = append(s.closeFns, func() { src.Close() }) //nolint:errcheck
		}
	} else {
		switch schemeOf(outU) {
		case "binary", "binary-v2":
			outW, errW, logger, lerr := spawnLogger(ctx, id, ns, outU)
			if lerr != nil {
				return nil, lerr
			}
			outSink, errSink, s.logger = outW, errW, logger
		case "fifo":
			if outSink, err = openFifoWrite(outU.Path); err != nil {
				return nil, err
			}
			s.sinkEOF = true
		case "file":
			outSink, err = os.OpenFile(outU.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return nil, fmt.Errorf("open log file: %w", err)
			}
			s.sinkEOF = true
		default:
			return nil, fmt.Errorf("unknown stdout scheme %q", schemeOf(outU))
		}
		// stderr is only ever the same sink kind as stdout in our flows,
		// but handle every combination like the runc shim does.
		switch schemeOf(errU) {
		case "binary", "binary-v2":
			if errSink == nil && sameURL(outU, errU) && s.logger != nil {
				// Same URI: reuse the one logger (its fd4 carries stderr).
				errSink = outSink
			} else {
				_, errSink, _, err = spawnLogger(ctx, id, ns, errU)
				if err != nil {
					return nil, err
				}
			}
		case "fifo":
			if errSink, err = openFifoWrite(errU.Path); err != nil {
				return nil, err
			}
		case "file":
			if errSink, err = os.OpenFile(errU.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unknown stderr scheme %q", schemeOf(errU))
		}
		s.childStdout, s.childStderr = outSink, errSink
		if outSink != nil {
			s.closeFns = append(s.closeFns, func() { outSink.Close() }) //nolint:errcheck
		}
		if errSink != nil && errSink != outSink {
			s.closeFns = append(s.closeFns, func() { errSink.Close() }) //nolint:errcheck
		}
	}

	// stdin: a fifo path (opened read end) or nothing (/dev/null).
	if stdin != "" && !terminal {
		if s.childStdin, err = openFifoRead(stdin); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func schemeOf(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme
}

func sameURL(a, b *url.URL) bool {
	return a != nil && b != nil && a.String() == b.String()
}

// argsFromQuery turns the uri query into argv pairs (binary-v2 protocol:
// every key is a flag, the first value its argument).
func argsFromQuery(u *url.URL) []string {
	var args []string
	for k, vs := range u.Query() {
		args = append(args, k)
		if len(vs) > 0 {
			args = append(args, vs[0])
		}
	}
	return args
}

// spawnLogger starts the binary-v2 logging binary: container streams land
// on its fd3/fd4 and it signals readiness with one byte on fd5 — exactly
// the contract runJSONLogger in the agent implements.
func spawnLogger(ctx context.Context, id, ns string, u *url.URL) (*os.File, *os.File, *exec.Cmd, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outW.Close()
		outR.Close()
		return nil, nil, nil, err
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		outW.Close()
		outR.Close()
		errW.Close()
		errR.Close()
		return nil, nil, nil, err
	}

	cmd := exec.Command(u.Path, argsFromQuery(u)...)
	cmd.Env = append(cmd.Env,
		"CONTAINER_ID="+id,
		"CONTAINER_NAMESPACE="+ns,
	)
	cmd.ExtraFiles = append(cmd.ExtraFiles, outR, errR, readyW)
	if err := cmd.Start(); err != nil {
		outW.Close()
		outR.Close()
		errW.Close()
		errR.Close()
		readyW.Close()
		readyR.Close()
		return nil, nil, nil, fmt.Errorf("start logging binary %s: %w", u.Path, err)
	}
	// Parent keeps only the write ends and the ready read end.
	outR.Close()
	errR.Close()
	readyW.Close()

	// Wait for the logger's ready byte (binary-v2: EOF without a byte is a
	// crashed logger).
	byteBuf := make([]byte, 1)
	n, rerr := readyR.Read(byteBuf)
	readyR.Close()
	if rerr != nil && rerr != io.EOF {
		cmd.Process.Kill() //nolint:errcheck
		outW.Close()
		errW.Close()
		return nil, nil, nil, fmt.Errorf("logging binary not ready: %w", rerr)
	}
	if n == 0 && u.Scheme == "binary-v2" {
		cmd.Process.Kill() //nolint:errcheck
		outW.Close()
		errW.Close()
		return nil, nil, nil, fmt.Errorf("logging binary did not call ready (it may have crashed or exited prematurely)")
	}
	go cmd.Wait() //nolint:errcheck
	return outW, errW, cmd, nil
}

// openFifoWrite opens a fifo write end for the init's stdout/stderr. The
// reader side may not exist yet (attach started before the task): retry
// through ENXIO like containerd/fifo's deferred open does.
func openFifoWrite(path string) (*os.File, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
		if err == nil {
			return os.NewFile(uintptr(fd), path), nil
		}
		if err != unix.ENXIO {
			return nil, fmt.Errorf("open fifo %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("open fifo %s: no reader appeared", path)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// openFifoRead opens a fifo read end (never blocks: O_NONBLOCK).
func openFifoRead(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open fifo %s: %w", path, err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

// dupTo returns the fd duplicated at target (clearing close-on-exec so it
// survives the child's execve).
func dupTo(f *os.File, target int) error {
	if f == nil {
		// Nothing wired for this stream: point it at /dev/null.
		null, err := unix.Open("/dev/null", unix.O_RDWR, 0)
		if err != nil {
			return err
		}
		if null != target {
			if err := unix.Dup3(null, target, 0); err != nil {
				return err
			}
			unix.Close(null) //nolint:errcheck
		}
		return nil
	}
	return unix.Dup3(int(f.Fd()), target, 0)
}

// startCopies runs the shim-side copying for a console: master -> stdout
// sink and, when stdin is wired, stdin -> master. Master EOF (all slaves
// closed, i.e. the process is gone) ends the out copy; stdin EOF ends only
// the in copy (runc semantics).
func (s *stdioSet) startCopies(ctx context.Context) {
	if s.console == nil {
		return
	}
	master := s.console
	if s.childStdout != nil {
		go func() {
			defer func() {
				if s.sinkEOF {
					s.childStdout.Close() //nolint:errcheck
				}
			}()
			if _, err := io.Copy(s.childStdout, master); err != nil && !isBenignCopyErr(err) {
				log.G(ctx).WithError(err).Debug("console copy ended")
			}
		}()
	}
	if s.stdinSrc != nil {
		go func() {
			_, err := io.Copy(master, s.stdinSrc)
			if err != nil && !isBenignCopyErr(err) {
				log.G(ctx).WithError(err).Debug("console stdin copy ended")
			}
			// The fifo read end closing does not EOF the container's own
			// slave copy; the copy is simply done.
			s.stdinSrc.Close() //nolint:errcheck
		}()
	}
}

func isBenignCopyErr(err error) bool {
	if err == io.EOF {
		return true
	}
	return strings.Contains(err.Error(), "file already closed") ||
		strings.Contains(err.Error(), "input/output error")
}

// closeIOStdin implements CloseIO: the shim drops its stdin ends (the
// container's read sees EOF once the fifo writers close).
func (s *stdioSet) closeIOStdin() {
	if s.childStdin != nil {
		s.childStdin.Close() //nolint:errcheck
		s.childStdin = nil
	}
	if s.stdinSrc != nil {
		s.stdinSrc.Close() //nolint:errcheck
		s.stdinSrc = nil
	}
}

// close tears the io down (task delete): fifo/file sinks close so their
// readers (the loggers) end naturally; the console master closes. Binary
// logger pipes close here too — the logger sees EOF and exits, which is
// what keeps `docker rm` from leaving logger processes behind. The logger
// process itself is NOT killed: it exits on the EOF.
func (s *stdioSet) close() {
	for _, fn := range s.closeFns {
		fn()
	}
	if s.childStdin != nil {
		s.childStdin.Close() //nolint:errcheck
		s.childStdin = nil
	}
	if s.stdinSrc != nil {
		s.stdinSrc.Close() //nolint:errcheck
		s.stdinSrc = nil
	}
}

// resizePty applies a winsize to the console master.
func (s *stdioSet) resizePty(w, h uint32) error {
	if s.console == nil {
		return fmt.Errorf("no console")
	}
	return resizeConsole(s.console, w, h)
}

// resizeConsole resizes a console master (init and exec processes).
func resizeConsole(master console.Console, w, h uint32) error {
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(w), Col: uint16(h),
	})
}

// dupChildFds places the stdio fds at the fixed fd numbers the init helper
// expects (extra files 3..7 of the helper's exec.Cmd; the stdio trio is the
// last three).
func (s *stdioSet) dupChildFds() error {
	if err := dupTo(s.childStdin, fdStdin); err != nil {
		return fmt.Errorf("dup stdin: %w", err)
	}
	if err := dupTo(s.childStdout, fdStdout); err != nil {
		return fmt.Errorf("dup stdout: %w", err)
	}
	if err := dupTo(s.childStderr, fdStderr); err != nil {
		return fmt.Errorf("dup stderr: %w", err)
	}
	return nil
}
