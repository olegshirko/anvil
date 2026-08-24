package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
)

// dockerImageSummary matches the JSON returned by GET /images/json.
type dockerImageSummary struct {
	Id          string            `json:"Id"`
	RepoTags    []string          `json:"RepoTags"`
	RepoDigests []string          `json:"RepoDigests"`
	Created     int64             `json:"Created"`
	Size        int64             `json:"Size"`
	VirtualSize int64             `json:"VirtualSize"`
	Labels      map[string]string `json:"Labels"`
	ParentId    string            `json:"ParentId"`
	Containers  int               `json:"Containers"`
}

// canonicalizeImageRef returns a fully-qualified containerd image reference.
// Unqualified library images (e.g. "postgres:15.5") become
// "docker.io/library/postgres:15.5", and user images (e.g. "foo/bar") become
// "docker.io/foo/bar". A registry domain is present only when the first path
// component looks like a host (contains "." or ":", or is localhost) — a bare
// "name:tag" has no slash and must NOT be mistaken for a registry. When
// neither tag nor digest is present, ":latest" is appended (Docker
// semantics; the check looks at the last path component so a registry port
// is not treated as a tag).
func canonicalizeImageRef(ref string) string {
	if ref == "" {
		return ref
	}
	parts := strings.SplitN(ref, "/", 2)
	result := ref
	if len(parts) == 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		result = ref
	} else if len(parts) == 1 {
		result = "docker.io/library/" + ref
	} else {
		result = "docker.io/" + ref
	}
	last := result
	if i := strings.LastIndex(result, "/"); i != -1 {
		last = result[i+1:]
	}
	if !strings.ContainsAny(last, ":@") {
		result += ":latest"
	}
	return result
}

// dockerMirrorRepo hosts Docker images mirrored as image-arm64.tar.zst on
// GitHub releases, keyed by tag <safe-name>-<tag>-arm64. Used as a fallback
// when the upstream registry is unreachable.
const dockerMirrorRepo = "olegshirko/docker-mirror"

// errMirrorNotFound signals that the image has not been mirrored: a clean 404
// from the release-asset URL. Callers fall through to the original pull error
// instead of reporting a noisy download failure.
var errMirrorNotFound = errors.New("image not found in docker-mirror")

// splitImageTag splits a user image ref into name and tag. Digest refs
// ("repo@sha256:...") are not mirrorable (ok=false). A ref without an explicit
// tag gets ":latest", matching Docker semantics and the mirror release naming.
// A ":" before the last "/" is a registry port, not a tag.
func splitImageTag(ref string) (name, tag string, ok bool) {
	if strings.Contains(ref, "@") {
		return "", "", false
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon == -1 || colon < slash {
		return ref, "latest", true
	}
	return ref[:colon], ref[colon+1:], true
}

// loadFromMirror downloads an image archive from the docker-mirror GitHub
// release and imports it into containerd. The release tag is derived from the
// ref (<safe-name>-<tag>-arm64, where safe-name replaces "/" with "-"), so no
// GitHub API call is made and the unauthenticated release-asset download never
// hits API rate limits. Returns errMirrorNotFound when the image has not been
// mirrored, so callers can distinguish "mirror it first" from a real failure.
func loadFromMirror(ctx context.Context, ref, ns string) error {
	name, tag, ok := splitImageTag(ref)
	if !ok {
		return errMirrorNotFound
	}
	safeName := strings.ReplaceAll(name, "/", "-")
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s-%s-arm64/image-arm64.tar.zst",
		dockerMirrorRepo, safeName, tag)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("mirror download: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errMirrorNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("mirror download %s: %s", url, resp.Status)
	}

	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}

	nsCtx := namespaces.WithNamespace(ctx, ns)
	ds, err := compression.DecompressStream(resp.Body)
	if err != nil {
		return fmt.Errorf("decompress: %w", err)
	}
	defer ds.Close()

	log.Printf("[images] loading %q from docker-mirror (%s)", ref, url)
	imgs, err := cl.Import(nsCtx, ds,
		client.WithDigestRef(func(d digest.Digest) string {
			return "docker.io/imported/anvil-image:" + d.Encoded()[:12]
		}),
		client.WithSkipDigestRef(func(name string) bool { return name != "" }),
		client.WithSkipMissing(),
	)
	if err != nil {
		return fmt.Errorf("import: %s", stripANSI(err.Error()))
	}
	for _, img := range imgs {
		_ = client.NewImage(cl, img).Unpack(nsCtx, "native")
		if canonical := canonicalizeImageRef(img.Name); canonical != img.Name {
			if err := putImage(cl, nsCtx, images.Image{Name: canonical, Target: img.Target, Labels: img.Labels}); err != nil {
				log.Printf("[images] mirror canonical alias %s -> %s: %v", img.Name, canonical, err)
			}
		}
	}
	log.Printf("[images] loaded %q from docker-mirror into ns=%s", ref, ns)
	return nil
}

