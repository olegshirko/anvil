package main

import (
	"bufio"
	"strings"
	"testing"
)

// consumeSetupReport walks the init helper's setup-phase report stream: "P
// <pid>" lines (the init exists), then "R" (setup complete) — or an error.
func TestConsumeSetupReportReady(t *testing.T) {
	buf := bufio.NewReader(strings.NewReader("R\n"))
	pid, ready, err := consumeSetupReport(buf)
	if err != nil {
		t.Fatalf("ready line: %v", err)
	}
	if !ready || pid != 0 {
		t.Fatalf("ready line: got ready=%v pid=%d, want ready=true pid=0", ready, pid)
	}
}

func TestConsumeSetupReportPidThenReady(t *testing.T) {
	buf := bufio.NewReader(strings.NewReader("P 4321\nR\n"))
	pid, ready, err := consumeSetupReport(buf)
	if err != nil || ready {
		t.Fatalf("pid line: err=%v ready=%v, want pid only", err, ready)
	}
	if pid != 4321 {
		t.Fatalf("pid line: got pid=%d, want 4321", pid)
	}
	pid, ready, err = consumeSetupReport(buf)
	if err != nil {
		t.Fatalf("ready after pid: %v", err)
	}
	if !ready || pid != 0 {
		t.Fatalf("ready after pid: got ready=%v pid=%d, want ready=true pid=0", ready, pid)
	}
}

func TestConsumeSetupReportSetupFailure(t *testing.T) {
	buf := bufio.NewReader(strings.NewReader("E pivot_root: device busy\n"))
	_, _, err := consumeSetupReport(buf)
	if err == nil || !strings.Contains(err.Error(), "pivot_root: device busy") {
		t.Fatalf("setup failure: got err=%v, want the detail text", err)
	}
}

func TestConsumeSetupReportEOF(t *testing.T) {
	buf := bufio.NewReader(strings.NewReader(""))
	if _, _, err := consumeSetupReport(buf); err == nil {
		t.Fatal("empty stream: want an error, got nil")
	}
	// A truncated line (helper died mid-report) is an error too.
	buf = bufio.NewReader(strings.NewReader("P 12"))
	if _, _, err := consumeSetupReport(buf); err == nil {
		t.Fatal("truncated line: want an error, got nil")
	}
}

func TestConsumeSetupReportBadLines(t *testing.T) {
	for _, line := range []string{"Q junk\n", "P abc\n", "P 0\n", "P -5\n", "X boom\n"} {
		buf := bufio.NewReader(strings.NewReader(line))
		if _, _, err := consumeSetupReport(buf); err == nil {
			t.Errorf("%q: want an error, got nil", line)
		}
	}
}
