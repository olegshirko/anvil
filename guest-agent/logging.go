package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sync"
	"time"
)

// Docker-compatible json-file logging for container tasks.
//
// containerd's built-in "file://" log URI appends raw bytes from BOTH streams
// into one file, which loses the stdout/stderr distinction Docker clients
// rely on. Like nerdctl (which re-executes itself as the logging binary),
// we re-execute the guest-agent binary under the shim's binary-v2 protocol:
//
//	fd3 -> container stdout, fd4 -> container stderr, fd5 -> ready pipe
//
// The logger writes one JSON object per line:
//
//	{"log":"line\n","stream":"stdout","time":"2006-01-02T15:04:05.000000000Z"}
//
// which is exactly Docker's json-file format, so logs readers can treat both
// identically. The task log lives at containerLogPath(ns, id).

const logReadyFD = 5

// logLine mirrors Docker's json-file record shape.
type logLine struct {
	Log    string    `json:"log"`
	Stream string    `json:"stream"`
	Time   time.Time `json:"time"`
}

// runJSONLogger implements the `guest-agent --log-json <path>` logging
// subcommand. It never returns until both input streams are closed.
func runJSONLogger(path string) error {
	out, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	// Signal readiness to the shim before consuming any bytes (binary-v2
	// treats EOF-without-byte as a crashed logger).
	if ready := os.NewFile(logReadyFD, "CONTAINER_WAIT"); ready != nil {
		_, _ = ready.Write([]byte{0})
		ready.Close()
	}

	// The shim never closes the stream fds, so the readers never see EOF
	// while the task record exists — a trailing partial line (printf
	// without newline, `... | head -c N`) would be lost forever. Flush
	// partials that have sat for a couple of seconds; full lines stream
	// immediately as before.
	startPartialFlusher(out)

	var wg sync.WaitGroup
	for _, s := range []struct {
		name string
		fd   uintptr
	}{
		{"stdout", 3},
		{"stderr", 4},
	} {
		wg.Add(1)
		go func(name string, fd uintptr) {
			defer wg.Done()
			f := os.NewFile(fd, "CONTAINER_"+name)
			if f == nil {
				return
			}
			writeStream(out, name, f)
		}(s.name, s.fd)
	}
	wg.Wait()
	return nil
}

// partialBuf holds a stream's unterminated tail so the owner watcher can
// flush it after container exit.
type partialBuf struct {
	mu    sync.Mutex
	b     bytes.Buffer
	since time.Time
}

var (
	partialMu      sync.Mutex
	partialBufs    = map[string]*partialBuf{}
	partialFlushed bool
)

func regPartial(stream string, data []byte) {
	partialMu.Lock()
	defer partialMu.Unlock()
	if partialFlushed {
		return
	}
	pb := partialBufs[stream]
	if pb == nil {
		pb = &partialBuf{}
		partialBufs[stream] = pb
	}
	if pb.b.Len() == 0 {
		pb.since = time.Now()
	}
	pb.b.Write(data)
}

// startPartialFlusher periodically emits registered partial lines that
// have not grown for two seconds.
func startPartialFlusher(out io.Writer) {
	go func() {
		for {
			time.Sleep(500 * time.Millisecond)
			partialMu.Lock()
			for stream, pb := range partialBufs {
				pb.mu.Lock()
				if pb.b.Len() > 0 && time.Since(pb.since) > 2*time.Second {
					rec, _ := json.Marshal(logLine{
						Log:    pb.b.String(),
						Stream: stream,
						Time:   time.Now().UTC(),
					})
					out.Write(append(rec, '\n'))
					pb.b.Reset()
				}
				pb.mu.Unlock()
			}
			partialMu.Unlock()
		}
	}()
}

// writeStream splits the raw stream into newline-terminated records. A
// trailing partial line is registered with the partial-flush registry (the
// owner watcher emits it after container exit) and flushed directly if the
// stream reaches real EOF.
func writeStream(out io.Writer, stream string, r io.Reader) {
	br := bufio.NewReaderSize(r, 64*1024)
	var buf bytes.Buffer
	flush := func(line []byte) {
		rec, err := json.Marshal(logLine{
			Log:    string(line),
			Stream: stream,
			Time:   time.Now().UTC(),
		})
		if err != nil {
			return
		}
		out.Write(append(rec, '\n'))
	}
	for {
		chunk, err := br.ReadBytes('\n')
		if len(chunk) > 0 {
			if chunk[len(chunk)-1] == '\n' {
				if buf.Len() > 0 {
					buf.Write(chunk)
					flush(buf.Bytes())
					buf.Reset()
				} else {
					flush(chunk)
				}
			} else {
				buf.Write(chunk)
				regPartial(stream, chunk)
			}
		}
		if err != nil {
			if buf.Len() > 0 {
				flush(buf.Bytes())
			}
			return
		}
	}
}

