package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// topPsFixture builds two processes formatted the way /proc would report
// them, with a fixed clock so every derived cell is deterministic.
func topPsFixture() ([]procSnapshot, time.Time, time.Time) {
	boot := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	now := boot.Add(2 * time.Hour).Add(30 * time.Minute) // btime + 2h30m
	started := now.Add(-1 * time.Hour)                   // ran for 1h
	procs := []procSnapshot{
		{
			pid: 42, ppid: 1, comm: "nginx", cmd: "nginx: worker process",
			uid: "root", state: "S", tty: "pts/0",
			cpuTicks: 900 * 100, // 900s of CPU at USER_HZ=100
			// started 1h ago = 3600s of start ticks
			startTicks: uint64(started.Sub(boot).Seconds()) * 100,
			vszKB:      51200, rssKB: 2048,
		},
		{
			pid: 7, ppid: 42, comm: "sh", cmd: "sh -c sleep",
			uid: "1000", state: "R", tty: "?",
			cpuTicks:   0,
			startTicks: uint64(started.Sub(boot).Seconds()) * 100,
			vszKB:      1024, rssKB: 128,
		},
	}
	return procs, now, boot
}

func TestTopPSEFDefault(t *testing.T) {
	procs, now, boot := topPsFixture()
	titles, rows, err := psFormat(procs, now, boot, 4<<20, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantTitles := []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"}
	if !reflect.DeepEqual(titles, wantTitles) {
		t.Fatalf("titles = %v, want %v", titles, wantTitles)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// root ran 1h wall, 900s CPU: C = int(900/3600*100) = 25; TIME 00:15:00.
	want0 := []string{"root", "42", "1", "25", "09:30", "pts/0", "00:15:00", "nginx: worker process"}
	if !reflect.DeepEqual(rows[0], want0) {
		t.Fatalf("row0 = %v, want %v", rows[0], want0)
	}
	if rows[1][0] != "1000" || rows[1][5] != "?" {
		t.Fatalf("row1 uid/tty = %v/%v, want 1000/?", rows[1][0], rows[1][5])
	}
}

func TestTopPSAux(t *testing.T) {
	procs, now, boot := topPsFixture()
	titles, rows, err := psFormat(procs, now, boot, 4<<20, "aux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantTitles := []string{"USER", "PID", "%CPU", "%MEM", "VSZ", "RSS", "TTY", "STAT", "START", "TIME", "COMMAND"}
	if !reflect.DeepEqual(titles, wantTitles) {
		t.Fatalf("titles = %v, want %v", titles, wantTitles)
	}
	// %CPU = 900/3600*100 = 25.0; %MEM = 2048/4194304*100 = 0.048828125.
	if rows[0][2] != "25.0" {
		t.Fatalf("%%cpu = %q, want 25.0", rows[0][2])
	}
	if rows[0][3] != "0.0" {
		t.Fatalf("%%mem = %q, want 0.0 (rounded to 1 decimal like ps)", rows[0][3])
	}
	if rows[0][4] != "51200" || rows[0][5] != "2048" {
		t.Fatalf("vsz/rss = %q/%q, want 51200/2048", rows[0][4], rows[0][5])
	}
	if rows[0][7] != "S" {
		t.Fatalf("stat = %q, want S", rows[0][7])
	}
	if rows[1][2] != "0.0" {
		t.Fatalf("zero-cpu %%cpu = %q, want 0.0", rows[1][2])
	}
}

func TestTopPSAuxFlagsVariants(t *testing.T) {
	procs, now, boot := topPsFixture()
	for _, args := range []string{"aux", "-aux", "ax", "a x u", "ux"} {
		_, _, err := psFormat(procs, now, boot, 4<<20, args)
		if err != nil {
			t.Errorf("ps_args %q: unexpected error: %v", args, err)
		}
	}
	for _, args := range []string{"-ef", "-e f", "-e", "-f", "ef"} {
		titles, _, err := psFormat(procs, now, boot, 4<<20, args)
		if err != nil {
			t.Errorf("ps_args %q: unexpected error: %v", args, err)
		}
		if titles[0] != "UID" {
			t.Errorf("ps_args %q: titles[0] = %q, want UID", args, titles[0])
		}
	}
}

func TestTopPSOFormat(t *testing.T) {
	procs, now, boot := topPsFixture()
	titles, rows, err := psFormat(procs, now, boot, 4<<20, "-o pid,comm,%cpu")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantTitles := []string{"PID", "COMM", "%CPU"}
	if !reflect.DeepEqual(titles, wantTitles) {
		t.Fatalf("titles = %v, want %v", titles, wantTitles)
	}
	if rows[0][0] != "42" || rows[0][1] != "nginx" || rows[0][2] != "25.0" {
		t.Fatalf("row0 = %v, want [42 nginx 25.0]", rows[0])
	}
}

func TestTopPSOFieldAliases(t *testing.T) {
	procs, now, boot := topPsFixture()
	for _, args := range []string{"-o pid,ppid,user,args,cmdline,command,stat,state,s,vsz,rss,tty,etime,time,cputime,stime,uid"} {
		titles, rows, err := psFormat(procs, now, boot, 4<<20, args)
		if err != nil {
			t.Fatalf("ps_args %q: %v", args, err)
		}
		if len(titles) != 17 || len(rows[0]) != 17 {
			t.Fatalf("got %d columns, want 17", len(titles))
		}
	}
}

func TestTopPSOFormatSpaces(t *testing.T) {
	procs, now, boot := topPsFixture()
	titles, _, err := psFormat(procs, now, boot, 4<<20, "-o pid rss %mem")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(titles, []string{"PID", "RSS", "%MEM"}) {
		t.Fatalf("titles = %v, want [PID RSS %%MEM]", titles)
	}
}

func TestTopPSErrors(t *testing.T) {
	procs, now, boot := topPsFixture()
	if _, _, err := psFormat(procs, now, boot, 4<<20, "-l"); err == nil {
		t.Fatal("-l: expected an error, got none")
	}
	if _, _, err := psFormat(procs, now, boot, 4<<20, "-o pid,bogus"); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("-o pid,bogus: err = %v, want it to name bogus", err)
	}
	if _, _, err := psFormat(procs, now, boot, 4<<20, "--forest"); err == nil {
		t.Fatal("--forest: expected an error, got none")
	}
}

func TestTopPSEmptyProcs(t *testing.T) {
	_, now, boot := topPsFixture()
	titles, rows, err := psFormat(nil, now, boot, 4<<20, "aux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %v, want empty", rows)
	}
	if titles[0] != "USER" {
		t.Fatalf("titles[0] = %q, want USER", titles[0])
	}
}
