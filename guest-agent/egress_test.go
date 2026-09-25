package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
)

func TestCheckEgress(t *testing.T) {
	okLookup := func(context.Context, string) ([]string, error) { return []string{"1.2.3.4"}, nil }
	badLookup := func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }
	okDial := func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	timeoutDial := func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}
	}

	if msg := checkEgress(context.Background(), egressProbeTarget, okLookup, okDial); msg != "" {
		t.Fatalf("reachable: got %q", msg)
	}
	if msg := checkEgress(context.Background(), egressProbeTarget, badLookup, okDial); !strings.HasPrefix(msg, "DNS lookup") {
		t.Fatalf("dns failure: got %q", msg)
	}
	if msg := checkEgress(context.Background(), egressProbeTarget, okLookup, timeoutDial); !strings.HasPrefix(msg, "TCP connect") {
		t.Fatalf("connect failure: got %q", msg)
	}
}

// The exact failure seen behind a Tailscale exit node gets the hint; a
// registry that answers with an error does not.
func TestExplainEgressFailure(t *testing.T) {
	seen := errors.New(`failed to resolve reference "docker.io/library/postgres:16-alpine": failed to do request: ` +
		`Head "https://registry-1.docker.io/v2/library/postgres/manifests/16-alpine": dial tcp 98.95.133.188:443: i/o timeout`)
	if got := explainEgressFailure(seen).Error(); !strings.Contains(got, "cannot reach the internet") {
		t.Fatalf("timeout not explained: %s", got)
	}
	if !errors.Is(explainEgressFailure(seen), seen) {
		t.Fatal("the original error must stay wrapped")
	}
	unreach := fmt.Errorf("dial: %w", syscall.ENETUNREACH)
	if !isEgressFailure(unreach) {
		t.Fatal("ENETUNREACH is an egress failure")
	}
	for _, other := range []error{
		errors.New(`pull access denied for foo, repository does not exist`),
		errors.New(`unexpected status from HEAD request: 429 Too Many Requests`),
		nil,
	} {
		if isEgressFailure(other) {
			t.Errorf("%v must not be an egress failure", other)
		}
	}
}
