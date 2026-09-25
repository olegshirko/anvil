package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

// Internet reachability of the VM. The guest leaves the host through the
// macOS NAT (vmnet); a full-tunnel VPN on the host (a Tailscale exit node,
// a corporate VPN) can swallow that NATed traffic while DNS through the
// NAT gateway still answers. Every registry pull then dies with a bare
// "dial tcp …:443: i/o timeout" that says nothing about the cause.

// egressProbeTarget is the host every pull needs first.
const egressProbeTarget = "registry-1.docker.io:443"

type dialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// checkEgress resolves and connects to target. It returns "" when the VM can
// reach the internet, otherwise a one-line explanation naming the failed step.
func checkEgress(ctx context.Context, target string, lookup func(ctx context.Context, host string) ([]string, error), dial dialContextFunc) string {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return err.Error()
	}
	if _, err := lookup(ctx, host); err != nil {
		return fmt.Sprintf("DNS lookup of %s failed: %v", host, err)
	}
	conn, err := dial(ctx, "tcp", target)
	if err != nil {
		return fmt.Sprintf("TCP connect to %s failed: %v", target, err)
	}
	conn.Close()
	return ""
}

// egressStatus is the control-channel `egress` command.
func egressStatus() Response {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	var d net.Dialer
	if msg := checkEgress(ctx, egressProbeTarget, net.DefaultResolver.LookupHost, d.DialContext); msg != "" {
		return Response{Error: msg, ExitCode: 1}
	}
	return Response{Status: "ok"}
}

// isEgressFailure reports whether err looks like the VM cannot reach the
// internet at all: a connect timeout or an unreachable network, as opposed
// to a registry answering with an error (auth, not found, rate limit).
func isEgressFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() && strings.Contains(err.Error(), "dial tcp") {
		return true
	}
	if errors.Is(err, os.ErrDeadlineExceeded) && strings.Contains(err.Error(), "dial tcp") {
		return true
	}
	// Errors that crossed a string boundary (containerd resolver wraps them
	// with %v in places) lose their type.
	msg := err.Error()
	return strings.Contains(msg, "dial tcp") &&
		(strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "network is unreachable") ||
			strings.Contains(msg, "no route to host"))
}

// egressHint is appended to pull errors caused by missing internet access.
const egressHint = "the anvil VM cannot reach the internet. A full-tunnel VPN on the Mac " +
	"(e.g. a Tailscale exit node) can block VM traffic while the Mac itself stays online; " +
	"run `anvil doctor` to check"

// explainEgressFailure keeps err and, for connectivity failures, says why.
func explainEgressFailure(err error) error {
	if !isEgressFailure(err) {
		return err
	}
	return fmt.Errorf("%w — %s", err, egressHint)
}
