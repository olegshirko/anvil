package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseByteSize(t *testing.T) {
	for in, want := range map[string]int64{"100": 100, "10k": 10 << 10, "20m": 20 << 20, "1g": 1 << 30, "5MB": 5 << 20, "-1": -1} {
		if got, err := parseByteSize(in); err != nil || got != want {
			t.Errorf("%s = %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "0", "-5m"} {
		if _, err := parseByteSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := logRotationFor(map[string]string{"max-file": "0"}); err == nil {
		t.Error("max-file=0 accepted")
	}
	if r, _ := logRotationFor(nil); r.maxSize != defaultLogMaxSize || r.maxFile != defaultLogMaxFile {
		t.Errorf("defaults = %+v", r)
	}
}

func TestLoggerArgsAnyOrder(t *testing.T) {
	path, rot, ok := loggerArgs([]string{"--max-file", "5", "--log-json", "/l/x.json", "--max-size", "1024"})
	if !ok || path != "/l/x.json" || rot.maxFile != 5 || rot.maxSize != 1024 {
		t.Errorf("parsed %q %+v %v", path, rot, ok)
	}
	if _, _, ok := loggerArgs([]string{"daemon"}); ok {
		t.Error("non-logger argv parsed as logger")
	}
}

func TestRotatingLogShiftsAndReplays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	l, err := openRotatingLog(path, logRotation{maxSize: 100, maxFile: 3})
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for i := 0; i < 20; i++ {
		msg := strings.Repeat(string(rune('a'+i)), 10) + "\n"
		writeLogRecord(l, "stdout", []byte(msg))
		want = append(want, msg)
	}
	l.Close()
	files := rotatedLogFiles(path)
	if len(files) != 3 || files[0] != path+".2" || files[2] != path {
		t.Fatalf("files = %v", files)
	}
	for _, f := range files {
		if fi, _ := os.Stat(f); fi.Size() > 100 {
			t.Errorf("%s is %d bytes, over max-size", f, fi.Size())
		}
	}
	var got []string
	if err := readTaskLog(path, logReadOptions{tail: -1}, func(_ byte, line []byte) { got = append(got, string(line)) }); err != nil {
		t.Fatal(err)
	}
	// Oldest records rotated away; what is left is the newest, in order.
	if len(got) == 0 || got[len(got)-1] != want[len(want)-1] {
		t.Fatalf("replay tail = %q", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("replay out of order: %q", got)
		}
	}

	one := filepath.Join(t.TempDir(), "one.json")
	l, _ = openRotatingLog(one, logRotation{maxSize: 60, maxFile: 1})
	for i := 0; i < 5; i++ {
		writeLogRecord(l, "stdout", []byte("xxxxxxxxxx\n"))
	}
	l.Close()
	if files := rotatedLogFiles(one); len(files) != 1 {
		t.Errorf("max-file=1 kept %v", files)
	}
}
