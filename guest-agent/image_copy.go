package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
)

// Content-store plumbing: walking, labelling and deleting image trees, and
// copying images between containerd namespaces (blobCopier), which also
// stages trees for export.

// imageTreeMissing returns descriptors of the image tree that are not
// visible in nsCtx (empty = the record is healthy). For a multi-arch index
// only the subtree of every VISIBLE platform manifest is required — a
// platform pull (the default) fetches just one subtree — but at least one
// manifest must be complete.
func imageTreeMissing(cl *client.Client, nsCtx context.Context, target ocispec.Descriptor) []string {
	cs := cl.ContentStore()
	var missing []string
	walkManifest := func(d ocispec.Descriptor) {
		if _, err := cs.Info(nsCtx, d.Digest); err != nil {
			missing = append(missing, d.Digest.String())
			return
		}
		data, err := readBlobAll(nsCtx, cs, d)
		if err != nil {
			missing = append(missing, d.Digest.String())
			return
		}
		var man ocispec.Manifest
		if json.Unmarshal(data, &man) != nil {
			missing = append(missing, d.Digest.String())
			return
		}
		for _, kid := range append([]ocispec.Descriptor{man.Config}, man.Layers...) {
			if _, err := cs.Info(nsCtx, kid.Digest); err != nil {
				missing = append(missing, kid.Digest.String())
			}
		}
	}
	switch target.MediaType {
	case ocispec.MediaTypeImageIndex, images.MediaTypeDockerSchema2ManifestList:
		data, err := readBlobAll(nsCtx, cs, target)
		if err != nil {
			return []string{target.Digest.String()}
		}
		var idx ocispec.Index
		if json.Unmarshal(data, &idx) != nil {
			return []string{target.Digest.String()}
		}
		complete, visible := 0, 0
		for _, child := range idx.Manifests {
			if _, err := cs.Info(nsCtx, child.Digest); err != nil {
				continue // platform subtree not fetched — fine
			}
			visible++
			before := len(missing)
			walkManifest(child)
			if len(missing) == before {
				complete++
			}
		}
		if complete == 0 {
			// An index whose platform subtrees are all absent (or broken)
			// is not usable: nothing can be unpacked or exported from it.
			for _, child := range idx.Manifests {
				missing = append(missing, child.Digest.String())
			}
		}
	default:
		walkManifest(target)
	}
	return missing
}

// gcRefLabel is the containerd label key prefix that pins a child blob to
// its parent in the content store (the GC walks these edges).
func gcRefLabel(d digest.Digest) string {
	return "containerd.io/gc.ref.content." + d.String()
}

// labelImageTree walks the descriptor tree and stamps gc.ref.content labels
// on every parent, making the tree explicitly reachable from the top blob
// regardless of whether the fetcher labelled it.
func labelImageTree(cs content.Store, ctx context.Context, target ocispec.Descriptor) {
	label := func(parent ocispec.Descriptor, children ...ocispec.Descriptor) {
		info, err := cs.Info(ctx, parent.Digest)
		if err != nil {
			return
		}
		changed := false
		for _, c := range children {
			key := gcRefLabel(c.Digest)
			if info.Labels == nil {
				info.Labels = map[string]string{}
			}
			if _, ok := info.Labels[key]; !ok {
				info.Labels[key] = c.Digest.String()
				changed = true
			}
		}
		if changed {
			if _, uerr := cs.Update(ctx, info, "labels"); uerr != nil {
				debugLog("labelImageTree: update %s: %v", parent.Digest, uerr)
			}
		}
	}
	var visit func(d ocispec.Descriptor)
	visit = func(d ocispec.Descriptor) {
		switch d.MediaType {
		case ocispec.MediaTypeImageIndex, images.MediaTypeDockerSchema2ManifestList:
			if data, rerr := readBlobAll(ctx, cs, d); rerr == nil {
				var idx ocispec.Index
				if json.Unmarshal(data, &idx) == nil {
					label(d, idx.Manifests...)
					for _, child := range idx.Manifests {
						visit(child)
					}
				}
			}
		case ocispec.MediaTypeImageManifest, images.MediaTypeDockerSchema2Manifest:
			if data, rerr := readBlobAll(ctx, cs, d); rerr == nil {
				var man ocispec.Manifest
				if json.Unmarshal(data, &man) == nil {
					kids := append([]ocispec.Descriptor{man.Config}, man.Layers...)
					label(d, kids...)
				}
			}
		}
	}
	visit(target)
}

