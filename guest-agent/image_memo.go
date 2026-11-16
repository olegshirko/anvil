package main

// Image create-path memo.
//
// Every container create asks containerd three questions it has already
// answered for the same image: whether the layers are unpacked in the default
// snapshotter (img.Unpack, ~3 ms), whether the image record is complete in
// the target namespace (ensureImageInNamespace, ~1.5 ms), and what the image
// config says (spec, exposed ports, healthcheck, platform). The answers only
// change when the image does, so they are memoized:
//
//   - "unpacked" (image digest + platform) and "present" (namespace, ref,
//     platform) are persisted in /var/lib/anvil/image-memo.json, so a restart
//     does not re-pay them;
//   - the config facts are kept in memory only: re-reading the config blob
//     once after a restart is cheap, and an in-memory cache cannot outlive
//     the process that knows how to invalidate it.
//
// Invalidation: docker rmi, image prune, system prune and every pull that
// rewrites an image record drop the affected entries. containerd's GC can
// also remove unpacked snapshots asynchronously without telling us, so a
// memo hit may go stale behind our back; the create path therefore treats
// NewContainer's "parent snapshot does not exist" as a memo miss — it clears
// the entry, unpacks once and retries, and never surfaces that failure to the
// client. A memo hit is therefore always an optimization, never a
// correctness dependency.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	imageMemoFileName = "image-memo.json"
	imageMemoVersion  = 1
)

// imageMemoPath is the persisted memo. It lives next to the container
// metadata on the persistent disk, so it survives resume and cold boot with
// the containerd data it describes.
func imageMemoPath() string {
	return filepath.Join(anvilStoreRoot, imageMemoFileName)
}

// imageMemoFile is the on-disk shape. Keys:
//   - unpacked: "<image digest>|<platform>"
//   - present:  "<namespace>|<canonical ref>|<platform>" -> image digest
//
// The platform is the requested one ("" for the default matcher): the same
// image digest under a different platform unpacked different layers.
type imageMemoFile struct {
	Version  int               `json:"version"`
	Unpacked map[string]bool   `json:"unpacked,omitempty"`
	Present  map[string]string `json:"present,omitempty"`
}

type imageMemoStore struct {
	mu       sync.Mutex
	loaded   bool
	unpacked map[string]bool   // digest|platform -> layers unpacked
	present  map[string]string // namespace|ref|platform -> digest
}

// imageMemo is the process-wide memo used by the create path.
var imageMemo = newImageMemoStore()

func newImageMemoStore() *imageMemoStore {
	return &imageMemoStore{
		unpacked: map[string]bool{},
		present:  map[string]string{},
	}
}

func imageUnpackKey(d digest.Digest, platform string) string {
	return d.String() + "|" + platform
}

func imagePresentKey(ns, ref, platform string) string {
	return ns + "|" + ref + "|" + platform
}

// ensureLoaded reads the persisted file once. A missing, unreadable or
// corrupt file is a cold cache: every miss re-derives its answer from
// containerd, so there is nothing to repair.
func (m *imageMemoStore) ensureLoaded() {
	if m.loaded {
		return
	}
	m.loaded = true
	data, err := os.ReadFile(imageMemoPath())
	if err != nil {
		return
	}
	var f imageMemoFile
	if json.Unmarshal(data, &f) != nil || f.Version != imageMemoVersion {
		return
	}
	for k, v := range f.Unpacked {
		if v {
			m.unpacked[k] = true
		}
	}
	for k, v := range f.Present {
		if v != "" {
			m.present[k] = v
		}
	}
}

// persistLocked writes the memo atomically. Failures are logged and dropped:
// the memo is an optimization, and a lost write only costs the next restart
// one re-derivation per image.
func (m *imageMemoStore) persistLocked() {
	data, err := json.Marshal(imageMemoFile{
		Version:  imageMemoVersion,
		Unpacked: m.unpacked,
		Present:  m.present,
	})
	if err != nil {
		return
	}
	path := imageMemoPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		debugLog("image memo: mkdir: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		debugLog("image memo: write: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		debugLog("image memo: rename: %v", err)
	}
}

