package main

import (
	"io"
	"sync"
)

// stdinBufferLimit bounds what eagerReader holds for a process that is not
// reading yet; beyond it the client is back to ordinary backpressure.
const stdinBufferLimit = 256 << 20

// eagerReader reads src as fast as it delivers, into memory, and serves the
// bytes to the consumer at its own pace.
//
// Hijacked stdin arrives over virtio-vsock. When the guest stopped reading
// one connection (the exec'd process not started or not reading yet) while
// another streamed out heavily (`docker save | docker exec -i c ...`), the
// guest kernel's vsock rx work spun on CPU and the VM locked up (Linux 6.6
// soft lockup in virtio_transport_rx_work). Draining stdin eagerly keeps the
// kernel's receive buffer from filling in the first place.
func eagerReader(src io.Reader, limit int) io.Reader {
	r := &eagerBuf{limit: limit}
	r.cond = sync.NewCond(&r.mu)
	go r.fill(src)
	return r
}

type eagerBuf struct {
	mu    sync.Mutex
	cond  *sync.Cond
	buf   []byte
	err   error // set once src ends
	limit int
}

func (r *eagerBuf) fill(src io.Reader) {
	chunk := make([]byte, 64<<10)
	for {
		n, err := src.Read(chunk)
		r.mu.Lock()
		for n > 0 && len(r.buf) >= r.limit && r.err == nil {
			r.cond.Wait() // full: backpressure, as without the buffer
		}
		r.buf = append(r.buf, chunk[:n]...)
		if err != nil {
			r.err = err
		}
		r.cond.Broadcast()
		r.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (r *eagerBuf) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.buf) == 0 && r.err == nil {
		r.cond.Wait()
	}
	if len(r.buf) == 0 {
		return 0, r.err
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	if len(r.buf) == 0 {
		r.buf = nil // let a drained burst's memory go
	}
	r.cond.Broadcast()
	return n, nil
}
