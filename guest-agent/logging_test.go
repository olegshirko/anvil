package main

import (
	"os"
	"path/filepath"
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
