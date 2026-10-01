package main

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// handleImageImport implements POST /images/create?fromSrc=… (docker
// import): a filesystem tarball — the request body for "-", else a URL —
// becomes a single-layer image named repo:tag, with --change applied.
func handleImageImport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	src := q.Get("fromSrc")
	var body io.Reader = r.Body
	if src != "-" {
		resp, err := authHTTPClient.Get(src)
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("fetch %s: %s", src, resp.Status))
			return
		}
		body = resp.Body
	}
	id, err := importImage(r.Context(), body, q.Get("repo"), q.Get("tag"), q.Get("message"), q["changes"])
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		msg := err.Error()
		fmt.Fprintf(w, `{"errorDetail":{"message":%q},"error":%q}`+"\n", msg, msg)
		return
	}
	fmt.Fprintf(w, `{"status":%q}`+"\n", id)
}

func importImage(ctx context.Context, tarball io.Reader, repo, tag, message string, changes []string) (string, error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return "", fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, "default")
	nsCtx, done, err := cl.WithLease(nsCtx)
	if err != nil {
		return "", fmt.Errorf("lease: %w", err)
	}
	defer done(context.WithoutCancel(nsCtx))
	cs := cl.ContentStore()

	// Re-compress to gzip on the persistent disk, digesting both forms.
	plain, err := compression.DecompressStream(tarball)
	if err != nil {
		return "", fmt.Errorf("read tarball: %w", err)
	}
	defer plain.Close()
	tmp, err := os.CreateTemp("/var/lib", "anvil-import-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	diffHash, gzHash := sha256.New(), sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(tmp, gzHash))
	if _, err := io.Copy(io.MultiWriter(gz, diffHash), plain); err != nil {
		return "", fmt.Errorf("read tarball: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	size, err := tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	diffID := digest.NewDigestFromBytes(digest.SHA256, diffHash.Sum(nil))
	layer := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip,
		Digest:    digest.NewDigestFromBytes(digest.SHA256, gzHash.Sum(nil)),
		Size:      size,
	}
	if err := content.WriteBlob(nsCtx, cs, "anvil-import-"+layer.Digest.Encoded(), tmp, layer,
		content.WithLabels(map[string]string{"containerd.io/uncompressed": diffID.String()})); err != nil {
		return "", fmt.Errorf("write layer: %w", err)
	}

	now := time.Now().UTC()
	cfg := ocispec.Image{
		Created:  &now,
		Platform: ocispec.Platform{OS: hostPlatform.OS, Architecture: hostPlatform.Architecture, Variant: hostPlatform.Variant},
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{diffID}},
		History:  []ocispec.History{{Created: &now, Comment: message, CreatedBy: "docker import"}},
	}
	for _, ch := range changes {
		if err := applyCommitChange(&cfg.Config, ch); err != nil {
			return "", err
		}
	}
	configDesc, err := writeJSONBlob(nsCtx, cs, ocispec.MediaTypeImageConfig, cfg, nil)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layer},
	}
	manifest.SchemaVersion = 2
	manifestDesc, err := writeJSONBlob(nsCtx, cs, ocispec.MediaTypeImageManifest, manifest, map[string]string{
		"containerd.io/gc.ref.content.config": configDesc.Digest.String(),
		"containerd.io/gc.ref.content.l.0":    layer.Digest.String(),
	})
	if err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}
	name := commitImageName(repo, tag, manifestDesc.Digest)
	rec := images.Image{Name: name, Target: manifestDesc, CreatedAt: now, UpdatedAt: now}
	if err := putImage(cl, nsCtx, rec); err != nil {
		return "", fmt.Errorf("create image %s: %w", name, err)
	}
	if err := client.NewImage(cl, rec).Unpack(nsCtx, ""); err != nil {
		return "", fmt.Errorf("unpack %s: %w", name, err)
	}
	publishObjectEvent("image", "import", manifestDesc.Digest.String(), map[string]string{"name": name})
	return manifestDesc.Digest.String(), nil
}