// deleteImageTree removes the image record plus every tree blob that is
// still visible in the namespace, so a re-pull ingests from scratch.
func deleteImageTree(cl *client.Client, nsCtx context.Context, name string, target ocispec.Descriptor) {
	if derr := cl.ImageService().Delete(nsCtx, name); derr != nil {
		debugLog("deleteImageTree: record %s: %v", name, derr)
	}
	cs := cl.ContentStore()
	var walk func(d ocispec.Descriptor)
	walk = func(d ocispec.Descriptor) {
		info, err := cs.Info(nsCtx, d.Digest)
		if err != nil {
			return
		}
		switch d.MediaType {
		case ocispec.MediaTypeImageIndex, images.MediaTypeDockerSchema2ManifestList:
			if data, rerr := readBlobAll(nsCtx, cs, d); rerr == nil {
				var idx ocispec.Index
				if json.Unmarshal(data, &idx) == nil {
					for _, child := range idx.Manifests {
						walk(child)
					}
				}
			}
		case ocispec.MediaTypeImageManifest, images.MediaTypeDockerSchema2Manifest:
			if data, rerr := readBlobAll(nsCtx, cs, d); rerr == nil {
				var man ocispec.Manifest
				if json.Unmarshal(data, &man) == nil {
					walk(man.Config)
					for _, layer := range man.Layers {
						walk(layer)
					}
				}
			}
		}
		if derr := cs.Delete(nsCtx, info.Digest); derr != nil {
			debugLog("deleteImageTree: blob %s: %v", d.Digest, derr)
		}
	}
	walk(target)
}

// saveScratchNs stages images for docker save so every blob sits in a
// single namespace before the archive export.
const saveScratchNs = "anvil-save-tmp"

// putImage creates or updates an image record.
func putImage(cl *client.Client, nsCtx context.Context, img images.Image) error {
	if _, err := cl.ImageService().Create(nsCtx, img); err != nil {
		if _, uerr := cl.ImageService().Update(nsCtx, img, "target"); uerr != nil {
			return err
		}
	}
	return nil
}

// blobCopier copies image trees between containerd namespaces, resolving
// each blob in any namespace (image records and their blobs often live in
// different ones after projects are pruned and content is GC'd).
type blobCopier struct {
	cs      content.Store
	cl      *client.Client
	srcCtxs []context.Context
	dstCtx  context.Context
}

func newBlobCopier(ctx context.Context, cl *client.Client, srcNs, dstNs string) *blobCopier {
	cs := cl.ContentStore()
	srcCtxs := []context.Context{namespaces.WithNamespace(ctx, srcNs)}
	if nss, lerr := cl.NamespaceService().List(ctx); lerr == nil {
		for _, ns := range nss {
			if ns != srcNs && ns != dstNs {
				srcCtxs = append(srcCtxs, namespaces.WithNamespace(ctx, ns))
			}
		}
	}
	return &blobCopier{
		cs:      cs,
		cl:      cl,
		srcCtxs: srcCtxs,
		dstCtx:  namespaces.WithNamespace(ctx, dstNs),
	}
}

func (bc *blobCopier) blobCtx(d ocispec.Descriptor) context.Context {
	for _, c := range bc.srcCtxs {
		if _, ierr := bc.cs.Info(c, d.Digest); ierr == nil {
			return c
		}
	}
	return bc.srcCtxs[0]
}