// ensureImageInNamespace makes sure an image reference exists in the target
// namespace. If the image already exists there, it returns nil. If it exists
// in another namespace, the image is streamed into the target namespace
// (containerd's content store is namespaced, so a bare metadata copy would
// leave a dangling pointer pointing at blobs the target namespace cannot see).
// Otherwise it is pulled into the target namespace.
func ensureImageInNamespace(ctx context.Context, ref, targetNs string) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}

	canonicalRef := canonicalizeImageRef(ref)

	// Fast path: image already exists in target namespace AND its content
	// is actually there (records left by interrupted copies or partial GCs
	// would otherwise pass the check and fail the unpack later). The whole
	// descriptor tree must be visible — a manifest-only check passes while
	// the layers have been collected.
	targetCtx := namespaces.WithNamespace(ctx, targetNs)
	if img, err := cl.GetImage(targetCtx, canonicalRef); err == nil {
		if missing := imageTreeMissing(cl, targetCtx, img.Target()); len(missing) == 0 {
			log.Printf("[images] %s already in namespace %s", canonicalRef, targetNs)
			return nil
		}
		debugLog("image %s in namespace %s is dangling, refreshing", canonicalRef, targetNs)
		deleteImageTree(cl, targetCtx, canonicalRef, img.Target())
	}

	// Images imported from OCI archives may be registered under the raw,
	// unnormalized ref.name (e.g. "myapp:1" instead of
	// "docker.io/library/myapp:1"). If the target namespace has the image
	// under that raw name, alias it to the canonical one (same namespace, so
	// the content store already sees the blobs).
	if ref != canonicalRef {
		if img, err := cl.GetImage(targetCtx, ref); err == nil {
			if err := putImage(cl, targetCtx, images.Image{Name: canonicalRef, Target: img.Target(), Labels: img.Labels()}); err != nil {
				return fmt.Errorf("alias image %s to %s: %w", ref, canonicalRef, err)
			}
			log.Printf("[images] aliased %s to %s in namespace %s", ref, canonicalRef, targetNs)
			return nil
		}
	}

	// Find the image in another namespace and stream it into the target one.
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	candidates := []string{canonicalRef}
	if ref != canonicalRef {
		candidates = append(candidates, ref)
		// Raw form with an explicit tag, registered verbatim by buildkit's
		// image exporter (see findImageNamespace).
		if !strings.Contains(ref, "@") {
			if _, tag, ok := strings.Cut(ref, ":"); !ok || tag == "" {
				candidates = append(candidates, ref+":latest")
			}
		}
	}
	cstore := cl.ContentStore()
	for _, ns := range nss {
		if ns == targetNs {
			continue
		}
		nsCtx := namespaces.WithNamespace(ctx, ns)
		for _, name := range candidates {
			img, err := cl.GetImage(nsCtx, name)
			if err != nil {
				continue
			}
			// Skip dangling records: the image name exists but its content
			// has been GC'd in that namespace (stale records left by old
			// projects). Copying from there would fail mid-tree.
			if _, cerr := cstore.Info(nsCtx, img.Target().Digest); cerr != nil {
				debugLog("image %s in namespace %s is dangling (no content for %s), skipping", name, ns, img.Target().Digest)
				continue
			}
			copyErr := copyImageBetweenNamespaces(ns, name, targetNs)
			if copyErr != nil {
				// The record looked complete but some blobs are missing —
				// either a partially GC'd namespace or a buildkitd export
				// that had not fully landed yet. Give it a moment and try
				// once more before moving to the next namespace.
				time.Sleep(500 * time.Millisecond)
				copyErr = copyImageBetweenNamespaces(ns, name, targetNs)
			}
			if copyErr != nil {
				debugLog("copy image %s from %s failed, trying next namespace: %v", name, ns, copyErr)
				continue
			}
			log.Printf("[images] streamed %s from %s to %s", name, ns, targetNs)
			return nil
		}
	}

	// Image not found anywhere; pull it natively through containerd's remote
	// resolver (public registries; auth comes later if ever needed). The
	// VZ NAT path to Docker Hub sporadically resets connections, so retry
	// briefly before falling through to the mirror.
	log.Printf("[images] pulling %q (canonical %q) into namespace %s", ref, canonicalRef, targetNs)
	var perr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
		perr = pullImageIntoNamespace(ctx, canonicalRef, targetNs)
		if perr == nil {
			break
		}
		debugLog("pull attempt %d for %s failed: %v", attempt+1, canonicalRef, perr)
	}
	if perr != nil {
		pullErr := fmt.Errorf("pull failed: %w", perr)
		// Fallback: docker-mirror GitHub release (used when the registry is
		// unreachable or rate-limited but a mirror exists). Only silent on a
		// clean 404 ("not mirrored yet"); other download errors are logged.
		if mErr := loadFromMirror(ctx, ref, targetNs); mErr == nil {
			if _, err := cl.GetImage(targetCtx, canonicalRef); err == nil {
				return nil
			}
		} else if !errors.Is(mErr, errMirrorNotFound) {
			log.Printf("[images] mirror fallback for %q: %v", ref, mErr)
		}
		return pullErr
	}
	log.Printf("[images] pulled %q into namespace %s", ref, targetNs)
	return nil
}

