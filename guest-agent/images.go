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
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

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
// leave a dangling pointer and nerdctl would fall back to a registry pull).
// Otherwise it is pulled into the target namespace.
func ensureImageInNamespace(ctx context.Context, ref, targetNs string) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}

	canonicalRef := canonicalizeImageRef(ref)

	// Fast path: image already exists in target namespace.
	targetCtx := namespaces.WithNamespace(ctx, targetNs)
	if _, err := cl.GetImage(targetCtx, canonicalRef); err == nil {
		log.Printf("[images] %s already in namespace %s", canonicalRef, targetNs)
		return nil
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
			if err := copyImageBetweenNamespaces(ns, name, targetNs); err != nil {
				// The record looked complete but some blobs are missing
				// (partially GC'd namespace); try the next namespace.
				debugLog("copy image %s from %s failed, trying next namespace: %v", name, ns, err)
				continue
			}
			log.Printf("[images] streamed %s from %s to %s", name, ns, targetNs)
			return nil
		}
	}

	// Image not found anywhere; pull it natively through containerd's remote
	// resolver (public registries; auth comes later if ever needed).
	log.Printf("[images] pulling %q (canonical %q) into namespace %s", ref, canonicalRef, targetNs)
	if perr := pullImageIntoNamespace(ctx, canonicalRef, targetNs); perr != nil {
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
	_, err = cl.Pull(nsCtx, canonicalRef,
		client.WithPullUnpack,
	)
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

// copyImageBetweenNamespaces copies an image from one containerd namespace
// to another by walking the descriptor tree (index -> manifests -> config and
// layers) and copying every blob through the content store. The legacy
// Export/Import round-trip cannot handle index (multi-arch) images: the tar
// it produces has neither manifest.json nor an OCI layout, so Import fails
// with "unrecognized image format".
func copyImageBetweenNamespaces(srcNs, ref, targetNs string) error {
	ctx := context.Background()
	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	srcCtx := namespaces.WithNamespace(ctx, srcNs)
	dstCtx := namespaces.WithNamespace(ctx, targetNs)
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

	cs := cl.ContentStore()
	// An image record may live in one namespace while some of its blobs sit
	// in another (records are cheap metadata; pulls and unpacks move blobs
	// around). Resolve each blob in any namespace, preferring srcNs.
	srcCtxs := []context.Context{srcCtx}
	if nss, lerr := cl.NamespaceService().List(ctx); lerr == nil {
		for _, ns := range nss {
			if ns != srcNs && ns != targetNs {
				srcCtxs = append(srcCtxs, namespaces.WithNamespace(ctx, ns))
			}
		}
	}
	blobCtx := func(d ocispec.Descriptor) context.Context {
		for _, c := range srcCtxs {
			if _, ierr := cs.Info(c, d.Digest); ierr == nil {
				return c
			}
		}
		return srcCtxs[0]
	}
	var copyDesc func(d ocispec.Descriptor) error
	copyDesc = func(d ocispec.Descriptor) error {
		if _, ierr := cs.Info(dstCtx, d.Digest); ierr == nil {
			return nil // blob already visible in the target namespace
		}
		bctx := blobCtx(d)
		// Recurse into child descriptors for indexes and manifests.
		switch d.MediaType {
		case ocispec.MediaTypeImageIndex, images.MediaTypeDockerSchema2ManifestList:
			var idx ocispec.Index
			if data, rerr := readBlobAll(bctx, cs, d); rerr == nil {
				if jerr := json.Unmarshal(data, &idx); jerr == nil {
					for _, child := range idx.Manifests {
						if cerr := copyDesc(child); cerr != nil {
							return cerr
						}
					}
				}
			}
		case ocispec.MediaTypeImageManifest, images.MediaTypeDockerSchema2Manifest:
			var man ocispec.Manifest
			if data, rerr := readBlobAll(bctx, cs, d); rerr == nil {
				if jerr := json.Unmarshal(data, &man); jerr == nil {
					if cerr := copyDesc(man.Config); cerr != nil {
						return cerr
					}
					for _, layer := range man.Layers {
						if cerr := copyDesc(layer); cerr != nil {
							return cerr
						}
					}
				}
			}
		}
		return copyBlob(bctx, dstCtx, cs, d)
	}
	if err := copyDesc(img.Target); err != nil {
		return err
	}
	if err := putImage(cl, dstCtx, images.Image{Name: canonical, Target: img.Target, Labels: img.Labels}); err != nil {
		return fmt.Errorf("put image: %w", err)
	}
	return nil
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

func copyBlob(srcCtx, dstCtx context.Context, cs content.Store, d ocispec.Descriptor) error {
	w, err := cs.Writer(dstCtx, content.WithDescriptor(d), content.WithRef("copy-"+d.Digest.String()))
	if err != nil {
		return err
	}
	defer w.Close()
	if err := copyBlobContent(srcCtx, cs, w, d); err != nil {
		return err
	}
	return w.Commit(dstCtx, d.Size, d.Digest)
}

func copyBlobContent(srcCtx context.Context, cs content.Store, w content.Writer, d ocispec.Descriptor) error {
	ra, err := cs.ReaderAt(srcCtx, d)
	if err != nil {
		return err
	}
	defer ra.Close()
	if _, err := io.Copy(w, io.NewSectionReader(ra, 0, ra.Size())); err != nil {
		return err
	}
	return nil
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
	// it would make the subsequent nerdctl inspect in that namespace fail
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
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		images, err := cl.ListImages(nsCtx)
		if err != nil {
			debugLog("list images in %s: %v", ns, err)
			continue
		}
		for _, img := range images {
			name := img.Name()
			for _, v := range variants {
				if name == v {
					return ns
				}
			}
		}
	}
	debugLog("findImageNamespace: no namespace for %s", ref)
	return ""
}

// tagDockerImage creates a new image record pointing at the same content —
// containerd tags are just additional names for a target descriptor.
func tagDockerImage(ctx context.Context, source, target string) error {
	ns := findImageNamespace(ctx, source)
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

func streamImageSave(ctx context.Context, w http.ResponseWriter, names []string) {
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
	nsCtx := namespaces.WithNamespace(ctx, ns)
	for _, name := range names {
		for _, candidate := range []string{canonicalizeImageRef(name), name} {
			img, gerr := cl.GetImage(nsCtx, candidate)
			if gerr != nil {
				continue
			}
			imgs = append(imgs, images.Image{
				Name:   canonicalizeImageRef(name),
				Labels: img.Labels(),
				Target: img.Target(),
			})
			break
		}
	}
	if len(imgs) == 0 {
		http.Error(w, `{"message":"save failed: no image records"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	if err := cl.Export(nsCtx, w, archive.WithImages(imgs)); err != nil {
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
