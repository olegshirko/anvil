package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The {"eof":true} marker record must never surface as log output: replay
// emits exactly the real records, whatever the filtering options are.
func TestReadTaskLogSkipsEOFMarkerOnReplay(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log.json")
	body := `{"time":"2026-01-01T00:00:00Z","stream":"stdout","log":"real line\n"}` + "\n" +
		`{"eof":true}` + "\n"
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		opts logReadOptions
		want []string
	}{
		{"all", logReadOptions{tail: -1}, []string{"real line\n"}},
		{"timestamps", logReadOptions{tail: -1, timestamps: true},
			[]string{"2026-01-01T00:00:00.000000000Z real line\n"}},
		{"since", logReadOptions{tail: -1, since: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
			[]string{"real line\n"}},
		{"tail-5", logReadOptions{tail: 5}, []string{"real line\n"}},
		// The marker must not consume the tail slot either, or --tail 1 of an
		// exited container would return nothing at all.
		{"tail-1", logReadOptions{tail: 1}, []string{"real line\n"}},
	} {
		var lines []string
		if err := readTaskLog(logPath, tc.opts, func(stream byte, line []byte) {
			if stream != 1 {
				t.Errorf("%s: stream = %d, want 1", tc.name, stream)
			}
			lines = append(lines, string(line))
		}); err != nil {
			t.Fatalf("%s: readTaskLog: %v", tc.name, err)
		}
		if len(lines) != len(tc.want) || (len(lines) > 0 && lines[0] != tc.want[0]) {
			t.Errorf("%s: emitted %q, want %q", tc.name, lines, tc.want)
		}
	}
}

// The marker is invisible to tail selection: with two records and a marker,
// every tail window holds only real records.
func TestReplayTailSkipsEOFMarker(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log.json")
	body := `{"time":"2026-01-01T00:00:00Z","stream":"stdout","log":"one\n"}` + "\n" +
		`{"time":"2026-01-01T00:00:01Z","stream":"stdout","log":"two\n"}` + "\n" +
		`{"eof":true}` + "\n"
	if err := os.WriteFile(logPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		tail int
		want []string
	}{
		{1, []string{"two\n"}},
		{2, []string{"one\n", "two\n"}},
		{5, []string{"one\n", "two\n"}},
	} {
		var lines []string
		if err := readTaskLog(logPath, logReadOptions{tail: tc.tail}, func(_ byte, line []byte) {
			lines = append(lines, string(line))
		}); err != nil {
			t.Fatalf("tail %d: readTaskLog: %v", tc.tail, err)
		}
		if len(lines) != len(tc.want) {
			t.Errorf("tail %d: emitted %q, want %q", tc.tail, lines, tc.want)
			continue
		}
		for i, w := range tc.want {
			if lines[i] != w {
				t.Errorf("tail %d: line %d = %q, want %q", tc.tail, i, lines[i], w)
			}
		}
	}
}

// isEOFMarker must not fire on a record whose log payload merely quotes the
// marker shape: the payload is JSON-escaped, the top-level field is not.
func TestEOFMarkerDetectionIgnoresQuotedPayloads(t *testing.T) {
	real := []byte(`{"time":"2026-01-01T00:00:00Z","stream":"stdout","log":"{\"eof\":true}\n"}`)
	if isEOFMarker(real) {
		t.Error("record with quoted marker payload detected as marker")
	}
	marker := []byte(`{"eof":true,"time":"2026-01-01T00:00:00Z"}`)
	if !isEOFMarker(marker) {
		t.Error("marker record not detected")
	}
}

