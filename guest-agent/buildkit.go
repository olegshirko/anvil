package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/mdlayher/vsock"
)

// buildkitd is started lazily: it idles at ~50 MB RSS, so it is launched on
// the first build request (the classic /build endpoint through its gRPC API,
// or a buildx remote-driver connection over vsock:1026) instead of at boot.
const (
	buildkitVsockPort = 1026
	buildkitSocket    = "/run/buildkit/buildkitd.sock"
	buildkitdBin      = "/opt/containerd/bin/buildkitd"
)

var buildkitMu sync.Mutex

func buildkitUp() bool {
	conn, err := net.DialTimeout("unix", buildkitSocket, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ensureBuildkitd starts buildkitd if its socket does not answer yet and
// waits for readiness. Safe to call concurrently.
func ensureBuildkitd() error {
	if buildkitUp() {
		return nil
	}
	buildkitMu.Lock()
	defer buildkitMu.Unlock()
	if buildkitUp() {
		return nil
	}

	// Kill stray buildkitd instances first: racing socket probes can leave
	// behind strays that either hold the socket with a registry-resolving
	// worker or leak. One canonical instance with /etc/buildkit/buildkitd.toml
	// (containerd worker, local FROM resolution) must own /run/buildkit.
	killallStaleBuildkitd()

	// buildkitd ships as a tarball extracted to /var/lib/buildkit by a
	// background stage2 job on first boot; a build racing that extraction
	// would see a dangling symlink. Wait (bounded) for it to appear.
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(buildkitdBin); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := os.MkdirAll("/var/lib/buildkit", 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile("/tmp/buildkitd.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(buildkitdBin)
	// guest-agent (PID 1) runs with an almost empty environment; a child
	// with no PATH/HOME misbehaves subtly (registry credential lookup,
	// helper resolution). Give buildkitd a sane minimal env.
	cmd.Env = []string{
		"PATH=/bin:/sbin:/usr/bin:/usr/sbin:/opt/containerd/bin",
		"HOME=/root",
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start buildkitd: %w", err)
	}
	// No Wait(): if buildkitd dies it is collected by the orphan reaper.
	log.Printf("[buildkit] started buildkitd (pid %d)", cmd.Process.Pid)
	for i := 0; i < 150; i++ {
		if buildkitUp() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("buildkitd did not open %s in time", buildkitSocket)
}

// killallStaleBuildkitd SIGKILLs every running buildkitd except our own pid
// tree member named in skipPID (0 = skip none). guest-agent is PID 1, so
// "ours" are the ones it spawned; simplest robust policy: kill all — the
// caller only runs this when the socket is dead, i.e. no usable instance
// is serving.
func killallStaleBuildkitd() {
	out, err := exec.Command("/bin/sh", "-c",
		"for p in /proc/[0-9]*; do c=$(tr '\\0' ' ' < $p/cmdline 2>/dev/null); case \"$c\" in *buildkitd*) kill -9 ${p#/proc/} 2>/dev/null;; esac; done").Output()
	_ = out
	if err == nil {
		log.Printf("[buildkit] killed stale buildkitd processes")
	}
	time.Sleep(200 * time.Millisecond)
}

// serveBuildkitBridge listens on vsock:1026 and pumps each connection to the
// buildkitd unix socket. The host proxies ~/.anvil-vz/buildkit.sock to this
// port, enabling the buildx remote driver (`docker buildx create --driver
// remote unix://.../buildkit.sock`).
func serveBuildkitBridge() {
	l, err := vsock.Listen(buildkitVsockPort, nil)
	if err != nil {
		log.Printf("[buildkit] vsock listen %d: %v", buildkitVsockPort, err)
		return
	}
	defer l.Close()
	log.Printf("listening on vsock port %d (buildkit bridge)", buildkitVsockPort)
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("[buildkit] accept error: %v", err)
			continue
		}
		go proxyBuildkitConn(conn)
	}
}

func proxyBuildkitConn(conn net.Conn) {
	defer conn.Close()
	if err := ensureBuildkitd(); err != nil {
		log.Printf("[buildkit] %v", err)
		return
	}
	target, err := net.DialTimeout("unix", buildkitSocket, 5*time.Second)
	if err != nil {
		log.Printf("[buildkit] dial %s: %v", buildkitSocket, err)
		return
	}
	defer target.Close()
	go func() {
		io.Copy(target, conn)
		if tc, ok := target.(*net.UnixConn); ok {
			tc.CloseWrite()
		}
	}()
	io.Copy(conn, target)
}

// pruneBuildCache drops the whole buildkit cache and returns the reclaimed
// bytes. It deliberately does not start buildkitd just to prune: without a
// running daemon there is no cache to reclaim.
func pruneBuildCache() (int64, error) {
	if !buildkitUp() {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	log.Printf("[buildkit] prune: connecting")
	c, err := bkclient.New(ctx, "unix://"+buildkitSocket)
	if err != nil {
		return 0, fmt.Errorf("buildkit connect: %w", err)
	}
	defer c.Close()
	log.Printf("[buildkit] prune: connected")

	var reclaimed int64
	ch := make(chan bkclient.UsageInfo)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for u := range ch {
			reclaimed += u.Size
		}
	}()
	err = c.Prune(ctx, ch, bkclient.PruneAll)
	// The client does not close the usage channel when the RPC completes;
	// drain briefly for pending records instead of blocking forever.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	if err != nil {
		return reclaimed, fmt.Errorf("buildkit prune: %w", err)
	}
	return reclaimed, nil
}

// handleBuildkitGRPC implements the dockerd-style gRPC hijack endpoints
// (/grpc). buildx's "docker" driver probes POST /grpc with an h2c upgrade:
// if the daemon accepts, the driver talks the full buildkit control gRPC
// API over the hijacked connection. Without this endpoint docker CLI 29
// synthesizes a docker-container "context builder" for the docker context
// instead, spawning a buildkitd-in-container that cannot resolve registries
// behind the VZ NAT DNS forwarder. The connection is served by a control-API
// proxy onto the guest's own buildkitd (see grpcbridge.go).
func handleBuildkitGRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"message":"grpc hijack requires POST"}`, http.StatusMethodNotAllowed)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, `{"message":"hijacking not supported"}`, http.StatusInternalServerError)
		return
	}
	// /session carries the client's buildkit session (context upload, secret
	// providers): tunnel the raw hijacked connection through a
	// control.Session stream so buildkitd registers it.
	if r.URL.Path == "/session" {
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(bufrw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n")
		if err := bufrw.Flush(); err != nil {
			conn.Close()
			return
		}
		go bridgeSession(prefixConn{Conn: conn, r: io.MultiReader(bufrw.Reader, conn)}, r.Header)
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	proto := r.Header.Get("Upgrade")
	if proto == "" {
		proto = "h2c"
	}
	fmt.Fprintf(bufrw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", proto)
	if err := bufrw.Flush(); err != nil {
		conn.Close()
		return
	}
	// The client may have pipelined bytes after the upgrade request (the
	// HTTP/2 preface); the bufio reader holds them, so it is passed to the
	// gRPC bridge alongside the raw connection.
	if err := serveBuildkitGRPC(conn, bufrw.Reader); err != nil {
		log.Printf("[buildkit] grpc bridge: %v", err)
	}
}
