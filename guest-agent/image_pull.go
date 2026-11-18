package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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

// mirrorHTTPClient adds a header timeout and stall-bounded connections to
// the mirror download: the body streams for as long as the transfer takes
// (archives are large), but an endpoint that accepts the connection and then
// goes silent must not hang the pull. mirrorTransport clones
// http.DefaultTransport, so ProxyFromEnvironment survives.
var mirrorHTTPClient = &http.Client{
	Transport: mirrorTransport(),
}

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
	tag = ref[colon+1:]
	if tag == "" {
		// A dangling colon ("alpine:") has no tag: the mirror release
		// naming would produce a garbage "<name>--arm64" tag.
		return "", "", false
	}
	return ref[:colon], tag, true
}

// loadFromMirrorPlatform downloads an image archive from the docker-mirror
// GitHub release and imports it into containerd. The release tag is derived
// from the ref (<safe-name>-<tag>-<platform>, where safe-name replaces "/"
// with "-"), so no GitHub API call is made and the unauthenticated
// release-asset download never hits API rate limits. Returns
// errMirrorNotFound when the image has not been mirrored, so callers can
// distinguish "mirror it first" from a real failure.
//
// platform: "" / linux/arm64 uses the native -arm64 release, linux/amd64 the
// -amd64 one (naming per mirrorReleaseAsset).
func loadFromMirrorPlatform(ctx context.Context, ref, ns, platform string) error {
	release, asset, err := mirrorReleaseAsset(ref, platform)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s",
		dockerMirrorRepo, release, asset)

	// Bound the download by the caller's context (the pull request dies
	// with its client) and a header timeout: a GitHub endpoint that never
	// answers used to hang the pull forever inside http.Get.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("mirror download: %w", err)
	}
	resp, err := mirrorHTTPClient.Do(req)
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
		// An unpack failure must fail the load: an imported-but-unpacked
		// image has no snapshots and every later `docker run` of it dies
		// with "no unpacked rootfs".
		if err := client.NewImage(cl, img).Unpack(nsCtx, "native"); err != nil {
			return fmt.Errorf("unpack %s: %w", img.Name, err)
		}
		if canonical := canonicalizeImageRef(img.Name); canonical != img.Name {
			if err := putImage(cl, nsCtx, images.Image{Name: canonical, Target: img.Target, Labels: img.Labels}); err != nil {
				log.Printf("[images] mirror canonical alias %s -> %s: %v", img.Name, canonical, err)
			}
		}
	}
	log.Printf("[images] loaded %q from docker-mirror into ns=%s", ref, ns)
	return nil
}

// findImageLocally makes ref available in targetNs from what the VM already
// holds — the target namespace itself, a raw-name alias, or another
// namespace (streamed across). It never pulls. The returned digest is the
// image's target digest when found.
func findImageLocally(ctx context.Context, ref, targetNs string) (bool, digest.Digest, error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return false, "", fmt.Errorf("containerd client: %w", err)
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
			return true, img.Target().Digest, nil
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
				return false, "", fmt.Errorf("alias image %s to %s: %w", ref, canonicalRef, err)
			}
			log.Printf("[images] aliased %s to %s in namespace %s", ref, canonicalRef, targetNs)
			return true, img.Target().Digest, nil
		}
	}

	// Find the image in another namespace and stream it into the target one.
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return false, "", fmt.Errorf("list namespaces: %w", err)
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
			return true, img.Target().Digest, nil
		}
	}
	return false, "", nil
}