// copyTree copies d and everything below it into the destination namespace.
// Parent blobs are committed with containerd gc.ref labels pointing at their
// children — without them the garbage collector eventually deletes the child
// blobs while the image record survives, and every later unpack fails with
// "content digest ... not found".
func (bc *blobCopier) copyTree(d ocispec.Descriptor) error {
	if _, ierr := bc.cs.Info(bc.dstCtx, d.Digest); ierr == nil {
		debugLog("copyTree: %s already in destination", d.Digest)
		return nil // blob already visible in the destination namespace
	}
	bctx := bc.blobCtx(d)
	gcLabels := map[string]string{}
	addRef := func(child ocispec.Descriptor) {
		gcLabels[fmt.Sprintf("containerd.io/gc.ref.content.%s", child.Digest.Encoded())] = child.Digest.String()
	}
	switch d.MediaType {
	case ocispec.MediaTypeImageIndex, images.MediaTypeDockerSchema2ManifestList:
		var idx ocispec.Index
		if data, rerr := readBlobAll(bctx, bc.cs, d); rerr == nil {
			if jerr := json.Unmarshal(data, &idx); jerr == nil {
				for _, child := range idx.Manifests {
					if cerr := bc.copyTree(child); cerr != nil {
						return cerr
					}
					addRef(child)
				}
			}
		}
	case ocispec.MediaTypeImageManifest, images.MediaTypeDockerSchema2Manifest:
		var man ocispec.Manifest
		if data, rerr := readBlobAll(bctx, bc.cs, d); rerr == nil {
			if jerr := json.Unmarshal(data, &man); jerr == nil {
				if cerr := bc.copyTree(man.Config); cerr != nil {
					return cerr
				}
				addRef(man.Config)
				for _, layer := range man.Layers {
					if cerr := bc.copyTree(layer); cerr != nil {
						return cerr
					}
					addRef(layer)
				}
			}
		}
	}
	if len(gcLabels) == 0 {
		gcLabels = nil
	}
	return bc.copyBlob(bctx, d, gcLabels)
}

func (bc *blobCopier) copyBlob(bctx context.Context, d ocispec.Descriptor, gcLabels map[string]string) error {
	w, err := bc.cs.Writer(bc.dstCtx, content.WithDescriptor(d), content.WithRef("anvil-copy-"+d.Digest.String()))
	if err != nil {
		return err
	}
	defer w.Close()
	// A writer handed an existing ingest ref resumes at its last offset;
	// always restart from zero so the copy is exactly the descriptor bytes.
	if err := w.Truncate(0); err != nil {
		return fmt.Errorf("truncate ingest: %w", err)
	}
	ra, err := bc.cs.ReaderAt(bctx, d)
	if err != nil {
		return err
	}
	defer ra.Close()
	debugLog("copyBlob: copying %s (%d bytes)", d.Digest, d.Size)
	if ra.Size() != d.Size {
		debugLog("copyBlob size mismatch for %s: blob=%d descriptor=%d", d.Digest, ra.Size(), d.Size)
	}
	if _, err := io.Copy(w, io.NewSectionReader(ra, 0, ra.Size())); err != nil {
		return err
	}
	var commitOpts []content.Opt
	if gcLabels != nil {
		commitOpts = append(commitOpts, content.WithLabels(gcLabels))
	}
	if cerr := w.Commit(bc.dstCtx, d.Size, d.Digest, commitOpts...); cerr != nil {
		if !errdefs.IsAlreadyExists(cerr) {
			return cerr
		}
		debugLog("copyBlob: %s commit: already exists", d.Digest)
	}
	if _, ierr := bc.cs.Info(bc.dstCtx, d.Digest); ierr != nil {
		debugLog("copyBlob: %s NOT VISIBLE right after commit: %v", d.Digest, ierr)
	}
	return nil
}

// writeBlob ingests raw bytes as a content blob and returns its descriptor.
func (bc *blobCopier) writeBlob(data []byte, mediaType string, gcLabels map[string]string) (ocispec.Descriptor, error) {
	d := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}
	if _, ierr := bc.cs.Info(bc.dstCtx, d.Digest); ierr == nil {
		return d, nil
	}
	w, err := bc.cs.Writer(bc.dstCtx, content.WithDescriptor(d), content.WithRef("anvil-write-"+d.Digest.String()))
	if err != nil {
		return d, err
	}
	defer w.Close()
	if err := w.Truncate(0); err != nil {
		return d, err
	}
	if _, err := w.Write(data); err != nil {
		return d, err
	}
	var commitOpts []content.Opt
	if gcLabels != nil {
		commitOpts = append(commitOpts, content.WithLabels(gcLabels))
	}
	return d, w.Commit(bc.dstCtx, d.Size, d.Digest, commitOpts...)
}