// pullImageIntoNamespace pulls an image into the given namespace's image and
// content stores, unpacking its rootfs so containers can start immediately.
func pullImageIntoNamespace(ctx context.Context, canonicalRef, ns string) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	// A leftover manifest blob from an interrupted copy or a partial GC
	// makes the fetcher skip re-ingesting it — and with it, skip setting
	// the gc.ref labels that pin the freshly fetched children. After the
	// pull lease is released, the next GC pass then collects the layers
	// while the record stays (dangling). Drop record AND visible tree
	// blobs first so the pull re-ingests everything with full labels.
	if img, gerr := cl.GetImage(nsCtx, canonicalRef); gerr == nil {
		if missing := imageTreeMissing(cl, nsCtx, img.Target()); len(missing) > 0 {
			debugLog("pull %s: record in ns %s dangling (%d blobs missing), dropping before pull",
				canonicalRef, ns, len(missing))
			deleteImageTree(cl, nsCtx, canonicalRef, img.Target())
		}
	}
	// Verify the tree after each attempt and re-pull (bounded): a long GC
	// pass started by an earlier synchronous rmi can snapshot its roots
	// before this pull commits and sweep the fresh blobs afterwards — by
	// the next attempt that pass has finished. NOTE: no outer lease here —
	// wrapping Pull in our own lease makes the committed image record
	// lease-scoped, and releasing it deletes the record outright.
	for attempt := 1; ; attempt++ {
		img, perr := cl.Pull(nsCtx, canonicalRef, client.WithPullUnpack)
		if perr != nil {
			return perr
		}
		// Pin the tree explicitly: the fetch path does not reliably set the
		// containerd.io/gc.ref.content.* labels on pre-existing parents,
		// so the first GC pass after the pull's internal lease is released
		// collects the children while the record stays (dangling).
		labelImageTree(cl.ContentStore(), nsCtx, img.Target())
		missing := imageTreeMissing(cl, nsCtx, img.Target())
		if len(missing) == 0 {
			return nil
		}
		if attempt >= 5 {
			return fmt.Errorf("pull %s into %s: %d tree blobs missing after %d attempts",
				canonicalRef, ns, len(missing), attempt)
		}
		debugLog("pull %s: %d blobs missing after attempt %d, re-pulling", canonicalRef, len(missing), attempt)
		deleteImageTree(cl, nsCtx, canonicalRef, img.Target())
		time.Sleep(750 * time.Millisecond)
	}
}

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
		if visible > 0 && complete == 0 && len(missing) == 0 {
			// Every visible manifest is broken but reported nothing?
			// Defensive: treat as missing.
			missing = append(missing, target.Digest.String())
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

// pullImageAllPlatforms pulls every platform of a (possibly multi-arch)
// image so the full index can be exported by docker save.
func pullImageAllPlatforms(ctx context.Context, canonicalRef, ns string) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	leaseCtx, release, lerr := cl.WithLease(nsCtx)
	if lerr == nil {
		if leaseID, ok := leases.FromContext(leaseCtx); ok {
			nsCtx = leases.WithLease(nsCtx, leaseID)
		}
		defer release(context.Background())
	}
	_, err = cl.Pull(nsCtx, canonicalRef, client.WithPlatformMatcher(platforms.All))
	return err
}

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
			// copyTree may claim "already in destination" from stale
			// scratch-namespace leftovers; only keep the child if it is
			// genuinely visible now, otherwise the export fails on it.
			if _, ierr := bc.cs.Info(bc.dstCtx, child.Digest); ierr != nil {
				debugLog("save: manifest %s copied but not visible, skipping", child.Digest)
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

// listDockerImages collects images from all namespaces and returns a
// Docker-compatible summary, deduplicating by image ID and tag.
func listDockerImages(ctx context.Context) ([]dockerImageSummary, error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}

	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	seen := make(map[string]struct{})
	result := make([]dockerImageSummary, 0)

	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		imgs, lerr := cl.ListImages(nsCtx)
		if lerr != nil {
			log.Printf("[docker-api] list images in %s: %v", ns, lerr)
			continue
		}
		for _, img := range imgs {
			target := img.Target()
			id := string(target.Digest)
			if id == "" {
				continue
			}
			name := img.Name()
			repo, tag := splitRepoTag(name)
			// Dedup by (ID, tag): the same image legitimately appears under
			// several names (`docker tag alpine x` must stay visible), but
			// namespace fan-out would otherwise list each name many times.
			dedupKey := id + "|" + repo + ":" + tag
			if _, ok := seen[dedupKey]; ok {
				continue
			}
			seen[dedupKey] = struct{}{}

			size := int64(0)
			if uerr := func() error {
				s, gerr := img.Usage(nsCtx)
				if gerr == nil {
					size = s
				}
				return gerr
			}(); uerr != nil {
				debugLog("[images] usage for %s: %v", name, uerr)
			}

			created := int64(0)
			var labels map[string]string
			if spec, serr := img.Spec(nsCtx); serr == nil {
				if !spec.Created.IsZero() {
					created = spec.Created.Unix()
				}
				labels = spec.Config.Labels
			}
			if labels == nil {
				labels = map[string]string{}
			}

			var repoTags []string
			if tag != "" && tag != "none" {
				repoTags = []string{repo + ":" + tag}
			}
			var repoDigests []string
			if target.Digest != "" && repo != "" {
				repoDigests = []string{repo + "@" + target.Digest.String()}
			}

			result = append(result, dockerImageSummary{
				Id:          id,
				RepoTags:    repoTags,
				RepoDigests: repoDigests,
				Created:     created,
				Size:        size,
				VirtualSize: size,
				Labels:      labels,
				ParentId:    "",
				Containers:  -1,
			})
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Id < result[j].Id
	})
	return result, nil
}