// taskLogURI builds the binary-v2 log URI pointing at this binary with the
// hidden logging subcommand. Query args become argv for the spawned logger.
func taskLogURI(logPath string) (*url.URL, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve agent path: %w", err)
	}
	u := &url.URL{Scheme: "binary-v2", Path: self}
	q := u.Query()
	q.Set("--log-json", logPath)
	u.RawQuery = q.Encode()
	return u, nil
}

// logReadOptions controls json-file log replay.
type logReadOptions struct {
	follow     bool
	tail       int // -1 = all records
	timestamps bool
	since      time.Time   // zero = no lower bound
	until      time.Time   // zero = no upper bound
	stop       func() bool // polled during follow; true ends the stream
}

// readTaskLog replays the container's json-file log. Each decoded record is
// passed to emit as (stream, payload); payloads are formatted per Docker
// expectations (raw line contents including the trailing newline).
func readTaskLog(logPath string, opts logReadOptions, emit func(stream byte, line []byte)) error {
	if opts.tail == 0 && !opts.follow {
		return nil // docker tail=0 semantics: nothing to replay
	}
	emitRecord := func(rec logLine) {
		if !opts.since.IsZero() && rec.Time.Before(opts.since) {
			return
		}
		if !opts.until.IsZero() && rec.Time.After(opts.until) {
			return
		}
		stream := byte(1)
		if rec.Stream == "stderr" {
			stream = 2
		}
		line := []byte(rec.Log)
		if timestamps := opts.timestamps; timestamps {
			line = append([]byte(rec.Time.Format(time.RFC3339Nano)+" "), line...)
		}
		emit(stream, line)
	}

	// Replay what exists, keeping only the last tail records when set.
	data, err := os.ReadFile(logPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		data = nil
	}
	var lines [][]byte
	if len(data) > 0 {
		lines = bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
		if opts.tail > 0 && len(lines) > opts.tail {
			lines = lines[len(lines)-opts.tail:]
		}
	}
	for _, raw := range lines {
		var rec logLine
		if json.Unmarshal(raw, &rec) != nil {
			continue
		}
		emitRecord(rec)
	}

	if !opts.follow {
		return nil
	}

	// The log file appears only when the task starts (the shim spawns the
	// logging binary then). docker run attaches BEFORE start, so poll until
	// the file shows up or the stop condition fires (debounced: a container
	// that runs and exits between polls must not cut off its own output).
	stopCount := 0
	stopFired := func() bool {
		if opts.stop == nil {
			return false
		}
		if opts.stop() {
			stopCount++
			if stopCount >= 3 {
				return true
			}
		} else {
			stopCount = 0
		}
		return false
	}

	// TTY tasks may flush their console output (and the logging binary may
	// replace the file wholesale) noticeably after the task exit becomes
	// observable, so follow re-reads by PATH instead of holding one fd, and
	// an empty/quiet log at stop time gets a grace period before giving up.
	var pending []byte // partial line carried between polls
	off := int64(len(data))
	var quietSince time.Time
	var stopDeadline time.Time
	deadline := time.Now().Add(30 * time.Second)
	for {
		if chunk, next, cerr := readLogFrom(logPath, off); cerr == nil && len(chunk) > 0 {
			pending = append(pending, chunk...)
			for {
				idx := bytes.IndexByte(pending, '\n')
				if idx < 0 {
					break
				}
				var rec logLine
				if json.Unmarshal(pending[:idx], &rec) == nil {
					emitRecord(rec)
				}
				pending = pending[idx+1:]
			}
			off = next
			quietSince = time.Time{} // fresh bytes: restart the quiet clock
		}
		if stopFired() {
			if stopDeadline.IsZero() {
				stopDeadline = time.Now().Add(5 * time.Second)
			}
			if quietSince.IsZero() {
				quietSince = time.Now()
			}
			// The logging binary's final flush can land after the task
			// exit is observable; end only once no new bytes arrived for
			// a second (or the hard stop deadline passes).
			if time.Since(quietSince) >= 2*time.Second || time.Now().After(stopDeadline) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return nil // safety valve for a wedged stop condition
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// readLogFrom returns the bytes appended to the log file since offset off
// (reopening by path so a wholesale file replacement is picked up) plus the
// new offset.
func readLogFrom(path string, off int64) ([]byte, int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, off, err
	}
	if fi.Size() == off {
		return nil, off, nil
	}
	if fi.Size() < off {
		off = 0 // the file was replaced wholesale; read it from the start
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, off, err
	}
	defer f.Close()
	buf := make([]byte, fi.Size()-off)
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, off, err
	}
	return buf[:n], off + int64(n), nil
}
