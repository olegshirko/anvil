package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/client"
)

// pc is the package-level persistent containerd client shared by all
// Docker API handlers. Initialized in main() before starting the API server.
var pc *persistentClient

// persistentClient manages a single containerd client with automatic
// reconnection. All Docker API handlers use this instead of creating
// new gRPC connections per request (~40 client.New() calls eliminated).
type persistentClient struct {
	mu      sync.RWMutex
	conn    *client.Client
	address string
}

func newPersistentClient(address string) *persistentClient {
	return &persistentClient{address: address}
}

// get returns the underlying containerd client, connecting first if there
// is none yet. The returned client must NOT be closed by the caller.
//
// An established client is never probed or replaced: gRPC reconnects on its
// own when containerd restarts. Probing with the caller's context used to
// close the shared client whenever a request had been cancelled (Ctrl-C, a
// compose sibling failing), which broke every in-flight call on it — task
// Wait streams included, which the exit watchers then took for container
// exits.
func (pc *persistentClient) get(ctx context.Context) (*client.Client, error) {
	if pc == nil {
		return nil, fmt.Errorf("persistent client not initialized")
	}
	pc.mu.RLock()
	c := pc.conn
	pc.mu.RUnlock()
	if c != nil {
		return c, nil
	}

	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.conn != nil {
		return pc.conn, nil
	}

	// Connect with retry loop (mimics scanner.go pattern). Honors ctx
	// cancellation so handlers do not pile up on the write lock forever
	// when containerd is down.
	for {
		c, err := client.New(pc.address, client.WithDefaultPlatform(defaultPlatformMatcher()))
		if err == nil {
			log.Printf("[persistent-client] connected to containerd")
			pc.conn = c
			return c, nil
		}
		log.Printf("[persistent-client] waiting for containerd: %v", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connecting to containerd: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// close shuts down the persistent client.
func (pc *persistentClient) close() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.conn != nil {
		pc.conn.Close()
		pc.conn = nil
	}
}