// splitRepoTag splits an image name into repository and tag parts. Digest
// refs keep the whole name as the repository (tag empty).
func splitRepoTag(name string) (repo, tag string) {
	if i := strings.Index(name, "@"); i != -1 {
		return name[:i], ""
	}
	i := strings.LastIndex(name, ":")
	if i == -1 || strings.Contains(name[i+1:], "/") {
		return name, ""
	}
	return name[:i], name[i+1:]
}

// findImageNamespace returns the containerd namespace that contains an image
// matching the given reference (name or name:tag). The empty string is returned
// if no matching image is found.
func findImageNamespace(ctx context.Context, ref string) string {
	cl, err := pc.get(ctx)
	if err != nil {
		return ""
	}

	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		log.Printf("[images] list namespaces: %v", err)
		return ""
	}
	log.Printf("[images] findImageNamespace ref=%q namespaces=%v", ref, nss)

	// Strict matching: the requested ref canonicalized to a fully-qualified
	// name (which appends ":latest" for tagless refs and keeps registry
	// ports/digests intact), plus the raw ref for images imported from OCI
	// archives under an unnormalized name. A tagless ref must NOT match an
	// image with a different tag: a namespace holding only "nginx:alpine"
	// does not satisfy a request for "nginx" (= nginx:latest), and returning
	// it would make the subsequent image lookup in that namespace fail
	// with "no such image".
	canonical := canonicalizeImageRef(ref)
	// buildkit's image exporter registers the name attribute verbatim, so a
	// locally built "myapp" is stored as "myapp:latest" without the
	// docker.io/library prefix — match that raw-with-tag form too.
	variants := []string{canonical, ref}
	if canonical != ref && !strings.Contains(ref, "@") {
		if _, tag, ok := strings.Cut(ref, ":"); !ok || tag == "" {
			variants = append(variants, ref+":latest")
		}
	}
	cstore := cl.ContentStore()
	for _, ns := range nss {
		if ns == saveScratchNs {
			continue // internal staging namespace
		}
		nsCtx := namespaces.WithNamespace(ctx, ns)
		images, err := cl.ListImages(nsCtx)
		if err != nil {
			debugLog("list images in %s: %v", ns, err)
			continue
		}
		for _, img := range images {
			name := img.Name()
			matched := false
			for _, v := range variants {
				if name == v {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			// Skip dangling records: the name exists but its target blob
			// has been garbage-collected in that namespace.
			if _, cerr := cstore.Info(nsCtx, img.Target().Digest); cerr != nil {
				debugLog("image %s in namespace %s is dangling, skipping", name, ns)
				continue
			}
			return ns
		}
	}
	debugLog("findImageNamespace: no namespace for %s", ref)
	return ""
}

// tagDockerImage creates a new image record pointing at the same content —
// containerd tags are just additional names for a target descriptor.
func tagDockerImage(ctx context.Context, source, target string) error {
	// Prefer a namespace where the source's WHOLE tree is visible: records
	// left dangling by partial GCs must not seed a dangling tag.
	ns := ""
	for _, cand := range findAllImageNamespaces(ctx, source) {
		candCtx := namespaces.WithNamespace(ctx, cand)
		cl, err := pc.get(ctx)
		if err != nil {
			return fmt.Errorf("containerd client: %w", err)
		}
		if img, gerr := cl.GetImage(candCtx, canonicalizeImageRef(source)); gerr == nil {
			if len(imageTreeMissing(cl, candCtx, img.Target())) == 0 {
				ns = cand
				break
			}
		}
	}
	if ns == "" {
		// Every record is dangling (a GC pass raced earlier pulls). Re-pull
		// the SOURCE into the default namespace — the retrying pull path
		// re-ingests a fully labelled tree — and tag from there.
		if perr := pullImageIntoNamespace(ctx, canonicalizeImageRef(source), "default"); perr != nil {
			debugLog("tag %s: healing pull: %v", source, perr)
		}
		ns = findImageNamespace(ctx, source)
	}
	if ns == "" {
		ns = "default"
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	srcImg, gerr := cl.GetImage(nsCtx, canonicalizeImageRef(source))
	if gerr != nil {
		srcImg, gerr = cl.GetImage(nsCtx, source)
	}
	if gerr != nil {
		return fmt.Errorf("tag: %w", gerr)
	}
	canonicalTarget := canonicalizeImageRef(target)
	for _, name := range []string{canonicalTarget, target} {
		if name == "" {
			continue
		}
		if err := putImage(cl, nsCtx, images.Image{Name: name, Target: srcImg.Target(), Labels: srcImg.Labels()}); err != nil {
			return fmt.Errorf("tag %s: %w", name, err)
		}
	}
	// Tags must be globally visible: mirror the new tag into the default
	// namespace so multi-image operations (save a b) resolve every name in
	// one namespace.
	if ns != "default" {
		if err := ensureImageInNamespace(ctx, target, "default"); err != nil {
			log.Printf("[images] mirror tag %s to default: %v", target, err)
		}
	}
	return nil
}

// handleImageGet implements GET /images/{name}/get (docker save) — streams a
// Docker-format tar of the image into the response.
func handleImageGet(w http.ResponseWriter, r *http.Request, name string) {
	streamImageSave(r.Context(), w, []string{name})
}

// handleImagesGet implements GET /images/get?names=a&names=b — same, for
// multiple images in one archive.
func handleImagesGet(w http.ResponseWriter, r *http.Request) {
	names := r.URL.Query()["names"]
	if len(names) == 0 {
		http.Error(w, `{"message":"no images specified"}`, http.StatusBadRequest)
		return
	}
	streamImageSave(r.Context(), w, names)
}

// saveMu serializes docker save: all saves share one scratch namespace,
// and a concurrent save's cleanup would gut another's staging.
var saveMu sync.Mutex

func streamImageSave(ctx context.Context, w http.ResponseWriter, names []string) {
	saveMu.Lock()
	defer saveMu.Unlock()
	// Verify all images exist first so a missing one is a clean 404 instead
	// of a failed stream mid-response.
	ns := "default"
	var imgs []images.Image
	for _, name := range names {
		imgNs := findImageNamespace(ctx, name)
		if imgNs == "" {
			http.Error(w, fmt.Sprintf(`{"message":"No such image: %s"}`, name), http.StatusNotFound)
			return
		}
		ns = imgNs
	}
	cl, err := pc.get(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	log.Printf("[docker-api] saving images %v from ns=%q", names, ns)
	// Export needs every blob in ONE namespace, but records and blobs are
	// frequently spread across namespaces. Stage the images into a scratch
	// namespace with the cross-namespace blob copier, export from there and
	// drop the namespace afterwards.
	scratchNs := saveScratchNs
	dropScratch := func() {
		// Remove the image records first so the namespace delete is not
		// blocked by leftover references; blob bytes are dropped with it.
		sctx := namespaces.WithNamespace(ctx, scratchNs)
		if imgs, lerr := cl.ListImages(sctx); lerr == nil {
			for _, im := range imgs {
				_ = cl.ImageService().Delete(sctx, im.Name())
			}
		}
		_ = cl.NamespaceService().Delete(ctx, scratchNs) //nolint:errcheck
	}
	dropScratch() // stale leftovers from a previous run
	scratchCtx := namespaces.WithNamespace(ctx, scratchNs)
	bc := newBlobCopier(ctx, cl, ns, scratchNs)
	for _, name := range names {
		canonical := canonicalizeImageRef(name)
		srcCtx := namespaces.WithNamespace(ctx, ns)
		var src images.Image
		for _, candidate := range []string{canonical, name} {
			if g, gerr := cl.GetImage(srcCtx, candidate); gerr == nil {
				src = images.Image{Name: g.Name(), Target: g.Target(), Labels: g.Labels()}
				break
			}
		}
		if src.Name == "" {
			dropScratch()
			http.Error(w, fmt.Sprintf(`{"message":"No such image: %s"}`, name), http.StatusNotFound)
			return
		}
		target, serr := stageTreeForExport(bc, src.Target)
		if serr != nil {
			dropScratch()
			http.Error(w, fmt.Sprintf(`{"message":"save %s: %s"}`, name, serr.Error()), http.StatusInternalServerError)
			return
		}
		if perr := putImage(cl, scratchCtx, images.Image{Name: canonical, Target: target}); perr != nil {
			dropScratch()
			http.Error(w, fmt.Sprintf(`{"message":"save %s: %s"}`, name, perr.Error()), http.StatusInternalServerError)
			return
		}
		imgs = append(imgs, images.Image{Name: canonical, Target: target})
	}
	if len(imgs) == 0 {
		dropScratch()
		http.Error(w, `{"message":"save failed: no image records"}`, http.StatusInternalServerError)
		return
	}
	defer dropScratch()

	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	if err := cl.Export(scratchCtx, w, archive.WithImages(imgs), archive.WithAllPlatforms()); err != nil {
		// Headers are already sent; the client sees the truncated stream.
		log.Printf("[docker-api] save %v failed: %v", names, stripANSI(err.Error()))
	}
}

// removeDockerImage removes every image record matching the ref (canonical or
// raw name) across namespaces. Docker semantics are "the image is gone";
// success if at least one namespace removed a record.
func removeDockerImage(ctx context.Context, name string, force bool) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	canonical := canonicalizeImageRef(name)
	nss := findAllImageNamespaces(ctx, name)
	if len(nss) == 0 {
		nss = []string{"default"}
	}
	removed, lastErr := 0, error(nil)
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		deletedOne := false
		for _, candidate := range dedupeStrings([]string{canonical, name}) {
			dopts := []images.DeleteOpt{}
			if force {
				dopts = append(dopts, images.SynchronousDelete())
			}
			derr := cl.ImageService().Delete(nsCtx, candidate, dopts...)
			if derr == nil || errdefs.IsNotFound(derr) {
				if derr == nil {
					deletedOne = true
				}
				continue
			}
			// Image in use by a container unless forced; report like docker.
			lastErr = fmt.Errorf("rmi %s: %w", candidate, derr)
		}
		if deletedOne {
			log.Printf("[images] rmi %q ns=%q force=%v", name, ns, force)
			removed++
		}
	}
	if removed == 0 && lastErr != nil {
		return lastErr
	}
	return nil
}

// dedupeStrings preserves order while dropping duplicates.
func dedupeStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// findAllImageNamespaces returns every namespace whose image store holds the
// ref (strict canonical/raw name match, like findImageNamespace).
func findAllImageNamespaces(ctx context.Context, ref string) []string {
	cl, err := pc.get(ctx)
	if err != nil {
		return nil
	}

	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		log.Printf("[images] list namespaces: %v", err)
		return nil
	}
	canonical := canonicalizeImageRef(ref)
	var found []string
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		images, err := cl.ListImages(nsCtx)
		if err != nil {
			continue
		}
		for _, img := range images {
			if n := img.Name(); n == canonical || n == ref {
				found = append(found, ns)
				break
			}
		}
	}
	return found
}

