package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Bounds on a registry pull. Without them a registry that accepts the TCP
// connection and then goes quiet (or stops mid-layer) keeps `docker pull`
// alive for as long as the process lives: containerd's own transport times
// out response headers, but not a body that never arrives.
//
// Two independent bounds:
//   - the resolve/manifest phase of each attempt (connect, auth, manifest
//     and config): ANVIL_PULL_TIMEOUT, default 60s;
//   - every registry read after that: no silence longer than
//     ANVIL_PULL_STALL_TIMEOUT, default 30s, refreshed on every byte.
//
// A large layer legitimately streams for minutes, so the blob phase has no
// total cap — but it can never go idle for the stall timeout, and a pull
// that keeps making progress is not a hang. A timeout means the registry is
// unreachable, so the caller goes to the mirror instead of retrying it.

var (
	// errPullPhaseTimeout marks an attempt that never reached the blob
	// phase. Kept as a sentinel so retry decisions do not depend on the
	// wording of the wrapped error.
	errPullPhaseTimeout = errors.New("registry resolve/manifest phase timed out")
	// errPullStall marks a connection that went silent mid-transfer.
	errPullStall = errors.New("registry connection stalled")
)

const (
	defaultPullPhaseTimeout = 60 * time.Second
	defaultPullStallTimeout = 30 * time.Second
)

func pullPhaseTimeout() time.Duration {
	return pullDurationSetting("ANVIL_PULL_TIMEOUT", "pull-timeout", defaultPullPhaseTimeout)
}

func pullStallTimeout() time.Duration {
	return pullDurationSetting("ANVIL_PULL_STALL_TIMEOUT", "pull-stall-timeout", defaultPullStallTimeout)
}

// pullDurationSetting reads an ANVIL_* setting from the environment, then
// from the value the host published to <share>/.anvil-run/<file> (see
// publishGuestSettings), then falls back to def. A plain integer means
// seconds; Go durations ("90s", "2m") are accepted too.
func pullDurationSetting(key, file string, def time.Duration) time.Duration {
	if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
		if d, ok := parsePullDuration(raw); ok {
			return d
		}
		log.Printf("[images] ignoring %s=%q: want a positive duration (e.g. 90s)", key, raw)
		return def
	}
	if raw, err := os.ReadFile(filepath.Join(anvilRunDir, file)); err == nil {
		if d, ok := parsePullDuration(string(raw)); ok {
			return d
		}
	}
	return def
}

// parsePullDuration accepts "90s", "2m" and a bare integer (seconds).
func parsePullDuration(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(raw); err == nil {
		if n <= 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// stallConn puts an idle deadline on every Read and Write. Refreshing it per
// operation means a stream that is actively delivering bytes is never cut
// off, while a connection that delivers nothing for the stall timeout
// surfaces as an i/o timeout instead of blocking the pull forever. Read
// deadlines are only armed while a Read is in flight, so keep-alive
// connections idling in the transport pool are unaffected.
type stallConn struct {
	net.Conn
	idle time.Duration
}

func (c *stallConn) Read(p []byte) (int, error) {
	if c.idle > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	}
	return c.Conn.Read(p)
}

func (c *stallConn) Write(p []byte) (int, error) {
	if c.idle > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	}
	return c.Conn.Write(p)
}

// stallWrappedDial returns dial with every accepted connection bounded by
// the pull stall timeout.
func stallWrappedDial(dial dialContextFunc, idle time.Duration) dialContextFunc {
	if dial == nil || idle <= 0 {
		return dial
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &stallConn{Conn: conn, idle: idle}, nil
	}
}

// registryTransportWithStall is the registry transport with its connections
// bounded by the stall timeout (auth, pull and push all share it).
func registryTransportWithStall() *http.Transport {
	t := registryTransport()
	t.DialContext = stallWrappedDial(t.DialContext, pullStallTimeout())
	return t
}

// mirrorTransport is the docker-mirror download transport: http.DefaultTransport's
// proxy behavior (a bare &http.Transport{} would silently drop
// ProxyFromEnvironment), a bounded header wait and stall-bounded connections
// routed the way every other registry connection is.
func mirrorTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	t.DialContext = stallWrappedDial(dialOut, pullStallTimeout())
	return t
}

// stopOnFirstLayer reports the moment the pull leaves the resolve/manifest
// phase: the first layer descriptor means the manifest was read and blobs
// are being fetched. Until then the attempt runs under the phase deadline.
func stopOnFirstLayer(stop func()) images.Handler {
	var once sync.Once
	return images.HandlerFunc(func(_ context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if isLayerType(desc.MediaType) {
			once.Do(stop)
		}
		return nil, nil
	})
}