// isUnpacked reports whether the image's layers for platform are known to be
// unpacked in the snapshotter.
func (m *imageMemoStore) isUnpacked(d digest.Digest, platform string) bool {
	if d == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	return m.unpacked[imageUnpackKey(d, platform)]
}

func (m *imageMemoStore) markUnpacked(d digest.Digest, platform string) {
	if d == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	key := imageUnpackKey(d, platform)
	if m.unpacked[key] {
		return
	}
	m.unpacked[key] = true
	m.persistLocked()
}

func (m *imageMemoStore) forgetUnpacked(d digest.Digest, platform string) {
	if d == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	key := imageUnpackKey(d, platform)
	if !m.unpacked[key] {
		return
	}
	delete(m.unpacked, key)
	m.persistLocked()
}

// isPresent reports whether (namespace, ref, platform) was verified healthy
// and its record still points at digest.
func (m *imageMemoStore) isPresent(ns, ref, platform string, d digest.Digest) bool {
	if d == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	return m.present[imagePresentKey(ns, ref, platform)] == d.String()
}

func (m *imageMemoStore) markPresent(ns, ref, platform string, d digest.Digest) {
	if d == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	key := imagePresentKey(ns, ref, platform)
	if m.present[key] == d.String() {
		return
	}
	m.present[key] = d.String()
	m.persistLocked()
}

// forgetRef drops the "present" answers for one ref in one namespace, every
// platform. Used when a pull or a dangling-tree refresh rewrites the record.
func (m *imageMemoStore) forgetRef(ns, ref string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLoaded()
	prefix := ns + "|" + ref + "|"
	changed := false
	for k := range m.present {
		if strings.HasPrefix(k, prefix) {
			delete(m.present, k)
			changed = true
		}
	}
	if changed {
		m.persistLocked()
	}
}

// forgetImage drops every memo entry about one image digest: its unpacked
// platforms and every namespace/ref that recorded it, plus the in-memory
// config facts. Called by rmi, image prune and system prune.
func (m *imageMemoStore) forgetImage(d digest.Digest) {
	if d == "" {
		return
	}
	unpackPrefix := d.String() + "|"
	digestStr := d.String()
	m.mu.Lock()
	m.ensureLoaded()
	changed := false
	for k := range m.unpacked {
		if strings.HasPrefix(k, unpackPrefix) {
			delete(m.unpacked, k)
			changed = true
		}
	}
	for k, v := range m.present {
		if v == digestStr {
			delete(m.present, k)
			changed = true
		}
	}
	if changed {
		m.persistLocked()
	}
	m.mu.Unlock()
	forgetImageInfo(d)
}

// missingUnpackedLayers reports whether a NewContainer error means the
// snapshotter could not find the image's unpacked layer chain. The metadata
// snapshotter answers NotFound ("parent snapshot ... does not exist"); the
// string checks cover wrappers that lose the gRPC status.
func missingUnpackedLayers(err error) bool {
	if err == nil {
		return false
	}
	if errdefs.IsNotFound(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "parent snapshot") ||
		strings.Contains(msg, "unpacked") ||
		(strings.Contains(msg, "snapshot") && strings.Contains(msg, "not found"))
}

// --- image config facts ----------------------------------------------------

// imageInfo is the per-(digest, requested platform) config cache: the config
// blob, read and decoded once, plus the platform resolution it produced.
type imageInfo struct {
	// resolved is the platform matcher the request resolved to: the
	// requested platform, the Rosetta amd64 fallback, or "" for the client
	// default matcher. It keys the unpack memo.
	resolved string
	// platform is the image config's own platform ("linux/amd64").
	platform string
	// warning is Docker's create warning for an emulated image ("" = none).
	warning string
	// config is nil when the config blob could not be read or decoded. Only
	// the ImageConfig is kept: the config's history is not needed by the
	// create path and can be large.
	config *ocispec.ImageConfig
	// extensions are the Docker-only config fields (Healthcheck, OnBuild,
	// Shell) that ocispec.ImageConfig does not carry.
	extensions map[string]json.RawMessage
}

var imageInfoCache = struct {
	mu sync.Mutex
	m  map[string]*imageInfo
}{m: map[string]*imageInfo{}}

