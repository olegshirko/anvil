package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	digest "github.com/opencontainers/go-digest"
)

// docker save / docker load: GET /images/get, /images/{name}/get and
// POST /images/load.

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
		writeJSONError(w, http.StatusBadRequest, "no images specified")
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
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("No such image: %s", name))
			return
		}
		ns = imgNs
	}
	cl, err := pc.get(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
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
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("No such image: %s", name))
			return
		}
		target, serr := stageTreeForExport(bc, src.Target)
		if serr != nil {
			dropScratch()
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("save %s: %s", name, serr.Error()))
			return
		}
		if perr := putImage(cl, scratchCtx, images.Image{Name: canonical, Target: target}); perr != nil {
			dropScratch()
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("save %s: %s", name, perr.Error()))
			return
		}
		imgs = append(imgs, images.Image{Name: canonical, Target: target})
	}
	if len(imgs) == 0 {
		dropScratch()
		writeJSONError(w, http.StatusInternalServerError, "save failed: no image records")
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
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("[docker-api] loading image stream (%d bytes) into ns=%q", r.ContentLength, ns)

	ds, err := compression.DecompressStream(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
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
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("import failed: %s", stripANSI(err.Error())))
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