// ensureImageInNamespace makes sure an image reference exists in the target
// namespace, locally if possible (findImageLocally), pulling otherwise. With
// an explicit platform the local record must also hold that platform's
// content — a multi-arch record may carry only another platform's subtree.
func ensureImageInNamespace(ctx context.Context, ref, targetNs, platform string, auth *registryAuth) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	canonicalRef := canonicalizeImageRef(ref)
	targetCtx := namespaces.WithNamespace(ctx, targetNs)

	// Memo fast path: a previous create verified this (namespace, ref,
	// platform) and the record still points at the same digest. One record
	// read replaces the descriptor-tree walk findImageLocally performs; the
	// digest comparison keeps a stale entry (rmi, a tag move, a pull) from
	// ever skipping a check that is due.
	if img, gerr := cl.GetImage(targetCtx, canonicalRef); gerr == nil &&
		imageMemo.isPresent(targetNs, canonicalRef, platform, img.Target().Digest) {
		return nil
	}

	found, foundDigest, err := findImageLocally(ctx, ref, targetNs)
	if err != nil {
		return err
	}
	if found && platform == "" {
		imageMemo.markPresent(targetNs, canonicalRef, "", foundDigest)
		return nil
	}
	if found {
		for _, name := range []string{canonicalRef, ref} {
			if img, gerr := cl.GetImage(targetCtx, name); gerr == nil &&
				platformContentPresent(targetCtx, cl.ContentStore(), img.Target(), platform) {
				imageMemo.markPresent(targetNs, canonicalRef, platform, img.Target().Digest)
				return nil
			}
		}
	}

	// Image not found anywhere; pull it natively through containerd's remote
	// resolver (public registries; auth comes later if ever needed). The
	// VZ NAT path to Docker Hub sporadically resets connections, so retry
	// briefly before falling through to the mirror. The pull rewrites the
	// record and may add another platform's content under the same target
	// digest, so no memo entry about this image survives it.
	imageMemo.forgetRef(targetNs, canonicalRef)
	if img, gerr := cl.GetImage(targetCtx, canonicalRef); gerr == nil {
		imageMemo.forgetImage(img.Target().Digest)
	}
	log.Printf("[images] pulling %q (canonical %q) into namespace %s", ref, canonicalRef, targetNs)
	mirrorUsed, pullErr, mirrorErr := pullWithMirrorFallback(ctx, ref, canonicalRef, targetNs, platform, auth)
	if pullErr != nil && mirrorErr != nil {
		return &pullFallbackError{ref: ref, registry: pullErr, mirror: mirrorErr}
	}
	if mirrorUsed {
		// The load imports under the canonical name; verify it landed before
		// reporting success (a partial import must not look like a pull).
		if img, err := cl.GetImage(targetCtx, canonicalRef); err == nil {
			imageMemo.forgetRef(targetNs, canonicalRef)
			imageMemo.forgetImage(img.Target().Digest)
			log.Printf("[images] %q loaded from docker-mirror after the registry failed: %v", ref, pullErr)
			return nil
		}
		return fmt.Errorf("mirror load of %s reported success but the image is not in namespace %s", ref, targetNs)
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
	baseOpts := append([]client.RemoteOpt{client.WithPullUnpack}, authResolverOpts(auth)...)
	if platform != "" {
		baseOpts = append(baseOpts, client.WithPlatformMatcher(platformMatcher(platform)))
	}
	prog := pullProgressFrom(ctx)
	phaseTimeout := pullPhaseTimeout()
	for attempt := 1; ; attempt++ {
		var stopPoll func()
		if prog != nil {
			stop, done := make(chan struct{}), make(chan struct{})
			go func() { defer close(done); prog.poll(nsCtx, cl.ContentStore(), stop) }()
			stopPoll = func() { close(stop); <-done }
		}
		// Each attempt is bounded: the resolve/manifest phase by a timer that
		// is stopped as soon as the first layer descriptor arrives, and every
		// registry read after that by the connection's stall timeout. A
		// registry that accepts the connection and then goes quiet therefore
		// fails the attempt instead of keeping the pull alive indefinitely.
		attemptCtx, cancelAttempt := context.WithCancel(nsCtx)
		var phaseExpired atomic.Bool
		phaseTimer := time.AfterFunc(phaseTimeout, func() {
			phaseExpired.Store(true)
			cancelAttempt()
		})
		handlers := []images.Handler{stopOnFirstLayer(func() { phaseTimer.Stop() })}
		if prog != nil {
			handlers = append(handlers, prog.handler())
		}
		opts := append([]client.RemoteOpt(nil), baseOpts...)
		opts = append(opts, client.WithImageHandler(images.Handlers(handlers...)))
		img, perr := cl.Pull(attemptCtx, canonicalRef, opts...)
		phaseTimer.Stop()
		cancelAttempt()
		if stopPoll != nil {
			stopPoll() // nothing may write to the response after we return
		}
		if perr != nil {
			switch {
			case phaseExpired.Load():
				return fmt.Errorf("%w: no image content within %s", errPullPhaseTimeout, phaseTimeout)
			case isStallTimeout(perr):
				return fmt.Errorf("%w: no data for %s", errPullStall, pullStallTimeout())
			}
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
func pushDockerImage(ctx context.Context, name string, auth *registryAuth, w io.Writer) (err error) {
	defer func() {
		if pw, ok := w.(*pushErrorWriter); ok && err != nil && !pw.reported {
			fmt.Fprintf(w, `{"errorDetail":{"message":%q},"error":%q}
`, err.Error(), err.Error())
		}
	}()
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
		err := fmt.Errorf("No such image: %s", name)
		fmt.Fprintf(w, `{"errorDetail":{"message":%q},"error":%q}
`, err.Error(), err.Error())
		return err
	}
	fmt.Fprintf(w, `{"status":"Pushing %s"}
`, name)
	target := pushableTarget(nsCtx, cl.ContentStore(), img.Target())
	if perr := cl.Push(nsCtx, canonicalizeImageRef(name), target, authResolverOpts(auth)...); perr != nil {
		msg := stripANSI(perr.Error())
		fmt.Fprintf(w, `{"errorDetail":{"message":%q},"error":%q}
`, msg, msg)
		return perr
	}
	fmt.Fprintf(w, `{"status":"Pushed %s"}
`, name)
	return nil
}

// pullDockerImage pulls an image natively into the default namespace. When
// the registry pull fails (including after its bounded deadline) it falls
// back to the docker-mirror GitHub release and returns a status line
// describing which path produced the image.
func pullDockerImage(ctx context.Context, image, platform string, auth *registryAuth) (string, error) {
	ns := "default"
	before := localImageDigest(ctx, ns, image)
	mirrorUsed, pullErr, mirrorErr := pullWithMirrorFallback(ctx, image, canonicalizeImageRef(image), ns, platform, auth)
	if pullErr != nil && mirrorErr != nil {
		return "", &pullFallbackError{ref: image, registry: pullErr, mirror: mirrorErr}
	}
	if mirrorUsed {
		// The wording clients check for success (docker-java) stays the
		// prefix; the parenthesis says where the image actually came from.
		log.Printf("[images] %s loaded from docker-mirror after the registry failed: %v", image, pullErr)
		return fmt.Sprintf("Downloaded newer image for %s (%s; loaded from docker-mirror)",
			image, registryFailureSummary(pullErr)), nil
	}
	if before != "" && before == localImageDigest(ctx, ns, image) {
		return fmt.Sprintf("Image is up to date for %s", image), nil
	}
	return fmt.Sprintf("Downloaded newer image for %s", image), nil
}

// pushableTarget is what `docker push` sends: the image's index when every
// manifest it lists is present locally, otherwise (a pull fetches only this
// machine's platform of a multi-platform image) the local platform's
// manifest — pushing the full index failed with "content digest … not
// found" for every tagged copy of a public image.
func pushableTarget(ctx context.Context, cs content.Store, target ocispec.Descriptor) ocispec.Descriptor {
	if !images.IsIndexType(target.MediaType) {
		return target
	}
	data, err := content.ReadBlob(ctx, cs, target)
	if err != nil {
		return target
	}
	var idx ocispec.Index
	if json.Unmarshal(data, &idx) != nil {
		return target
	}
	complete := true
	for _, m := range idx.Manifests {
		if _, err := cs.Info(ctx, m.Digest); err != nil {
			complete = false
			break
		}
	}
	if complete {
		return target
	}
	matcher := defaultPlatformMatcher()
	for _, m := range idx.Manifests {
		if m.Platform == nil || !matcher.Match(*m.Platform) {
			continue
		}
		if _, err := cs.Info(ctx, m.Digest); err == nil {
			return m
		}
	}
	return target
}

// localImageDigest is the digest image has in ns, or "".
func localImageDigest(ctx context.Context, ns, image string) string {
	cl, err := pc.get(ctx)
	if err != nil {
		return ""
	}
	img, err := cl.GetImage(namespaces.WithNamespace(ctx, ns), canonicalizeImageRef(image))
	if err != nil {
		return ""
	}
	return img.Target().Digest.String()
}
