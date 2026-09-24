package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"

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
		if perr := pullImageIntoNamespace(ctx, canonicalizeImageRef(source), "default", nil); perr != nil {
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
	// one namespace. The mirror copies the TREE (under a lease), so the
	// tag is not left dangling when a racing GC pass sweeps the source
	// namespace right after the records are created.
	if ns != "default" {
		if err := ensureImageInNamespace(ctx, target, "default", nil); err != nil {
			log.Printf("[images] mirror tag %s to default: %v", target, err)
			// The source tree was swept mid-copy. Re-pull the SOURCE into
			// default (works for registry-backed refs) and re-tag from the
			// fresh copy; otherwise remove the tag records we just made so
			// no dangling tag is left behind.
			if perr := pullImageIntoNamespace(ctx, canonicalizeImageRef(source), "default", nil); perr != nil {
				for _, name := range []string{canonicalTarget, target} {
					_ = cl.ImageService().Delete(nsCtx, name)
				}
				return fmt.Errorf("tag %s: source content missing: %w", target, err)
			}
			dctx := namespaces.WithNamespace(ctx, "default")
			if dimg, dgerr := cl.GetImage(dctx, canonicalizeImageRef(source)); dgerr == nil {
				for _, name := range []string{canonicalTarget, target} {
					if name == "" {
						continue
					}
					_ = putImage(cl, dctx, images.Image{Name: name, Target: dimg.Target(), Labels: dimg.Labels()})
				}
				return nil
			}
		}
	}
	return nil
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
