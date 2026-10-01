package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/mdlayher/vsock"
	bkclient "github.com/moby/buildkit/client"
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
	// The log lives on the virtiofs share so host-side debugging does not
	// need guest shell access.
	logFile, err := os.OpenFile(filepath.Join(anvilRunDir, "buildkitd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(buildkitdBin)
	// guest-agent (PID 1) runs with an almost empty environment; a child
	// with no PATH/HOME misbehaves subtly (registry credential lookup,
	// helper resolution). Give buildkitd a sane minimal env.
	cmd.Env = append([]string{
		"PATH=/bin:/sbin:/usr/bin:/usr/sbin:/opt/containerd/bin",
		"HOME=/root",
	}, buildkitdProxyEnv...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start buildkitd: %w", err)
	}
	// It inherited the agent's -999 OOM score; builds may be killed.
	if err := os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", cmd.Process.Pid), []byte("0"), 0o644); err != nil {
		log.Printf("[buildkit] oom_score_adj: %v", err)
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
			// Back off: a persistent failure (fd exhaustion) spun here.
			log.Printf("[buildkit] accept error: %v", err)
			time.Sleep(100 * time.Millisecond)
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
func pruneBuildCache(opts ...bkclient.PruneOption) (int64, []string, error) {
	if !buildkitUp() {
		return 0, []string{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	log.Printf("[buildkit] prune: connecting")
	c, err := bkclient.New(ctx, "unix://"+buildkitSocket)
	if err != nil {
		return 0, nil, fmt.Errorf("buildkit connect: %w", err)
	}
	defer c.Close()
	log.Printf("[buildkit] prune: connected")

	var reclaimed int64
	deleted := []string{}
	ch := make(chan bkclient.UsageInfo)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for u := range ch {
			reclaimed += u.Size
			deleted = append(deleted, u.ID)
		}
	}()
	err = c.Prune(ctx, ch, opts...)
	// The client sends every record synchronously and never closes the
	// channel; once Prune returns nothing else is sent, so close it here
	// and let the reader finish (no leaked goroutine, no racy total).
	close(ch)
	<-done
	if err != nil {
		return reclaimed, deleted, fmt.Errorf("buildkit prune: %w", err)
	}
	return reclaimed, deleted, nil
}

// buildPruneOptions translates POST /build/prune's query the way dockerd
// does: all, the space limits (keep-storage is the old reserved-space),
// until as the keep duration, and the cache-record filters.
func buildPruneOptions(q url.Values) ([]bkclient.PruneOption, error) {
	var opts []bkclient.PruneOption
	if queryBool(q, "all") {
		opts = append(opts, bkclient.PruneAll)
	}
	space := func(key string) (int64, error) {
		v := q.Get(key)
		if v == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, errInvalid("invalid %s: %s", key, v)
		}
		return n, nil
	}
	reserved, err := space("reserved-space")
	if err != nil {
		return nil, err
	}
	if reserved == 0 {
		if reserved, err = space("keep-storage"); err != nil {
			return nil, err
		}
	}
	maxUsed, err := space("max-used-space")
	if err != nil {
		return nil, err
	}
	minFree, err := space("min-free-space")
	if err != nil {
		return nil, err
	}
	var keep time.Duration
	var filter []string
	for key, values := range parseDockerFilters(q.Get("filters")) {
		vals := slices.Sorted(maps.Keys(values))
		switch key {
		case "until":
			if len(vals) != 1 {
				return nil, errInvalid("filters: until takes one value")
			}
			now := time.Now()
			ts, terr := parseUntil(vals[0], now)
			if terr != nil {
				return nil, errInvalid("filters: %v", terr)
			}
			keep = now.Sub(ts)
		case "id", "parent", "type", "description", "inuse", "shared", "private":
			switch len(vals) {
			case 0:
				filter = append(filter, key)
			case 1:
				op := "=="
				if key == "id" {
					op = "~="
				}
				filter = append(filter, key+op+vals[0])
			default:
				return nil, errInvalid("filters: %s takes one value", key)
			}
		default:
			return nil, errInvalid("filters: %q is not a build cache filter", key)
		}
	}
	if len(filter) > 0 {
		opts = append(opts, bkclient.WithFilter(filter))
	}
	if keep > 0 || reserved > 0 || maxUsed > 0 || minFree > 0 {
		opts = append(opts, bkclient.WithKeepOpt(keep, reserved, maxUsed, minFree))
	}
	return opts, nil
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
		writeJSONError(w, http.StatusMethodNotAllowed, "grpc hijack requires POST")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "hijacking not supported")
		return
	}
	// /session carries the client's buildkit session (context upload, secret
	// providers): tunnel the raw hijacked connection through a
	// control.Session stream so buildkitd registers it.
	if r.URL.Path == "/session" {
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
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
		writeJSONError(w, http.StatusInternalServerError, err.Error())
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
