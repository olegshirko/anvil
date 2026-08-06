package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/mdlayher/vsock"
)

// buildkitd is started lazily: it idles at ~50 MB RSS, so it is launched on
// the first build request (the classic /build endpoint via nerdctl, or a
// buildx remote-driver connection over vsock:1026) instead of at boot.
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

	if err := os.MkdirAll("/var/lib/buildkit", 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile("/tmp/buildkitd.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(buildkitdBin)
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
	cmd := exec.Command("/opt/containerd/bin/buildctl", "--addr", "unix://"+buildkitSocket, "prune")
	cmd.Env = append(cmd.Env, "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("buildctl prune: %v: %s", err, stripANSI(string(out)))
	}
	// buildctl prune ends with a "Total: <size>" summary line.
	var reclaimed int64
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Total:") {
			reclaimed = parseHumanSize(strings.TrimSpace(strings.TrimPrefix(line, "Total:")))
		}
	}
	return reclaimed, nil
}
