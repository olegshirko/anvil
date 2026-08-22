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

	var wg sync.WaitGroup
	for _, s := range []struct {
		name string
		fd   uintptr
	}{
		{"stdout", 3},
		{"stderr", 4},
	}	{
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

// writeStream splits the raw stream into newline-terminated records. A
// trailing partial line is flushed once the stream closes so short-lived
// containers using print (no newline) are still captured.
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
			}
		}
		if err != nil {
			break
		}
	}
	if buf.Len() > 0 {
		flush(buf.Bytes())
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
	since      time.Time // zero = no lower bound
	until      time.Time // zero = no upper bound
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
	f, err := os.Open(logPath)
	if err != nil {
		return nil // nothing to follow
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	br := bufio.NewReader(f)
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			var rec logLine
			if json.Unmarshal(raw, &rec) == nil {
				emitRecord(rec)
			}
		}
		if err != nil {
			time.Sleep(100 * time.Millisecond)
		}
	}
}