// isPullTimeout reports a failure already bounded by one of our deadlines;
// retrying it would spend another full budget on a dead registry.
func isPullTimeout(err error) bool {
	return errors.Is(err, errPullPhaseTimeout) || errors.Is(err, errPullStall)
}

// isStallTimeout reports a transport read/write that hit the idle deadline
// (as opposed to a dial timeout or an HTTP status answer).
func isStallTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout() && !errors.Is(err, context.DeadlineExceeded)
}

// retryablePullError decides between one more registry attempt and the
// mirror. A definitive registry answer (missing, denied) is not retried; a
// timeout is not retried either — the registry had its whole budget. Only a
// fast transport failure (reset, EOF, 5xx after containerd's own retries)
// gets the single retry.
func retryablePullError(err error) bool {
	if err == nil {
		return false
	}
	if errdefs.IsNotFound(err) || errdefs.IsUnauthorized(err) ||
		errdefs.IsInvalidArgument(err) || errdefs.IsPermissionDenied(err) {
		return false
	}
	if isPullTimeout(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// describePullError renders one attempt's failure as a single line naming
// the mechanism, never a bare "context canceled".
func describePullError(err error) string {
	if err == nil {
		return ""
	}
	msg := stripANSI(explainEgressFailure(err).Error())
	if errors.Is(err, errPullStall) {
		return fmt.Sprintf("%s (no data for %s)", msg, pullStallTimeout())
	}
	return msg
}

func describeMirrorError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errMirrorNotFound) {
		return "image is not in " + dockerMirrorRepo
	}
	return stripANSI(err.Error())
}

// pullFallbackError reports both paths in one line. The registry error is
// unwrapped so callers keep containerd's classification (not-found, auth).
type pullFallbackError struct {
	ref      string
	registry error
	mirror   error
}

func (e *pullFallbackError) Error() string {
	return fmt.Sprintf("pull %s failed: registry attempt: %s; mirror fallback: %s",
		e.ref, describePullError(e.registry), describeMirrorError(e.mirror))
}

func (e *pullFallbackError) Unwrap() error { return e.registry }

// registryPullAttempts is how many bounded registry attempts one pull makes.
// containerd already retries transient transport errors inside an attempt;
// a second attempt here covers a reset that landed mid-transfer, while a
// timeout goes straight to the mirror.
const registryPullAttempts = 2

// pullWithMirrorFallback makes canonicalRef available in ns: bounded
// registry attempts first, then one docker-mirror load (platform-aware; the
// mirror carries arm64 and amd64 releases). It returns which path produced
// the image plus each path's error, so callers can report both reasons.
func pullWithMirrorFallback(ctx context.Context, ref, canonicalRef, ns, platform string, auth *registryAuth) (mirrorUsed bool, registryErr, mirrorErr error) {
	for attempt := 1; attempt <= registryPullAttempts; attempt++ {
		if attempt > 1 {
			log.Printf("[images] retrying registry pull of %q after a transient failure: %v", canonicalRef, registryErr)
			time.Sleep(500 * time.Millisecond)
		}
		registryErr = pullImageIntoNamespace(ctx, canonicalRef, ns, platform, auth)
		if registryErr == nil {
			return false, nil, nil
		}
		if !retryablePullError(registryErr) {
			break
		}
	}

	log.Printf("[images] registry pull of %q failed (%s); trying docker-mirror", ref, describePullError(registryErr))
	if prog := pullProgressFrom(ctx); prog != nil {
		prog.status(fmt.Sprintf("%s (%s); loading %s from docker-mirror",
			registryFailureSummary(registryErr), truncateLine(describePullError(registryErr), 120), ref))
	}
	mirrorErr = loadFromMirrorPlatform(ctx, ref, ns, platform)
	if mirrorErr == nil {
		return true, registryErr, nil
	}
	return false, registryErr, mirrorErr
}

// registryFailureSummary is the short human cause used in the pull status
// line: "unavailable" only when the registry never answered, not when it
// answered with a definitive error (missing image, denied).
func registryFailureSummary(err error) string {
	if isPullTimeout(err) || isEgressFailure(err) {
		return "Docker Hub unreachable"
	}
	return "Registry pull failed"
}

// truncateLine keeps the one-line progress status readable for long errors.
func truncateLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