// initGone is kill(pid, 0)-based: true only once the pid is gone AND reaped
// (a zombie still answers kill, matching the shim whose reaper owns the
// real init).
func TestInitGoneDetects(t *testing.T) {
	if initGone(os.Getpid()) {
		t.Error("initGone(self) = true, want false")
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	pid := cmd.Process.Pid
	if initGone(pid) {
		t.Error("initGone(running child) = true, want false")
	}
	_ = cmd.Wait() // reap: the shim's reaper does this for a real init
	if !initGone(pid) {
		t.Error("initGone(reaped child) = false, want true")
	}
}

// The eof-marker argv gate: non-TTY tasks carry it, TTY tasks do not
// (builder-check resolution: TTY containers keep the stopQuiet timers end
// to end).
func TestTaskLogURICarriesMarker(t *testing.T) {
	rot := logRotation{maxSize: 1 << 20, maxFile: 2}
	u, err := taskLogURI("/store/ns/abc.json", rot, true)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("--eof-marker") != "1" {
		t.Errorf("non-TTY URI: --eof-marker = %q, want 1", q.Get("--eof-marker"))
	}
	if q.Get("--log-json") != "/store/ns/abc.json" {
		t.Errorf("--log-json = %q", q.Get("--log-json"))
	}
	u, err = taskLogURI("/store/ns/abc.json", rot, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := u.Query()["--eof-marker"]; ok {
		t.Error("TTY URI carries --eof-marker, want none")
	}
}

// The shim builds the logger's argv from the URI query pairs in any order;
// the marker flag must survive that trip (and argv without it must leave
// the marker off).
func TestLoggerArgsMarker(t *testing.T) {
	path, rot, fifos, marker, ok := loggerArgsFifos([]string{
		"--eof-marker", "1", "--log-json", "/l/x.json", "--max-size", "1024", "--max-file", "2",
	})
	if !ok || path != "/l/x.json" || rot.maxSize != 1024 || rot.maxFile != 2 || fifos.stdout != "" {
		t.Fatalf("loggerArgsFifos = path=%q rot=%+v fifos=%+v ok=%v", path, rot, fifos, ok)
	}
	if !marker {
		t.Error("marker = false, want true")
	}
	if _, _, _, m, ok := loggerArgsFifos([]string{"--log-json", "/l/x.json"}); !ok || m {
		t.Errorf("marker = %v without the arg (ok=%v), want false/true", m, ok)
	}
}

// readLogLines parses a json-file log into its records.
func readLogLines(t *testing.T, logPath string) []logLine {
	t.Helper()
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var recs []logLine
	s := bufio.NewScanner(f)
	for s.Scan() {
		var rec logLine
		if err := json.Unmarshal(s.Bytes(), &rec); err != nil {
			t.Fatalf("log line %q: %v", s.Text(), err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// countEOFMarkers counts marker records in a parsed log.
func countEOFMarkers(recs []logLine) int {
	n := 0
	for _, r := range recs {
		if r.EOF {
			n++
		}
	}
	return n
}

// The shim-spawned logger ends the log with the marker once the container
// init is gone, with pending partial output flushed before it: the follow
// ends only after everything the container wrote is on disk, in order.
func TestRunJSONLoggerWritesMarkerOnPidDeath(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log.json")
	out, err := openRotatingLog(logPath, logRotation{})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutW.Close()
	defer stderrW.Close()

	runErr := make(chan error, 1)
	go func() { runErr <- runStreamLogger(out, logPath, true, stdoutR, stderrR) }()

	// An unterminated tail: the plain flusher would hold it
	// partialFlushAfter before writing; the marker must never overtake it.
	if _, err := stdoutW.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}

	// The init runs, exits and is reaped (the shim's reaper role), and the
	// agent posts its pid file.
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	writeInitPidFile(logPath, pid)

	deadline := time.Now().Add(5 * time.Second)
	for {
		recs := readLogLines(t, logPath)
		if len(recs) > 0 && recs[len(recs)-1].EOF {
			if len(recs) != 2 {
				t.Fatalf("records = %+v, want [partial, marker]", recs)
			}
			if recs[0].Log != "partial" {
				t.Errorf("first record = %+v, want the partial line", recs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no marker within 5s; log = %+v", recs)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The marker lands while the streams are still open (the shim holds
	// them until task delete) and only once.
	if _, err := stdoutW.Write([]byte("more\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := countEOFMarkers(readLogLines(t, logPath)); n != 1 {
		t.Errorf("%d marker records after late output, want 1", n)
	}

	// Streams close (task delete): the logger returns without writing
	// another marker.
	stdoutW.Close()
	stderrW.Close()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("runStreamLogger: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logger did not finish after stream EOF")
	}
	if n := countEOFMarkers(readLogLines(t, logPath)); n != 1 {
		t.Errorf("%d marker records after close, want 1", n)
	}
}

// The FIFO logger (stdin-attached non-TTY containers, `docker run -i`)
// writes the marker on its natural stream EOF: the shim closes the fifos at
// the process exit, so the marker needs no pid at all.
func TestFifoLoggerWritesMarkerOnStreamEOF(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log.json")
	fifoOut, fifoErr := filepath.Join(dir, "out.fifo"), filepath.Join(dir, "err.fifo")
	for _, p := range []string{fifoOut, fifoErr} {
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatalf("mkfifo %s: %v", p, err)
		}
	}

	runErr := make(chan error, 1)
	go func() {
		runErr <- runFifoJSONLogger(logPath, logRotation{}, loggerFifos{stdout: fifoOut, stderr: fifoErr}, true)
	}()

	// Opening the write ends releases the logger's blocking opens; closing
	// them is the shim closing the fifos at the process exit.
	w, err := os.OpenFile(fifoOut, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	we, err := os.OpenFile(fifoErr, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	we.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("runFifoJSONLogger: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fifo logger did not finish")
	}
	recs := readLogLines(t, logPath)
	if len(recs) != 2 || recs[0].Log != "hi\n" || !recs[1].EOF {
		t.Fatalf("records = %+v, want [hi record, marker]", recs)
	}
}
