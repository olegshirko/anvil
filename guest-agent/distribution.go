package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// handleDistributionInspect implements GET /distribution/{name}/json: the
// registry's descriptor for a reference and the platforms it offers.
// compose resolves image digests through it (--resolve-image-digests,
// publish).
func handleDistributionInspect(w http.ResponseWriter, r *http.Request, p routeParams) {
	ref := canonicalizeImageRef(p["name"])
	a := parseRegistryAuth(r)
	authOpts := []docker.AuthorizerOpt{docker.WithAuthClient(authHTTPClient)}
	if !a.empty() {
		authOpts = append(authOpts, docker.WithAuthCreds(a.credentials))
	}
	resolver := docker.NewResolver(docker.ResolverOptions{
		Hosts: docker.ConfigureDefaultRegistries(
			docker.WithClient(authHTTPClient),
			docker.WithPlainHTTP(docker.MatchLocalhost),
			docker.WithAuthorizer(docker.NewDockerAuthorizer(authOpts...)),
		),
	})
	ctx := r.Context()
	name, desc, err := resolver.Resolve(ctx, ref)
	if err != nil {
		writeAPIError(w, err, http.StatusNotFound)
		return
	}
	platforms := []ocispec.Platform{}
	if images.IsIndexType(desc.MediaType) {
		if fetcher, ferr := resolver.Fetcher(ctx, name); ferr == nil {
			if data, rerr := fetchBlob(ctx, fetcher, desc); rerr == nil {
				var idx ocispec.Index
				if json.Unmarshal(data, &idx) == nil {
					for _, m := range idx.Manifests {
						if m.Platform != nil && m.Platform.OS != "unknown" {
							platforms = append(platforms, *m.Platform)
						}
					}
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
		"Descriptor": ocispec.Descriptor{MediaType: desc.MediaType, Digest: desc.Digest, Size: desc.Size},
		"Platforms":  platforms,
	})
}

// fetchBlob reads a (small) blob, the index, through a registry fetcher.
func fetchBlob(ctx context.Context, f remotes.Fetcher, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := f.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 4<<20))
}
