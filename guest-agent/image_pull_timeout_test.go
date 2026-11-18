package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestParsePullDuration(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
		ok   bool
	}{
		{"90", 90 * time.Second, true},
		{" 45 \n", 45 * time.Second, true},
		{"90s", 90 * time.Second, true},
		{"2m", 2 * time.Minute, true},
		{"1500ms", 1500 * time.Millisecond, true},
		{"", 0, false},
		{"0", 0, false},
		{"-5", 0, false},
		{"soon", 0, false},
	}
	for _, tc := range cases {
		got, ok := parsePullDuration(tc.raw)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parsePullDuration(%q) = %v, %v; want %v, %v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPullDurationSettingPrecedence(t *testing.T) {
	oldRun := anvilRunDir
	anvilRunDir = t.TempDir()
	t.Cleanup(func() { anvilRunDir = oldRun })

	if err := os.WriteFile(filepath.Join(anvilRunDir, "pull-timeout"), []byte("7s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANVIL_PULL_TIMEOUT", "")
	if got := pullDurationSetting("ANVIL_PULL_TIMEOUT", "pull-timeout", time.Minute); got != 7*time.Second {
		t.Fatalf("file setting = %v, want 7s", got)
	}

	t.Setenv("ANVIL_PULL_TIMEOUT", "12")
	if got := pullDurationSetting("ANVIL_PULL_TIMEOUT", "pull-timeout", time.Minute); got != 12*time.Second {
		t.Fatalf("env setting = %v, want 12s", got)
	}

	// An invalid env value falls back to the default, not to the file.
	t.Setenv("ANVIL_PULL_TIMEOUT", "nonsense")
	if got := pullDurationSetting("ANVIL_PULL_TIMEOUT", "pull-timeout", 90*time.Second); got != 90*time.Second {
		t.Fatalf("invalid env = %v, want default 90s", got)
	}

	// No env, no file: default.
	t.Setenv("ANVIL_PULL_TIMEOUT", "")
	if got := pullDurationSetting("ANVIL_PULL_MISSING", "missing-file", 33*time.Second); got != 33*time.Second {
		t.Fatalf("default = %v, want 33s", got)
	}
}

func TestStallConnTimesOutSilentRead(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	conn := &stallConn{Conn: client, idle: 50 * time.Millisecond}

	start := time.Now()
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("Read returned no error on a silent connection")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("Read error = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stall timeout took %v", elapsed)
	}
	if !isStallTimeout(err) {
		t.Fatal("isStallTimeout did not recognize the deadline error")
	}
}

func TestStallConnKeepsProgressingStreamAlive(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	conn := &stallConn{Conn: client, idle: 80 * time.Millisecond}

	go func() {
		// Each write arrives well inside the idle window, so a slow but
		// progressing stream must survive far longer than the stall timeout.
		for i := 0; i < 5; i++ {
			time.Sleep(25 * time.Millisecond)
			_, _ = server.Write([]byte{'x'})
		}
	}()
	buf := make([]byte, 1)
	for i := 0; i < 5; i++ {
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
}

func TestStallWrappedDial(t *testing.T) {
	if stallWrappedDial(nil, time.Second) != nil {
		t.Fatal("nil dial must stay nil")
	}
	var got net.Conn
	dial := func(context.Context, string, string) (net.Conn, error) {
		server, client := net.Pipe()
		t.Cleanup(func() { server.Close(); client.Close() })
		got = client
		return client, nil
	}
	conn, err := stallWrappedDial(dial, 50*time.Millisecond)(context.Background(), "tcp", "x:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := conn.(*stallConn); !ok {
		t.Fatalf("dial returned %T, want *stallConn", conn)
	}
	if conn.(*stallConn).idle != 50*time.Millisecond || got == nil {
		t.Fatal("stall wrapper lost the idle duration or the connection")
	}
}

func TestStopOnFirstLayer(t *testing.T) {
	stopped := 0
	h := stopOnFirstLayer(func() { stopped++ })
	ctx := context.Background()

	if _, err := h.Handle(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig}); err != nil {
		t.Fatal(err)
	}
	if stopped != 0 {
		t.Fatalf("stop fired on metadata, calls=%d", stopped)
	}
	layer := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip}
	for i := 0; i < 3; i++ {
		if _, err := h.Handle(ctx, layer); err != nil {
			t.Fatal(err)
		}
	}
	if stopped != 1 {
		t.Fatalf("stop calls=%d, want exactly 1", stopped)
	}
}

func TestRetryablePullError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"phase timeout", errPullPhaseTimeout, false},
		{"stall", errPullStall, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"generic transient", errors.New("read: connection reset by peer"), true},
	}
	for _, tc := range cases {
		if got := retryablePullError(tc.err); got != tc.want {
			t.Errorf("%s: retryablePullError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

func TestPullFallbackErrorNamesBothPaths(t *testing.T) {
	err := &pullFallbackError{
		ref:      "alpine:3.21",
		registry: errPullPhaseTimeout,
		mirror:   errMirrorNotFound,
	}
	msg := err.Error()
	for _, want := range []string{"alpine:3.21", "registry attempt", "resolve/manifest phase timed out", dockerMirrorRepo} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "context deadline exceeded") {
		t.Errorf("message %q leaks a bare context error", msg)
	}
	if !errors.Is(err, errPullPhaseTimeout) {
		t.Error("pullFallbackError must unwrap to the registry error")
	}
}
