package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	digest "github.com/opencontainers/go-digest"
)

// Getting images into a namespace: registry pull (with auth), the
// docker-mirror fallback, and push.

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
func ensureImageInNamespace(ctx context.Context, ref, targetNs, platform string, auth *registryAuth) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}

	canonicalRef := canonicalizeImageRef(ref)

	// An explicit non-native platform (--platform linux/amd64): the record
	// may exist with only the arm64 subtree, so check that platform's
	// content and pull it when missing.
	if platform != "" {
		targetCtx := namespaces.WithNamespace(ctx, targetNs)
		if img, err := cl.GetImage(targetCtx, canonicalRef); err == nil &&
			platformContentPresent(targetCtx, cl.ContentStore(), img.Target(), platform) {
			return nil
		}
		log.Printf("[images] pulling %q for %s into namespace %s", canonicalRef, platform, targetNs)
		if err := pullImageIntoNamespace(ctx, canonicalRef, targetNs, platform, auth); err != nil {
			return fmt.Errorf("pull failed: %w", explainEgressFailure(err))
		}
		return nil
	}

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
		perr = pullImageIntoNamespace(ctx, canonicalRef, targetNs, "", auth)
		if perr == nil {
			break
		}
		debugLog("pull attempt %d for %s failed: %v", attempt+1, canonicalRef, perr)
	}
	if perr != nil {
		pullErr := fmt.Errorf("pull failed: %w", explainEgressFailure(perr))
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
func pullImageIntoNamespace(ctx context.Context, canonicalRef, ns, platform string, auth *registryAuth) error {
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
	opts := append([]client.RemoteOpt{client.WithPullUnpack}, authResolverOpts(auth)...)
	if platform != "" {
		opts = append(opts, client.WithPlatformMatcher(platformMatcher(platform)))
	}
	for attempt := 1; ; attempt++ {
		img, perr := cl.Pull(nsCtx, canonicalRef, opts...)
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

// pushDockerImage pushes an image to its registry through containerd's
// remote resolver, streaming minimal Docker-style status lines to w.
func pushDockerImage(ctx context.Context, name string, auth *registryAuth, w io.Writer) error {
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
	if perr := cl.Push(nsCtx, canonicalizeImageRef(name), img.Target(), authResolverOpts(auth)...); perr != nil {
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
func pullDockerImage(ctx context.Context, image, platform string, auth *registryAuth) (string, error) {
	ns := "default"
	if err := pullImageIntoNamespace(ctx, canonicalizeImageRef(image), ns, platform, auth); err == nil {
		return fmt.Sprintf("Downloaded newer image for %s", image), nil
	} else {
		pullErr := err
		if mErr := loadFromMirror(ctx, image, ns); mErr == nil {
			return fmt.Sprintf("Loaded image: %s (from docker-mirror)", image), nil
		} else if !errors.Is(mErr, errMirrorNotFound) {
			log.Printf("[images] mirror fallback for %q: %v", image, mErr)
		}
		return "", fmt.Errorf("pull failed: %w", explainEgressFailure(pullErr))
	}
}
