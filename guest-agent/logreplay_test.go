package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLogFile(t *testing.T, path string, from, to int, partial string) {
	t.Helper()
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "line-%d-%s\n", i, strings.Repeat("x", i%300))
	}
	b.WriteString(partial)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func collect(t *testing.T, path string, tail int) ([]string, int64) {
	t.Helper()
	var got []string
	off, err := replayLog(path, tail, func(raw []byte) { got = append(got, strings.SplitN(string(raw), "-", 3)[1]) })
	if err != nil {
		t.Fatal(err)
	}
	return got, off
}

func TestReplayLogAcrossRotationsAndTail(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "c.log")
	writeLogFile(t, rotatedLogName(live, 2), 1, 1000, "")
	writeLogFile(t, rotatedLogName(live, 1), 1001, 2000, "")
	writeLogFile(t, live, 2001, 2500, `{"partial`)

	all, off := collect(t, live, -1)
	if len(all) != 2500 || all[0] != "1" || all[2499] != "2500" {
		t.Fatalf("full replay: %d lines, first %v last %v", len(all), all[0], all[len(all)-1])
	}
	data, _ := os.ReadFile(live)
	if want := int64(len(data) - len(`{"partial`)); off != want {
		t.Fatalf("live offset = %d, want %d (before the unterminated record)", off, want)
	}

	for _, tail := range []int{1, 7, 500, 501, 1700, 5000} {
		got, toff := collect(t, live, tail)
		n := tail
		if n > 2500 {
			n = 2500
		}
		if len(got) != n || got[len(got)-1] != "2500" || got[0] != fmt.Sprint(2500-n+1) {
			t.Fatalf("tail %d: %d lines from %v to %v", tail, len(got), got[0], got[len(got)-1])
		}
		if toff != off {
			t.Fatalf("tail %d: live offset %d, want %d", tail, toff, off)
		}
	}
}

func TestReplayLogMissingAndEmpty(t *testing.T) {
	dir := t.TempDir()
	got, off := collect(t, filepath.Join(dir, "absent.log"), 10)
	if len(got) != 0 || off != 0 {
		t.Fatalf("absent: %v %d", got, off)
	}
	p := filepath.Join(dir, "partial.log")
	os.WriteFile(p, []byte("no newline yet"), 0o644) //nolint:errcheck
	got, off = collect(t, p, 10)
	if len(got) != 0 || off != 0 {
		t.Fatalf("only a partial record: %v %d", got, off)
	}
}
