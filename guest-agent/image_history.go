package main

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
)

// `docker history`: the image config's history entries joined with the
// layers they produced, newest first.

type dockerImageHistoryItem struct {
	Id        string   `json:"Id"`
	Created   int64    `json:"Created"`
	CreatedBy string   `json:"CreatedBy"`
	Tags      []string `json:"Tags"`
	Size      int64    `json:"Size"`
	Comment   string   `json:"Comment"`
}

// lookupDockerImage finds an image by user reference across namespaces and
// returns it with a context bound to its namespace.
func lookupDockerImage(ctx context.Context, name string) (client.Image, context.Context, error) {
	ns := findImageNamespace(ctx, name)
	if ns == "" {
		ns = "default"
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	img, gerr := cl.GetImage(nsCtx, canonicalizeImageRef(name))
	if gerr != nil {
		img, gerr = cl.GetImage(nsCtx, name)
	}
	if gerr != nil {
		return nil, nil, fmt.Errorf("No such image: %s", name)
	}
	return img, nsCtx, nil
}

func imageHistory(ctx context.Context, name string) ([]dockerImageHistoryItem, error) {
	img, nsCtx, err := lookupDockerImage(ctx, name)
	if err != nil {
		return nil, err
	}
	spec, err := img.Spec(nsCtx)
	if err != nil {
		return nil, fmt.Errorf("image spec: %w", err)
	}
	sizes := imageLayerSizes(nsCtx, img, spec.RootFS.DiffIDs)

	var tags []string
	if repo, tag := splitRepoTag(img.Name()); tag != "" {
		tags = []string{repo + ":" + tag}
	}

	var items []dockerImageHistoryItem
	layer := 0
	for _, h := range spec.History {
		it := dockerImageHistoryItem{Id: "<missing>", CreatedBy: h.CreatedBy, Comment: h.Comment}
		if h.Created != nil {
			it.Created = h.Created.Unix()
		}
		if !h.EmptyLayer && layer < len(sizes) {
			it.Size = sizes[layer]
			layer++
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		// No history in the config (some hand-built images): one row per layer.
		for _, s := range sizes {
			it := dockerImageHistoryItem{Id: "<missing>", Size: s}
			if spec.Created != nil {
				it.Created = spec.Created.Unix()
			}
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		items = []dockerImageHistoryItem{{}}
	}
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	items[0].Id = img.Target().Digest.String()
	items[0].Tags = tags
	return items, nil
}

// imageLayerSizes returns each layer's size, preferring the unpacked
// snapshot (what Docker reports) and falling back to the compressed blob
// from the manifest when the image has not been unpacked.
func imageLayerSizes(nsCtx context.Context, img client.Image, diffIDs []digest.Digest) []int64 {
	sizes := make([]int64, len(diffIDs))
	var manifestSizes []int64
	if m, err := images.Manifest(nsCtx, img.ContentStore(), img.Target(), img.Platform()); err == nil {
		for _, l := range m.Layers {
			manifestSizes = append(manifestSizes, l.Size)
		}
	}
	cl, err := pc.get(nsCtx)
	var chain []digest.Digest
	if err == nil {
		chain = identity.ChainIDs(append([]digest.Digest(nil), diffIDs...))
	}
	for i := range diffIDs {
		if chain != nil {
			if u, uerr := cl.SnapshotService(defaults.DefaultSnapshotter).Usage(nsCtx, chain[i].String()); uerr == nil {
				sizes[i] = u.Size
				continue
			}
		}
		if i < len(manifestSizes) {
			sizes[i] = manifestSizes[i]
		}
	}
	return sizes
}