func imageInfoKey(d digest.Digest, requested string) string {
	return d.String() + "|" + requested
}

func lookupImageInfo(d digest.Digest, requested string) *imageInfo {
	imageInfoCache.mu.Lock()
	defer imageInfoCache.mu.Unlock()
	return imageInfoCache.m[imageInfoKey(d, requested)]
}

func storeImageInfo(d digest.Digest, requested string, inf *imageInfo) {
	imageInfoCache.mu.Lock()
	defer imageInfoCache.mu.Unlock()
	imageInfoCache.m[imageInfoKey(d, requested)] = inf
}

func forgetImageInfo(d digest.Digest) {
	prefix := d.String() + "|"
	imageInfoCache.mu.Lock()
	defer imageInfoCache.mu.Unlock()
	for k := range imageInfoCache.m {
		if strings.HasPrefix(k, prefix) {
			delete(imageInfoCache.m, k)
		}
	}
}

// resolveImageInfo returns img resolved for the requested platform (the
// same choice imageWithPlatform makes) and its cached config facts. The
// platform content probes and the config blob read run once per (digest,
// requested platform); a container create that repeats them pays neither.
func resolveImageInfo(ctx context.Context, cl *client.Client, img client.Image, requested string) (client.Image, *imageInfo) {
	if img == nil {
		return nil, &imageInfo{}
	}
	if inf := lookupImageInfo(img.Target().Digest, requested); inf != nil {
		return imageResolvedFor(cl, img, inf.resolved), inf
	}

	// Same resolution as imageWithPlatform: no request, but the record holds
	// only an amd64 subtree while Rosetta is on, runs the amd64 variant.
	resolved := requested
	if resolved == "" && rosettaActive() {
		cs := cl.ContentStore()
		native := platforms.Format(hostPlatform)
		if !platformContentPresent(ctx, cs, img.Target(), native) &&
			platformContentPresent(ctx, cs, img.Target(), platforms.Format(amd64Platform)) {
			resolved = platforms.Format(amd64Platform)
		}
	}
	resolvedImg := imageResolvedFor(cl, img, resolved)
	inf := loadImageInfo(ctx, resolvedImg)
	inf.resolved = resolved
	storeImageInfo(img.Target().Digest, requested, inf)
	return resolvedImg, inf
}

// imageResolvedFor binds img to the matcher of a resolved platform ("" keeps
// the client's default matcher).
func imageResolvedFor(cl *client.Client, img client.Image, resolved string) client.Image {
	if resolved == "" {
		return img
	}
	return client.NewImageWithPlatform(cl, img.Metadata(), platformMatcher(resolved))
}

// loadImageInfo reads the image config blob once and derives everything the
// create path needs from it.
func loadImageInfo(ctx context.Context, img client.Image) *imageInfo {
	inf := &imageInfo{}
	desc, err := img.Config(ctx)
	if err != nil {
		return inf
	}
	blob, err := content.ReadBlob(ctx, img.ContentStore(), desc)
	if err != nil {
		return inf
	}
	var spec ocispec.Image
	if json.Unmarshal(blob, &spec) != nil {
		return inf
	}
	inf.config = &spec.Config
	if spec.Architecture != "" {
		inf.platform = platforms.Format(platforms.Normalize(ocispec.Platform{
			OS: spec.OS, Architecture: spec.Architecture, Variant: spec.Variant,
		}))
		if spec.Architecture != "arm64" {
			inf.warning = fmt.Sprintf("The requested image's platform (%s) does not match the detected host platform (%s) and no specific platform was requested",
				platforms.Format(ocispec.Platform{OS: spec.OS, Architecture: spec.Architecture, Variant: spec.Variant}),
				platforms.Format(hostPlatform))
		}
	}
	var raw struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if json.Unmarshal(blob, &raw) == nil {
		ext := map[string]json.RawMessage{}
		for _, k := range []string{"Healthcheck", "OnBuild", "Shell"} {
			if v, ok := raw.Config[k]; ok && string(v) != "null" {
				ext[k] = v
			}
		}
		if len(ext) > 0 {
			inf.extensions = ext
		}
	}
	return inf
}
