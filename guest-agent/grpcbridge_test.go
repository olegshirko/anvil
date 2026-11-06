package main

import (
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// A finished buildx session must not leave Serve blocked in Accept forever:
// the listener hands out its single connection and Close (which gRPC calls
// on Stop) releases the next Accept with net.ErrClosed.
func TestSingleConnListenerOneShot(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	l := newSingleConnListener(server)

	conn, err := l.Accept()
	if err != nil || conn != server {
		t.Fatalf("first Accept = %v, %v; want the hijacked conn", conn, err)
	}

	closed := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		closed <- err
	}()
	select {
	case err := <-closed:
		t.Fatalf("Accept returned %v before Close", err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-closed:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock Accept")
	}

	// Close is called by gRPC more than once (Stop + the Serve defer).
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// serveBuildkitGRPC stops its per-session gRPC server when the client goes
// away; otherwise every buildx connection leaked a server and a goroutine.
func TestSingleConnServerStopsOnClientClose(t *testing.T) {
	server, client := net.Pipe()
	hijacked := newNotifyConn(server)
	gs := grpc.NewServer()
	go func() {
		<-hijacked.done
		gs.Stop()
	}()
	done := make(chan error, 1)
	go func() { done <- gs.Serve(newSingleConnListener(hijacked)) }()

	// The client connects and immediately disappears (a buildx probe that
	// was cancelled, a builder that stopped).
	client.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v, want nil after Stop", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the client disconnected")
	}
}

// Only a successful dial is cached: caching buildkitd's startup error broke
// every build until the guest-agent restarted.
func TestLazyConnRetriesAfterFailure(t *testing.T) {
	var l lazyConn
	calls := 0
	dial := func() (*grpc.ClientConn, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("buildkitd is not up yet")
		}
		return &grpc.ClientConn{}, nil
	}
	if _, err := l.get(dial); err == nil {
		t.Fatal("first dial must surface the error")
	}
	cc, err := l.get(dial)
	if err != nil || cc == nil {
		t.Fatalf("second dial = %v, %v; want a cached connection", cc, err)
	}
	if calls != 2 {
		t.Fatalf("dial calls = %d, want 2", calls)
	}
	again, err := l.get(dial)
	if err != nil || again != cc {
		t.Fatalf("cached get = %v, %v; want the same connection", again, err)
	}
	if calls != 2 {
		t.Fatalf("dial calls after success = %d, want 2 (success cached)", calls)
	}
}
