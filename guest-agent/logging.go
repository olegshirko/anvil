package main

import (
	"bufio"
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

// runFifoJSONLogger is the logger for a task the shim runs in FIFO mode
// (stdin-attached, non-TTY containers): it reads the stream FIFOs by path and
// exits when the shim closes them at the process exit.
func runFifoJSONLogger(path string, rot logRotation, fifos loggerFifos) error {
	out, err := openRotatingLog(path, rot)
	if err != nil {
		return err
	}
	defer out.Close()
	startPartialFlusher(out)
	var wg sync.WaitGroup
	for name, p := range map[string]string{"stdout": fifos.stdout, "stderr": fifos.stderr} {
		wg.Add(1)
		go func(name, p string) {
			defer wg.Done()
			// Blocking open: returns once the shim opens the write end.
			f, err := os.OpenFile(p, os.O_RDONLY, 0)
			if err != nil {
				return
			}
			defer f.Close()
			writeStream(out, name, f)
		}(name, p)
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
const partialFlushAfter = 100 * time.Millisecond

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
	// stream selects which streams are emitted (1 stdout, 2 stderr); nil
	// emits both.
	stream func(stream byte) bool
	// noStopWait bounds the follow when the log file never appears and no
	// stop condition can fire (zero = 30s production default; tests shrink it).
	noStopWait time.Duration
	// stopQuiet is how long the log must stay quiet after the task exit
	// before the follow ends (zero = stopQuietDefault). TTY tasks get
	// longer: the shim copies the console after the process is gone.
	stopQuiet time.Duration
}

// stopQuietDefault bounds the wait for the logger's last writes after the
// task exit. The logger reads the stream fifos as bytes arrive and flushes
// an unterminated tail after partialFlushAfter, so the log is final well
// within this. `docker run` returns this long after the container exits,
// so it is kept tight: twice partialFlushAfter.
const (
	stopQuietDefault = 2 * partialFlushAfter
	stopQuietTTY     = 500 * time.Millisecond
)

// readTaskLog replays the container's json-file log. Each decoded record is
// passed to emit as (stream, payload); payloads are formatted per Docker
// expectations (raw line contents including the trailing newline).
func readTaskLog(logPath string, opts logReadOptions, emit func(stream byte, line []byte)) error {
	if opts.tail == 0 && !opts.follow {
		return nil // docker tail=0 semantics: nothing to replay
	}
	if opts.stream != nil {
		inner := emit
		emit = func(stream byte, line []byte) {
			if opts.stream(stream) {
				inner(stream, line)
			}
		}
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
	liveSize, err := replayLog(logPath, opts.tail, func(raw []byte) {
		var rec logLine
		if json.Unmarshal(raw, &rec) == nil {
			emitRecord(rec)
		}
	})
	if err != nil {
		return err
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
			if stopCount >= 2 {
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
			quiet := opts.stopQuiet
			if quiet <= 0 {
				quiet = stopQuietDefault
			}
			if time.Since(quietSince) >= quiet || time.Now().After(stopDeadline) {
				debugLog("follow %s: ended by stop condition (quiet %v, hard deadline %v)",
					logPath, time.Since(quietSince) >= quiet, time.Now().After(stopDeadline))
				return nil
			}
		}
		if !fileSeen && !noStopDeadline.IsZero() && time.Now().After(noStopDeadline) {
			return nil // the task never started and no stop condition exists
		}
		if stopCount > 0 {
			time.Sleep(50 * time.Millisecond) // exiting: finish promptly
		} else {
			time.Sleep(100 * time.Millisecond)
		}
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

// replayLog passes the complete records of the log (rotated files oldest
// first, then the live one) to emit — only the last tail of them when
// tail > 0 — without loading the files into memory: up to 300 MiB of log
// was read and split in memory on every `docker logs`, even with --tail 10.
// It returns the offset in the live file after the last complete record,
// where a follow continues (an unterminated record is still being written
// and is picked up by the follow, not lost).
func replayLog(logPath string, tail int, emit func(raw []byte)) (int64, error) {
	if tail == 0 {
		// Nothing to replay (`logs --tail 0 -f`, attach without logs):
		// a follow starts at the current end.
		return completeLogEnd(logPath)
	}
	files := rotatedLogFiles(logPath)
	if tail > 0 {
		return replayLogTail(files, logPath, tail, emit)
	}
	var liveOff int64
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return liveOff, err
		}
		r := bufio.NewReaderSize(f, 64<<10)
		var off int64
		for {
			line, rerr := r.ReadBytes('\n')
			if rerr != nil {
				break // EOF; an unterminated tail is not a complete record
			}
			off += int64(len(line))
			emit(line[:len(line)-1])
		}
		f.Close()
		if p == logPath {
			liveOff = off
		}
	}
	return liveOff, nil
}

// completeLogEnd returns the offset after the last complete record of the
// live log file (0 when it does not exist yet).
func completeLogEnd(logPath string) (int64, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	_, end, err := lastLogLines(f, fi.Size(), 1)
	return end, err
}

// replayLogTail emits the last n complete records across files, reading
// each file backwards from its end.
func replayLogTail(files []string, logPath string, n int, emit func(raw []byte)) (int64, error) {
	var liveOff int64
	var newestFirst [][]byte
	for i := len(files) - 1; i >= 0 && len(newestFirst) < n; i-- {
		f, err := os.Open(files[i])
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return 0, err
		}
		lines, completeEnd, err := lastLogLines(f, fi.Size(), n-len(newestFirst))
		f.Close()
		if err != nil {
			return 0, err
		}
		if files[i] == logPath {
			liveOff = completeEnd
		}
		newestFirst = append(newestFirst, lines...)
	}
	for i := len(newestFirst) - 1; i >= 0; i-- {
		emit(newestFirst[i])
	}
	return liveOff, nil
}

// lastLogLines returns up to want complete lines (without '\n') from the end
// of f, newest first, plus the offset after the file's last '\n'.
func lastLogLines(f io.ReaderAt, size int64, want int) ([][]byte, int64, error) {
	const block = 64 << 10
	var out [][]byte
	var carry []byte // the not yet split bytes in [pos, completeEnd)
	pos := size
	completeEnd := int64(-1)
	for pos > 0 && len(out) < want {
		n := int64(block)
		if pos < n {
			n = pos
		}
		pos -= n
		chunk := make([]byte, n, n+int64(len(carry)))
		if _, err := f.ReadAt(chunk, pos); err != nil && err != io.EOF {
			return nil, 0, err
		}
		carry = append(chunk, carry...)
		if completeEnd < 0 {
			idx := bytes.LastIndexByte(carry, '\n')
			if idx < 0 {
				continue
			}
			completeEnd = pos + int64(idx) + 1
			carry = carry[:idx+1] // drop the unterminated tail
		}
		// carry ends with '\n': peel whole lines off its end, keeping the
		// first (possibly partial) line for the next block.
		for len(out) < want {
			body := carry[:len(carry)-1]
			idx := bytes.LastIndexByte(body, '\n')
			if idx < 0 {
				break
			}
			out = append(out, append([]byte(nil), body[idx+1:]...))
			carry = carry[:idx+1]
		}
	}
	if pos == 0 && completeEnd >= 0 && len(out) < want && len(carry) > 0 {
		out = append(out, append([]byte(nil), carry[:len(carry)-1]...)) // the file's first line
	}
	if completeEnd < 0 {
		completeEnd = 0
	}
	return out, completeEnd, nil
}
