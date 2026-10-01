package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// pullProgress streams Docker's pull progress messages: "Pulling fs
// layer", "Downloading" with progressDetail (what draws the CLI's bars),
// "Pull complete". Without it `docker pull` sat silent until the end, and
// clients with response timeouts gave up on large images.
type pullProgress struct {
	mu     sync.Mutex
	enc    *json.Encoder
	flush  func()
	layers []string // short IDs in fetch order
	seen   map[string]bool
}

func newPullProgress(w io.Writer) *pullProgress {
	p := &pullProgress{enc: json.NewEncoder(w), seen: map[string]bool{}, flush: func() {}}
	if f, ok := w.(http.Flusher); ok {
		p.flush = f.Flush
	}
	return p
}

// send writes one message; nil-safe so callers need no checks.
func (p *pullProgress) send(msg map[string]any) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enc.Encode(msg) //nolint:errcheck — the client going away ends the pull through ctx
	p.flush()
}

func (p *pullProgress) status(s string) {
	p.send(map[string]any{"status": s})
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

func isLayerType(mt string) bool {
	return strings.Contains(mt, "layer") || strings.Contains(mt, "rootfs")
}

// handler announces each layer as the fetcher reaches it.
func (p *pullProgress) handler() images.Handler {
	return images.HandlerFunc(func(_ context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if !isLayerType(desc.MediaType) {
			return nil, nil
		}
		id := shortDigest(desc.Digest.String())
		p.mu.Lock()
		fresh := !p.seen[id]
		if fresh {
			p.seen[id] = true
			p.layers = append(p.layers, id)
		}
		p.mu.Unlock()
		if fresh {
			p.send(map[string]any{"status": "Pulling fs layer", "id": id, "progressDetail": map[string]any{}})
		}
		return nil, nil
	})
}

// poll reports active layer downloads from the content store until stop.
func (p *pullProgress) poll(ctx context.Context, cs content.Store, stop <-chan struct{}) {
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
		}
		statuses, err := cs.ListStatuses(ctx)
		if err != nil {
			continue
		}
		for _, st := range statuses {
			if !strings.HasPrefix(st.Ref, "layer-") || st.Total <= 0 {
				continue
			}
			id := shortDigest(strings.TrimPrefix(st.Ref, "layer-"))
			if st.Expected != "" {
				id = shortDigest(st.Expected.String())
			}
			p.send(map[string]any{
				"status":         "Downloading",
				"id":             id,
				"progressDetail": map[string]any{"current": st.Offset, "total": st.Total},
			})
		}
	}
}

// complete marks every announced layer done.
func (p *pullProgress) complete() {
	if p == nil {
		return
	}
	p.mu.Lock()
	layers := append([]string(nil), p.layers...)
	p.mu.Unlock()
	for _, id := range layers {
		p.send(map[string]any{"status": "Pull complete", "id": id, "progressDetail": map[string]any{}})
	}
}

type pullProgressKey struct{}

func withPullProgress(ctx context.Context, p *pullProgress) context.Context {
	return context.WithValue(ctx, pullProgressKey{}, p)
}

func pullProgressFrom(ctx context.Context) *pullProgress {
	p, _ := ctx.Value(pullProgressKey{}).(*pullProgress)
	return p
}
