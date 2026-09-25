package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"sync"
	"syscall"
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
func runJSONLogger(path string, rot logRotation) error {
	out, err := openRotatingLog(path, rot)
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
	// without newline, `... | head -c N`) would be lost forever. Partials
	// are flushed once the stream has been quiet briefly; full lines
	// stream immediately.
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

// partialFlushAfter is how long an unterminated tail may sit before it is
// written as its own record. It must stay well under the follow grace in
// readTaskLog (2 s after the task exit), or `docker run` / attach of a
// short-lived container ends before its last partial line reaches the log
// — and with --rm the container is gone by then. Splitting a slowly
// written line into several records does not change the replayed bytes.
const partialFlushAfter = 250 * time.Millisecond

// maxPartialRecord bounds an unterminated tail held in memory.
const maxPartialRecord = 16 * 1024

// partialBuf is a stream's unterminated tail. It is the only copy: the
// stream reader and the flusher both take from it under mu, so a flushed
// fragment is never written again when its newline arrives.
type partialBuf struct {
	mu        sync.Mutex
	b         bytes.Buffer
	lastWrite time.Time
}

// take returns and clears the buffered tail.
func (pb *partialBuf) take() []byte {
	out := append([]byte(nil), pb.b.Bytes()...)
	pb.b.Reset()
	return out
}

var partials = struct {
	sync.Mutex
	m map[string]*partialBuf
}{m: map[string]*partialBuf{}}

func partialFor(stream string) *partialBuf {
	partials.Lock()
	defer partials.Unlock()
	pb := partials.m[stream]
	if pb == nil {
		pb = &partialBuf{}
		partials.m[stream] = pb
	}
	return pb
}

// startPartialFlusher periodically writes partial lines that stopped
// growing partialFlushAfter ago.
func startPartialFlusher(out io.Writer) {
	go func() {
		for {
			time.Sleep(partialFlushAfter / 2)
			flushQuietPartials(out, time.Now())
		}
	}()
}

func flushQuietPartials(out io.Writer, now time.Time) {
	partials.Lock()
	streams := make(map[string]*partialBuf, len(partials.m))
	for k, v := range partials.m {
		streams[k] = v
	}
	partials.Unlock()
	for stream, pb := range streams {
		pb.mu.Lock()
		var tail []byte
		if pb.b.Len() > 0 && now.Sub(pb.lastWrite) >= partialFlushAfter {
			tail = pb.take()
		}
		pb.mu.Unlock()
		if len(tail) > 0 {
			writeLogRecord(out, stream, tail)
		}
	}
}

func writeLogRecord(out io.Writer, stream string, data []byte) {
	rec, err := json.Marshal(logLine{
		Log:    string(data),
		Stream: stream,
		Time:   time.Now().UTC(),
	})
	if err != nil {
		return
	}
	out.Write(append(rec, '\n'))
}

// writeStream splits the raw stream into newline-terminated records. An
// unterminated tail waits in the stream's partialBuf until its newline
// arrives, the flusher writes it, or the stream reaches real EOF.
//
// It reads raw chunks: bufio's ReadBytes('\n') blocks until a newline or
// EOF, and the shim never closes the stream, so a tail without a newline
// was never even seen by the flusher.
func writeStream(out io.Writer, stream string, r io.Reader) {
	pb := partialFor(stream)
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		data := buf[:n]
		for len(data) > 0 {
			idx := bytes.IndexByte(data, '\n')
			pb.mu.Lock()
			if idx < 0 {
				pb.b.Write(data)
				pb.lastWrite = time.Now()
				// A newline-less flood: write full 16 KiB records (Docker
				// splits there too) and keep only the remainder buffered.
				var full [][]byte
				for pb.b.Len() >= maxPartialRecord {
					full = append(full, append([]byte(nil), pb.b.Next(maxPartialRecord)...))
				}
				pb.mu.Unlock()
				for _, rec := range full {
					writeLogRecord(out, stream, rec)
				}
				break
			}
			line := append(pb.take(), data[:idx+1]...)
			pb.mu.Unlock()
			writeLogRecord(out, stream, line)
			data = data[idx+1:]
		}
		if err != nil {
			pb.mu.Lock()
			tail := pb.take()
			pb.mu.Unlock()
			if len(tail) > 0 {
				writeLogRecord(out, stream, tail)
			}
			return
		}
	}
}

// taskLogURI builds the binary-v2 log URI pointing at this binary with the
// hidden logging subcommand. Query args become argv for the spawned logger.
func taskLogURI(logPath string, rot logRotation) (*url.URL, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve agent path: %w", err)
	}
	u := &url.URL{Scheme: "binary-v2", Path: self}
	q := u.Query()
	q.Set("--log-json", logPath)
	q.Set("--max-size", strconv.FormatInt(rot.maxSize, 10))
	q.Set("--max-file", strconv.Itoa(rot.maxFile))
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
	// noStopWait bounds the follow when the log file never appears and no
	// stop condition can fire (zero = 30s production default; tests shrink it).
	noStopWait time.Duration
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

	// Replay what exists — rotated files oldest first, then the live one —
	// keeping only the last tail records when set.
	var data []byte
	var liveSize int64
	for _, p := range rotatedLogFiles(logPath) {
		b, err := os.ReadFile(p)
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			continue
		}
		data = append(data, b...)
		if p == logPath {
			liveSize = int64(len(b))
		}
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
	off := liveSize
	ino := logInode(logPath)
	var quietSince time.Time
	var stopDeadline time.Time
	// A running container is followed indefinitely (docker logs -f must not
	// end on a timer); the only bounded wait is for a log file that never
	// appears while no stop condition can ever fire (callers without stop).
	var fileSeen bool
	var noStopDeadline time.Time
	if opts.stop == nil {
		wait := opts.noStopWait
		if wait <= 0 {
			wait = 30 * time.Second
		}
		noStopDeadline = time.Now().Add(wait)
	}
	for {
		// Rotation: the live file got a new inode. The old one is now
		// path.1; take what was appended to it since the last poll first.
		if cur := logInode(logPath); cur != 0 && ino != 0 && cur != ino {
			if logInode(rotatedLogName(logPath, 1)) == ino {
				if chunk, _, cerr := readLogFrom(rotatedLogName(logPath, 1), off); cerr == nil {
					pending = append(pending, chunk...)
				}
			}
			ino, off = cur, 0
		} else if ino == 0 {
			ino = cur
		}
		if chunk, next, cerr := readLogFrom(logPath, off); cerr == nil {
			fileSeen = true
			if len(chunk) > 0 {
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
				debugLog("follow %s: ended by stop condition (quiet %v, hard deadline %v)",
					logPath, time.Since(quietSince) >= 2*time.Second, time.Now().After(stopDeadline))
				return nil
			}
		}
		if !fileSeen && !noStopDeadline.IsZero() && time.Now().After(noStopDeadline) {
			return nil // the task never started and no stop condition exists
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

// logInode identifies the file at path (0 when absent), to notice rotation.
func logInode(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
