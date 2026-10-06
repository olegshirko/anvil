package main

import (
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The source is drained while nobody reads, and the consumer later gets
// every byte in order, then the source's EOF.
func TestEagerReaderDrainsAhead(t *testing.T) {
	data := strings.Repeat("0123456789", 100000) // 1 MB
	src := &countingReader{r: strings.NewReader(data)}
	r := eagerReader(src, 4<<20)
	deadline := time.Now().Add(2 * time.Second)
	for src.n.Load() < int64(len(data)) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := src.n.Load(); got != int64(len(data)) {
		t.Fatalf("source drained %d of %d bytes before any read", got, len(data))
	}
	out, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(out, []byte(data)) {
		t.Fatalf("read %d bytes, err %v", len(out), err)
	}
}

// Past the limit the reader stops draining (backpressure).
func TestEagerReaderLimit(t *testing.T) {
	src := &countingReader{r: strings.NewReader(strings.Repeat("x", 1<<20))}
	r := eagerReader(src, 64<<10)
	time.Sleep(50 * time.Millisecond)
	if got := src.n.Load(); got > 256<<10 {
		t.Fatalf("drained %d bytes past a 64 KiB limit", got)
	}
	out, _ := io.ReadAll(r)
	if len(out) != 1<<20 {
		t.Fatalf("read %d bytes, want %d", len(out), 1<<20)
	}
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