// stageTreeForExport copies an image tree for docker save. Multi-arch
// indexes frequently have blobs missing locally (only the native platform
// was ever pulled), so incomplete child manifests are dropped and the index
// is rebuilt from the complete ones instead of failing the whole export.
func stageTreeForExport(bc *blobCopier, target ocispec.Descriptor) (ocispec.Descriptor, error) {
	switch target.MediaType {
	case ocispec.MediaTypeImageIndex, images.MediaTypeDockerSchema2ManifestList:
		var idx ocispec.Index
		data, rerr := readBlobAll(bc.blobCtx(target), bc.cs, target)
		if rerr != nil {
			return target, fmt.Errorf("read index: %w", rerr)
		}
		if jerr := json.Unmarshal(data, &idx); jerr != nil {
			return target, fmt.Errorf("parse index: %w", jerr)
		}
		debugLog("save: index %s mediaType=%s manifests=%d", target.Digest, target.MediaType, len(idx.Manifests))
		kept := make([]ocispec.Descriptor, 0, len(idx.Manifests))
		for _, child := range idx.Manifests {
			if cerr := bc.copyTree(child); cerr != nil {
				debugLog("save: skipping incomplete manifest %s: %v", child.Digest, cerr)
				continue
			}
			// Keep a platform only if its whole subtree is in the
			// destination: copyTree can report success for a manifest whose
			// config or layers are gone (a partially pruned platform, e.g.
			// after rmi of an image sharing the manifest), and the export
			// then dies mid-stream on the missing blob.
			if missing := manifestTreeMissing(bc.dstCtx, bc.cs, child); len(missing) > 0 {
				debugLog("save: manifest %s incomplete in staging (%d missing), skipping", child.Digest, len(missing))
				continue
			}
			kept = append(kept, child)
		}
		if len(kept) == 0 {
			return target, fmt.Errorf("no complete manifests in index %s", target.Digest)
		}
		if len(kept) == len(idx.Manifests) {
			// Children are copied above; the index blob itself still needs
			// to land in the destination namespace for the export.
			if cerr := bc.copyBlob(bc.blobCtx(target), target, nil); cerr != nil {
				return target, cerr
			}
			return target, nil
		}
		newIdx := idx
		newIdx.Manifests = kept
		data, merr := json.Marshal(newIdx)
		if merr != nil {
			return target, merr
		}
		gcLabels := make(map[string]string, len(kept))
		for _, child := range kept {
			gcLabels[fmt.Sprintf("containerd.io/gc.ref.content.%s", child.Digest.Encoded())] = child.Digest.String()
		}
		return bc.writeBlob(data, ocispec.MediaTypeImageIndex, gcLabels)
	default:
		if cerr := bc.copyTree(target); cerr != nil {
			return target, cerr
		}
		return target, nil
	}
}

// manifestTreeMissing lists the blobs of a single-platform manifest (the
// manifest itself, its config and layers) not visible in ctx's namespace.
func manifestTreeMissing(ctx context.Context, cs content.Store, d ocispec.Descriptor) []string {
	data, err := readBlobAll(ctx, cs, d)
	if err != nil {
		return []string{d.Digest.String()}
	}
	var man ocispec.Manifest
	if json.Unmarshal(data, &man) != nil {
		return []string{d.Digest.String()}
	}
	var missing []string
	for _, kid := range append([]ocispec.Descriptor{man.Config}, man.Layers...) {
		if _, err := cs.Info(ctx, kid.Digest); err != nil {
			missing = append(missing, kid.Digest.String())
		}
	}
	return missing
}

