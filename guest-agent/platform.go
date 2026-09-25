package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Image platforms. The guest is linux/arm64; with Rosetta on (the host ran
// with ANVIL_ROSETTA=1 and stage2 registered the binfmt_misc handler)
// linux/amd64 runs too:
//
//   - an explicit --platform linux/amd64 pulls and runs the amd64 variant;
//   - with no platform, arm64 is preferred and an amd64-only image falls
//     back to amd64 (a warning is returned, as Docker Desktop does).
//
// Without Rosetta every non-arm64 request is refused as before.

var (
	hostPlatform  = ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	amd64Platform = ocispec.Platform{OS: "linux", Architecture: "amd64"}
)

// rosettaBinfmt is the handler stage2 registers when the Rosetta share is
// mounted.
const rosettaBinfmt = "/proc/sys/fs/binfmt_misc/rosetta"

// rosettaActive reports whether amd64 binaries can run. Seam for tests;
// stage2 registers the handler before the agent starts, so it is read once.
var rosettaActive = sync.OnceValue(func() bool {
	_, err := os.Stat(rosettaBinfmt)
	return err == nil
})

// resolveRequestedPlatform normalizes a create/pull platform: "" for the
// native platform (or none requested), "linux/amd64" when Rosetta can run
// it, an error naming the platform otherwise.
func resolveRequestedPlatform(p string) (string, error) {
	if p == "" || p == "linux" {
		return "", nil
	}
	spec, err := platforms.Parse(p)
	if err != nil {
		return "", fmt.Errorf("invalid platform %q: %v", p, err)
	}
	if spec.OS == "linux" && spec.Architecture == "arm64" {
		// With Rosetta the default matcher falls back to amd64; an
		// explicit arm64 request must not (Docker: "no matching manifest").
		if rosettaActive() {
			return platforms.Format(hostPlatform), nil
		}
		return "", nil
	}
	if spec.OS == "linux" && spec.Architecture == "amd64" && (spec.Variant == "" || spec.Variant == "v1") {
		if rosettaActive() {
			return platforms.Format(amd64Platform), nil
		}
		return "", fmt.Errorf("platform %q is not supported by anvil: amd64 needs Rosetta — restart anvil with ANVIL_ROSETTA=1 (Rosetta installed on the Mac)", p)
	}
	return "", fmt.Errorf("platform %q is not supported by anvil: the guest runs linux/arm64 (and linux/amd64 through Rosetta)", p)
}

// defaultPlatformMatcher is the containerd client's platform: arm64, then
// amd64 when Rosetta is on (Ordered prefers the earlier platform, and pulls
// fetch only the best match).
func defaultPlatformMatcher() platforms.MatchComparer {
	if rosettaActive() {
		return platforms.Ordered(hostPlatform, amd64Platform)
	}
	return platforms.Only(hostPlatform)
}

// platformMatcher is the matcher for a resolved platform ("" = default).
func platformMatcher(resolved string) platforms.MatchComparer {
	if resolved == "" {
		return defaultPlatformMatcher()
	}
	spec, err := platforms.Parse(resolved)
	if err != nil {
		return defaultPlatformMatcher()
	}
	return platforms.OnlyStrict(spec)
}

// imageWithPlatform returns img resolved for the requested platform. With
// none requested, a record holding only its amd64 subtree (pulled with
// --platform linux/amd64) resolves to amd64 under Rosetta: the default
// matcher would pick the index's arm64 entry, whose blobs are not local.
func imageWithPlatform(ctx context.Context, cl *client.Client, img client.Image, resolved string) client.Image {
	if resolved == "" && rosettaActive() {
		cs := cl.ContentStore()
		native := platforms.Format(hostPlatform)
		if !platformContentPresent(ctx, cs, img.Target(), native) &&
			platformContentPresent(ctx, cs, img.Target(), platforms.Format(amd64Platform)) {
			resolved = platforms.Format(amd64Platform)
		}
	}
	if resolved == "" {
		return img
	}
	return client.NewImageWithPlatform(cl, img.Metadata(), platformMatcher(resolved))
}

// platformContentPresent reports whether the image has the manifest,
// config and layers of the requested platform locally (a multi-arch record
// may hold only another platform's subtree).
func platformContentPresent(ctx context.Context, cs content.Store, target ocispec.Descriptor, resolved string) bool {
	m, err := images.Manifest(ctx, cs, target, platformMatcher(resolved))
	if err != nil {
		return false
	}
	for _, d := range append([]ocispec.Descriptor{m.Config}, m.Layers...) {
		if _, err := cs.Info(ctx, d.Digest); err != nil {
			return false
		}
	}
	return true
}

// emulatedPlatformWarning is Docker's create warning for an image whose
// platform differs from the host's when none was requested.
func emulatedPlatformWarning(ctx context.Context, img client.Image) string {
	var cfg struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	}
	desc, err := img.Config(ctx)
	if err != nil {
		return ""
	}
	blob, err := content.ReadBlob(ctx, img.ContentStore(), desc)
	if err != nil || json.Unmarshal(blob, &cfg) != nil || cfg.Architecture == "" || cfg.Architecture == "arm64" {
		return ""
	}
	p := platforms.Format(ocispec.Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant})
	return fmt.Sprintf("The requested image's platform (%s) does not match the detected host platform (%s) and no specific platform was requested",
		p, platforms.Format(hostPlatform))
}

// imagePlatform is the platform of the image's config ("linux/amd64"), or
// "" when it cannot be read.
func imagePlatform(ctx context.Context, img client.Image) string {
	var cfg struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	}
	desc, err := img.Config(ctx)
	if err != nil {
		return ""
	}
	blob, err := content.ReadBlob(ctx, img.ContentStore(), desc)
	if err != nil || json.Unmarshal(blob, &cfg) != nil || cfg.Architecture == "" {
		return ""
	}
	return platforms.Format(platforms.Normalize(ocispec.Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant}))
}
