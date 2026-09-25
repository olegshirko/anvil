package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// replay concatenates the records' payloads the way attach/logs emit them.
func (s *syncBuf) replay(t *testing.T) (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out strings.Builder
	n := 0
	for _, raw := range bytes.Split(bytes.TrimRight(s.b.Bytes(), "\n"), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var rec logLine
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("bad record %q: %v", raw, err)
		}
		out.WriteString(rec.Log)
		n++
	}
	return out.String(), n
}

func TestWriteStreamLines(t *testing.T) {
	out := &syncBuf{}
	writeStream(out, "t-lines", strings.NewReader("one\ntwo\n"))
	if got, n := out.replay(t); got != "one\ntwo\n" || n != 2 {
		t.Fatalf("got %q in %d records", got, n)
	}
}

// `printf abc` in a container that stays alive: the stream never ends, so
// the tail must be flushed by the quiet timer, promptly.
func TestPartialTailIsFlushedWhenQuiet(t *testing.T) {
	out := &syncBuf{}
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { writeStream(out, "t-tail", r); close(done) }()
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // let the reader buffer it
	flushQuietPartials(out, time.Now())
	if got, _ := out.replay(t); got != "" {
		t.Fatalf("flushed before the stream went quiet: %q", got)
	}
	flushQuietPartials(out, time.Now().Add(partialFlushAfter))
	if got, _ := out.replay(t); got != "abc" {
		t.Fatalf("quiet tail not flushed: %q", got)
	}
	w.Close()
	<-done
}

// A line written in two pieces with a pause: the first piece is flushed on
// its own, and must not be written a second time when the newline arrives.
func TestSlowLineIsNotDuplicated(t *testing.T) {
	out := &syncBuf{}
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { writeStream(out, "t-slow", r); close(done) }()
	w.Write([]byte("a"))
	time.Sleep(20 * time.Millisecond)
	flushQuietPartials(out, time.Now().Add(partialFlushAfter))
	w.Write([]byte("b\n"))
	w.Close()
	<-done
	if got, n := out.replay(t); got != "ab\n" || n != 2 {
		t.Fatalf("replayed %q in %d records, want \"ab\\n\" in 2", got, n)
	}
}

// A newline-less flood is written in bounded records, and nothing is lost.
func TestHugePartialIsBounded(t *testing.T) {
	out := &syncBuf{}
	big := strings.Repeat("x", 3*maxPartialRecord+5)
	writeStream(out, "t-huge", strings.NewReader(big))
	got, n := out.replay(t)
	if got != big {
		t.Fatalf("replayed %d bytes, want %d", len(got), len(big))
	}
	if n < 3 {
		t.Fatalf("%d records, want the flood split", n)
	}
}
