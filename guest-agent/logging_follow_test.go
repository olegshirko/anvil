package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// docker logs -f on a running container must not end on a timer: the follow
// loop runs until the stop condition fires (container exit / client
// disconnect), however long that takes.
func TestFollowRunsUntilStopFires(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log.json")
	writeLine := func(s string) {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		f.WriteString(`{"time":"2026-01-01T00:00:00Z","stream":"stdout","log":"` + s + `\n"}` + "\n")
	}
	writeLine("first")

	stop := make(chan struct{})
	var emitted []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = readTaskLog(logPath, logReadOptions{
			follow: true,
			tail:   -1,
			stop: func() bool {
				select {
				case <-stop:
					return true
				default:
					return false
				}
			},
		}, func(stream byte, line []byte) {
			emitted = append(emitted, string(line))
		})
	}()

	// The old code killed this at a hard 30s; here the stream must survive
	// arbitrary quiet periods while the "container" keeps running.
	time.Sleep(300 * time.Millisecond)
	writeLine("second")
	time.Sleep(300 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("follow loop did not end after stop fired")
	}
	if len(emitted) != 2 {
		t.Errorf("expected 2 lines, got %d: %q", len(emitted), emitted)
	}
}

// Without a stop condition, a follow on a task whose log file never appears
// must still give up (bounded wait), instead of looping forever.
func TestFollowWithoutStopGivesUpOnMissingFile(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the 30s missing-file deadline")
	}
	start := time.Now()
	_ = readTaskLog(filepath.Join(t.TempDir(), "absent.json"), logReadOptions{
		follow: true, tail: -1,
	}, func(stream byte, line []byte) {})
	if elapsed := time.Since(start); elapsed > 35*time.Second {
		t.Errorf("missing-file wait took too long: %v", elapsed)
	}
	if elapsed := time.Since(start); elapsed < 29*time.Second {
		t.Errorf("missing-file wait gave up too early: %v", elapsed)
	}
}
