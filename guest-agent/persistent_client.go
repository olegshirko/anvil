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

// get returns the underlying containerd client, reconnecting if
// necessary. The returned client must NOT be closed by the caller.
func (pc *persistentClient) get(ctx context.Context) (*client.Client, error) {
	if pc == nil {
		return nil, fmt.Errorf("persistent client not initialized")
	}
	pc.mu.RLock()
	c := pc.conn
	pc.mu.RUnlock()

	if c != nil {
		// Verify the connection is alive with a lightweight operation.
		if _, err := c.NamespaceService().List(ctx); err == nil {
			return c, nil
		}
		// Connection lost — fall through to reconnect.
	}

	pc.mu.Lock()
	defer pc.mu.Unlock()

	// Double-check after acquiring write lock (another goroutine may have
	// reconnected while we waited).
	if pc.conn != nil {
		if _, err := pc.conn.NamespaceService().List(ctx); err == nil {
			return pc.conn, nil
		}
		// Close the dead connection before reconnecting.
		pc.conn.Close()
		pc.conn = nil
	}

	// Connect with retry loop (mimics scanner.go pattern). Honors ctx
	// cancellation so handlers do not pile up on the write lock forever
	// when containerd is down.
	for {
		c, err := client.New(pc.address)
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
