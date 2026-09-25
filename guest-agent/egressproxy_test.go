package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func resetDirectEgress(t *testing.T) {
	t.Helper()
	directEgress.Lock()
	directEgress.badUntil, directEgress.logged = time.Time{}, false
	directEgress.Unlock()
	oldDirect, oldHost := dialDirect, dialHost
	t.Cleanup(func() {
		dialDirect, dialHost = oldDirect, oldHost
		directEgress.Lock()
		directEgress.badUntil, directEgress.logged = time.Time{}, false
		directEgress.Unlock()
	})
}

// fakeHost plays vz-runner: it reads the request frame, answers, and then
// echoes the stream back.
func fakeHost(t *testing.T, targets chan<- string, replyErr string) func() (net.Conn, error) {
	return func() (net.Conn, error) {
		guest, host := net.Pipe()
		go func() {
			defer host.Close()
			var req hostEgressRequest
			if err := readFrame(host, &req); err != nil {
				return
			}
			targets <- req.Target
			if err := writeFrame(host, hostEgressReply{Error: replyErr}); err != nil || replyErr != "" {
				return
			}
			_, _ = io.Copy(host, host)
		}()
		return guest, nil
	}
}

func timeoutErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}
}

// Behind a dead NAT path the first connect times out, later ones skip the
// direct attempt, and all of them reach the target through the host.
func TestDialOutFallsBackToHost(t *testing.T) {
	resetDirectEgress(t)
	directCalls := 0
	dialDirect = func(context.Context, string, string) (net.Conn, error) {
		directCalls++
		return nil, timeoutErr()
	}
	targets := make(chan string, 4)
	dialHost = fakeHost(t, targets, "")

	for i := 0; i < 2; i++ {
		conn, err := dialOut(context.Background(), "tcp", "registry-1.docker.io:443")
		if err != nil {
			t.Fatal(err)
		}
		if got := <-targets; got != "registry-1.docker.io:443" {
			t.Fatalf("host asked for %q", got)
		}
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
			t.Fatalf("stream through host: %q, %v", buf, err)
		}
		conn.Close()
	}
	if directCalls != 1 {
		t.Fatalf("direct dial attempted %d times, want 1 (then cached as broken)", directCalls)
	}
}

// A working direct path never touches the host; a real refusal is returned
// as is instead of being retried through the host.
func TestDialOutPrefersDirect(t *testing.T) {
	resetDirectEgress(t)
	dialHost = func() (net.Conn, error) { t.Fatal("host used"); return nil, nil }
	a, b := net.Pipe()
	defer b.Close()
	dialDirect = func(context.Context, string, string) (net.Conn, error) { return a, nil }
	if conn, err := dialOut(context.Background(), "tcp", "example.com:443"); err != nil || conn != a {
		t.Fatalf("direct: %v", err)
	}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	dialDirect = func(context.Context, string, string) (net.Conn, error) { return nil, refused }
	if _, err := dialOut(context.Background(), "tcp", "example.com:443"); !errors.Is(err, refused) {
		t.Fatalf("refusal must be returned as is: %v", err)
	}
}

func TestDialViaHostError(t *testing.T) {
	resetDirectEgress(t)
	targets := make(chan string, 1)
	dialHost = fakeHost(t, targets, "refusing loopback target")
	if _, err := dialViaHost(context.Background(), "127.0.0.1:22"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("host error not surfaced: %v", err)
	}
}

// buildkitd's HTTPS_PROXY: CONNECT, 200, then a transparent stream.
func TestEgressProxyConnect(t *testing.T) {
	client, server := net.Pipe()
	var dialed string
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = addr
		up, echo := net.Pipe()
		go func() { defer echo.Close(); _, _ = io.Copy(echo, echo) }()
		return up, nil
	}
	go handleEgressProxyClient(server, dial)

	if _, err := io.WriteString(client, "CONNECT registry-1.docker.io:443 HTTP/1.1\r\nHost: registry-1.docker.io:443\r\n\r\nhello"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(client)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT response: %v %v", resp, err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("bytes after CONNECT: %q, %v", buf, err)
	}
	if dialed != "registry-1.docker.io:443" {
		t.Fatalf("dialed %q", dialed)
	}
	client.Close()

	c2, s2 := net.Pipe()
	defer c2.Close()
	go handleEgressProxyClient(s2, dial)
	if _, err := io.WriteString(c2, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err = http.ReadResponse(bufio.NewReader(c2), nil)
	if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("plain HTTP must be refused: %v %v", resp, err)
	}
}
