package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// json-file log rotation (--log-opt max-size=, max-file=). Docker keeps
// logs unbounded unless asked; anvil defaults to 100 MB x 3 files instead,
// because the VM's disk is a fixed-size image the Mac cannot easily clean
// up. Explicit --log-opt values win, including max-size=-1 (unbounded).

const (
	defaultLogMaxSize = 100 << 20
	defaultLogMaxFile = 3
)

// logRotation is the resolved policy; maxSize <= 0 means unbounded.
type logRotation struct {
	maxSize int64
	maxFile int
}

// logRotationFor resolves a container's --log-opt values over the defaults.
func logRotationFor(opts map[string]string) (logRotation, error) {
	r := logRotation{maxSize: defaultLogMaxSize, maxFile: defaultLogMaxFile}
	if v, ok := opts["max-size"]; ok {
		n, err := parseByteSize(v)
		if err != nil {
			return r, fmt.Errorf("log-opt max-size=%s: %v", v, err)
		}
		r.maxSize = n
	}
	if v, ok := opts["max-file"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return r, fmt.Errorf("log-opt max-file=%s: must be a positive integer", v)
		}
		r.maxFile = n
	}
	return r, nil
}

// parseByteSize parses Docker's sizes: "100", "10k", "20m", "1g" (binary
// units), and -1 for unbounded.
func parseByteSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "-1" {
		return -1, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "kb") || strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimRight(s, "kb")
	case strings.HasSuffix(s, "mb") || strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimRight(s, "mb")
	case strings.HasSuffix(s, "gb") || strings.HasSuffix(s, "g"):
		mult, s = 1<<30, strings.TrimRight(s, "gb")
	case strings.HasSuffix(s, "b"):
		s = strings.TrimSuffix(s, "b")
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size")
	}
	return int64(n * float64(mult)), nil
}

// rotatingLog is the logger's output: appends records, and when the next
// one would pass maxSize, shifts path -> path.1 -> ... -> path.(maxFile-1)
// and starts a fresh file. Safe for the logger's concurrent writers.
type rotatingLog struct {
	mu   sync.Mutex
	path string
	rot  logRotation
	f    *os.File
	size int64
}

func openRotatingLog(path string, rot logRotation) (*rotatingLog, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	size := int64(0)
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	return &rotatingLog{path: path, rot: rot, f: f, size: size}, nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rot.maxSize > 0 && l.size > 0 && l.size+int64(len(p)) > l.rot.maxSize {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *rotatingLog) rotate() error {
	l.f.Close()
	if l.rot.maxFile <= 1 {
		os.Remove(l.path) // max-file=1: keep only the current file
	} else {
		os.Remove(rotatedLogName(l.path, l.rot.maxFile-1))
		for i := l.rot.maxFile - 2; i >= 1; i-- {
			os.Rename(rotatedLogName(l.path, i), rotatedLogName(l.path, i+1)) //nolint:errcheck — gaps are fine
		}
		os.Rename(l.path, rotatedLogName(l.path, 1)) //nolint:errcheck
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	l.f, l.size = f, 0
	return nil
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

func rotatedLogName(path string, i int) string {
	return path + "." + strconv.Itoa(i)
}

// rotatedLogFiles lists a log's files oldest first: path.N ... path.1, path.
func rotatedLogFiles(path string) []string {
	var older []string
	for i := 1; ; i++ {
		if _, err := os.Stat(rotatedLogName(path, i)); err != nil {
			break
		}
		older = append(older, rotatedLogName(path, i))
	}
	out := make([]string, 0, len(older)+1)
	for i := len(older) - 1; i >= 0; i-- {
		out = append(out, older[i])
	}
	return append(out, path)
}

// loggerArgs parses the logger's argv: containerd builds it from the
// binary-v2 URI's query map, so the flag pairs come in any order.
func loggerArgs(args []string) (path string, rot logRotation, ok bool) {
	rot = logRotation{maxSize: defaultLogMaxSize, maxFile: defaultLogMaxFile}
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--log-json":
			path, ok = args[i+1], true
			i++
		case "--max-size":
			if n, err := strconv.ParseInt(args[i+1], 10, 64); err == nil {
				rot.maxSize = n
			}
			i++
		case "--max-file":
			if n, err := strconv.Atoi(args[i+1]); err == nil && n >= 1 {
				rot.maxFile = n
			}
			i++
		}
	}
	return path, rot, ok
}