// pruneDockerImages removes images and returns Docker's /images/prune
// response shape. With dangling=true only untagged images go; with
// dangling=false (`docker system prune -a`) every image not referenced by a
// container goes.
func pruneDockerImages(ctx context.Context, dangling bool) ([]map[string]string, int64, error) {
	images, err := listDockerImages(ctx)
	if err != nil {
		return nil, 0, err
	}

	used := map[string]bool{}
	if containers, err := listDockerContainers(ctx, nil); err == nil {
		for _, c := range containers {
			if c.Image != "" {
				used[c.Image] = true
			}
			if c.ImageID != "" {
				used[c.ImageID] = true
			}
		}
	}

	deleted := []map[string]string{}
	var reclaimed int64
	for _, img := range images {
		tag := ""
		if len(img.RepoTags) > 0 {
			tag = img.RepoTags[0]
		}
		isDangling := tag == "" || strings.HasPrefix(tag, "<none>")
		if dangling && !isDangling {
			continue
		}
		if !dangling && used[tag] {
			continue
		}
		ref := tag
		if isDangling {
			ref = img.Id
		}
		if err := removeDockerImage(ctx, ref, true); err != nil {
			log.Printf("[docker-api] prune image %s: %v", ref, err)
			continue
		}
		deleted = append(deleted, map[string]string{"Deleted": img.Id})
		reclaimed += img.Size
	}
	return deleted, reclaimed, nil
}