// copyImageBetweenNamespaces copies an image from one containerd namespace
// to another. The legacy Export/Import round-trip cannot handle index
// (multi-arch) images, so the descriptor tree is walked and every blob is
// copied through the content store instead.
func copyImageBetweenNamespaces(srcNs, ref, targetNs string) error {
	ctx := context.Background()
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	srcCtx := namespaces.WithNamespace(ctx, srcNs)
	canonical := canonicalizeImageRef(ref)
	var img images.Image
	found := false
	for _, name := range []string{canonical, ref} {
		if g, gerr := cl.GetImage(srcCtx, name); gerr == nil {
			img = images.Image{Name: name, Target: g.Target(), Labels: g.Labels()}
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("image %s not found in namespace %s", ref, srcNs)
	}
	// The image record (the GC root for the tree) is created only AFTER the
	// blobs are copied; a concurrent GC pass triggered by an unrelated image
	// delete would sweep the fresh, still-unreferenced blobs. A lease pins
	// them until the record is in place.
	// NOTE: the lease must live in the TARGET namespace — a lease created
	// in a namespace-less (default) context does not pin blobs written to
	// another namespace.
	targetCtx := namespaces.WithNamespace(ctx, targetNs)
	leaseCtx, release, lerr := cl.WithLease(targetCtx)
	if lerr == nil {
		if leaseID, ok := leases.FromContext(leaseCtx); ok {
			ctx = leases.WithLease(ctx, leaseID)
		}
		defer release(context.Background()) //nolint:errcheck
	}
	bc := newBlobCopier(ctx, cl, srcNs, targetNs)
	if err := bc.copyTree(img.Target); err != nil {
		return err
	}
	if missing := bc.verifyTree(img.Target); len(missing) > 0 {
		// A concurrent copy using the same ingest refs can truncate our
		// in-flight writes; re-copy the missing blobs once.
		debugLog("copyImage: %d blobs missing after copy into %s, retrying", len(missing), targetNs)
		if err := bc.copyTree(img.Target); err != nil {
			return fmt.Errorf("retry after gaps: %w", err)
		}
		if missing := bc.verifyTree(img.Target); len(missing) > 0 {
			for _, dgst := range missing {
				bc.logDigestNamespaces(dgst)
			}
			return fmt.Errorf("blobs still missing after copy: %v", missing)
		}
	}
	if err := putImage(cl, bc.dstCtx, images.Image{Name: canonical, Target: img.Target, Labels: img.Labels}); err != nil {
		return fmt.Errorf("put image: %w", err)
	}
	return nil
}

// logDigestNamespaces reports which namespaces actually hold the digest —
// forensic help for the copy race where a committed blob is not visible in
// the destination namespace.
func (bc *blobCopier) logDigestNamespaces(dgst string) {
	dd := digest.Digest(dgst)
	var found []string
	for _, c := range bc.srcCtxs {
		if c == bc.dstCtx {
			continue
		}
		if _, ierr := bc.cs.Info(c, dd); ierr == nil {
			ns, _ := namespaces.Namespace(c)
			found = append(found, ns)
		}
	}
	dstNs, _ := namespaces.Namespace(bc.dstCtx)
	debugLog("digest %s: present in %v, missing in %q", dgst, found, dstNs)
}

// verifyTree walks the descriptor tree and returns digests whose blobs are
// not visible in the destination namespace.
func (bc *blobCopier) verifyTree(d ocispec.Descriptor) []string {
	var missing []string
	var walk func(d ocispec.Descriptor)
	walk = func(d ocispec.Descriptor) {
		if _, ierr := bc.cs.Info(bc.dstCtx, d.Digest); ierr != nil {
			missing = append(missing, d.Digest.String())
			return
		}
		switch d.MediaType {
		case ocispec.MediaTypeImageIndex, images.MediaTypeDockerSchema2ManifestList:
			if data, rerr := readBlobAll(bc.dstCtx, bc.cs, d); rerr == nil {
				var idx ocispec.Index
				if json.Unmarshal(data, &idx) == nil {
					for _, child := range idx.Manifests {
						walk(child)
					}
				}
			}
		case ocispec.MediaTypeImageManifest, images.MediaTypeDockerSchema2Manifest:
			if data, rerr := readBlobAll(bc.dstCtx, bc.cs, d); rerr == nil {
				var man ocispec.Manifest
				if json.Unmarshal(data, &man) == nil {
					walk(man.Config)
					for _, layer := range man.Layers {
						walk(layer)
					}
				}
			}
		}
	}
	walk(d)
	return missing
}

func readBlobAll(ctx context.Context, cs content.Store, d ocispec.Descriptor) ([]byte, error) {
	ra, err := cs.ReaderAt(ctx, d)
	if err != nil {
		return nil, err
	}
	defer ra.Close()
	buf := make([]byte, ra.Size())
	if _, err := io.ReadFull(io.NewSectionReader(ra, 0, ra.Size()), buf); err != nil {
		return nil, err
	}
	return buf, nil
}