// inspectDockerImage returns a Docker-compatible image inspect payload,
// searching all namespaces for the image.
func inspectDockerImage(ctx context.Context, name string) (map[string]interface{}, error) {
	ns := findImageNamespace(ctx, name)
	if ns == "" {
		ns = "default"
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	img, gerr := cl.GetImage(nsCtx, canonicalizeImageRef(name))
	if gerr != nil {
		img, gerr = cl.GetImage(nsCtx, name)
	}
	if gerr != nil {
		return nil, fmt.Errorf("No such image: %s", name)
	}
	spec, serr := img.Spec(nsCtx)
	if serr != nil {
		return nil, fmt.Errorf("image spec: %w", serr)
	}

	repo, tag := splitRepoTag(img.Name())
	var repoTags []string
	if tag != "" {
		repoTags = []string{repo + ":" + tag}
	}
	target := img.Target()
	var repoDigests []string
	if target.Digest != "" {
		repoDigests = []string{repo + "@" + target.Digest.String()}
	}

	size := int64(0)
	if u, uerr := img.Usage(nsCtx); uerr == nil {
		size = u
	}

	layers := make([]string, 0, len(spec.RootFS.DiffIDs))
	for _, d := range spec.RootFS.DiffIDs {
		layers = append(layers, d.String())
	}

	config := map[string]interface{}{
		"Cmd":        spec.Config.Cmd,
		"Entrypoint": spec.Config.Entrypoint,
		"Env":        spec.Config.Env,
		"Labels":     spec.Config.Labels,
		"WorkingDir": spec.Config.WorkingDir,
		"User":       spec.Config.User,
	}
	if len(spec.Config.ExposedPorts) > 0 {
		exposed := map[string]interface{}{}
		for p := range spec.Config.ExposedPorts {
			exposed[p] = struct{}{}
		}
		config["ExposedPorts"] = exposed
	}
	if spec.Config.StopSignal != "" {
		config["StopSignal"] = spec.Config.StopSignal
	}

	created := ""
	if spec.Created != nil && !spec.Created.IsZero() {
		created = spec.Created.Format(time.RFC3339Nano)
	}
	comment := ""
	if n := len(spec.History); n > 0 {
		comment = spec.History[n-1].Comment
	}

	return map[string]interface{}{
		"Id":          target.Digest.String(),
		"RepoTags":    repoTags,
		"RepoDigests": repoDigests,
		"Comment":     comment,
		"Created":     created,
		"Author":      spec.Author,
		"Config":      config,
		"RootFS": map[string]interface{}{
			"Type":   "layers",
			"Layers": layers,
		},
		"Size":        size,
		"VirtualSize": size,
		"GraphDriver": map[string]interface{}{"Data": map[string]interface{}{}, "Name": "overlayfs"},
	}, nil
}

// pushDockerImage pushes an image to its registry through containerd's
// remote resolver, streaming minimal Docker-style status lines to w.
func pushDockerImage(ctx context.Context, name string, w io.Writer) error {
	ns := findImageNamespace(ctx, name)
	if ns == "" {
		ns = "default"
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	img, gerr := cl.GetImage(nsCtx, canonicalizeImageRef(name))
	if gerr != nil {
		img, gerr = cl.GetImage(nsCtx, name)
	}
	if gerr != nil {
		return fmt.Errorf("No such image: %s", name)
	}
	fmt.Fprintf(w, `{"status":"Pushing %s"}
`, name)
	if perr := cl.Push(nsCtx, canonicalizeImageRef(name), img.Target()); perr != nil {
		fmt.Fprintf(w, `{"errorDetail":{"message":%q}}
`, stripANSI(perr.Error()))
		return perr
	}
	fmt.Fprintf(w, `{"status":"Pushed %s"}
`, name)
	return nil
}

// pullDockerImage pulls an image natively into the default namespace. When
// the registry pull fails it falls back to the docker-mirror GitHub release
// and returns a status line describing which path produced the image.
func pullDockerImage(ctx context.Context, image string) (string, error) {
	ns := "default"
	if err := pullImageIntoNamespace(ctx, canonicalizeImageRef(image), ns); err == nil {
		return fmt.Sprintf("Downloaded newer image for %s", image), nil
	} else {
		pullErr := err
		if mErr := loadFromMirror(ctx, image, ns); mErr == nil {
			return fmt.Sprintf("Loaded image: %s (from docker-mirror)", image), nil
		} else if !errors.Is(mErr, errMirrorNotFound) {
			log.Printf("[images] mirror fallback for %q: %v", image, mErr)
		}
		return "", fmt.Errorf("pull failed: %w", pullErr)
	}
}

// handleImageLoad implements POST /images/load — streams a Docker/OCI tar
// archive from the request body straight into containerd via the Go client.
// No temp file is used: the guest root is a small RAM tmpfs, so buffering
// large archives on disk can fill it up. gzip/zstd streams are auto-detected.
func handleImageLoad(w http.ResponseWriter, r *http.Request) {
	ns := "default"
	if r.URL.Query().Get("namespace") != "" {
		ns = r.URL.Query().Get("namespace")
	}

	cl, err := pc.get(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	log.Printf("[docker-api] loading image stream (%d bytes) into ns=%q", r.ContentLength, ns)

	ds, err := compression.DecompressStream(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer ds.Close()

	// Archives without name annotations (e.g. `buildx --output type=oci`
	// without -t) import content only and register no image reference, so
	// `docker run <name>` would fall back to a registry pull. Give unnamed
	// manifests a digest-based ref so they can be run/tagged locally.
	nsCtx := namespaces.WithNamespace(r.Context(), ns)
	imgs, err := cl.Import(nsCtx, ds,
		client.WithDigestRef(func(d digest.Digest) string {
			return "docker.io/imported/anvil-image:" + d.Encoded()[:12]
		}),
		client.WithSkipDigestRef(func(name string) bool { return name != "" }),
		// Archives exported for a single platform may still reference
		// manifests/blobs of other platforms (e.g. a multi-arch index) that
		// are not included. Skip those instead of failing the whole import;
		// ctr's transfer-based import is lenient the same way.
		client.WithSkipMissing(),
	)
	if err != nil {
		log.Printf("[docker-api] import failed: %v", err)
		http.Error(w, fmt.Sprintf(`{"message":"import failed: %s"}`, stripANSI(err.Error())), http.StatusInternalServerError)
		return
	}

	// Import into the content store without unpacking (avoids whiteout
	// conversion errors with the native snapshotter). Unpack best-effort so
	// images are immediately usable; otherwise the first container start
	// unpacks via containerd's snapshotter.
	for _, img := range imgs {
		_ = client.NewImage(cl, img).Unpack(nsCtx, "native")
		// Archives may carry raw, unnormalized names (e.g. "myapp:1"), but
		// nerdctl canonicalizes short names (docker.io/library/myapp:1) on
		// inspect/run, so a raw-only record is invisible to it and clients
		// fall back to a registry pull. Register the canonical alias too.
		if canonical := canonicalizeImageRef(img.Name); canonical != img.Name {
			if err := putImage(cl, nsCtx, images.Image{Name: canonical, Target: img.Target, Labels: img.Labels}); err != nil {
				log.Printf("[docker-api] canonical alias %s -> %s failed: %v", img.Name, canonical, err)
			}
		}
	}

	// Docker CLI prints every "status" line from the response stream. Report
	// the exact refs that were registered so the user knows which name to run.
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if len(imgs) == 0 {
		enc.Encode(map[string]interface{}{"status": "Loaded image"})
		return
	}
	for _, img := range imgs {
		enc.Encode(map[string]interface{}{"status": fmt.Sprintf("Loaded image: %s", img.Name)})
	}
}
