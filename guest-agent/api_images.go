package main

// Image endpoints: list/pull/tag/push/rmi/prune/save(load is in images.go).
// Registered in imageRoutes. Image references contain slashes, so the
// sub-resource patterns use *name wildcards.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"io"
	"log"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
)

var imageRoutes = []apiRoute{
	newRoute(http.MethodGet, "/images/json", handleImagesList),
	newRoute(http.MethodPost, "/images/create", handleImageCreate),
	newRoute(http.MethodPost, "/images/prune", handleImagesPrune),
	newRoute(http.MethodPost, "/images/load", func(w http.ResponseWriter, r *http.Request, _ routeParams) {
		handleImageLoad(w, r)
	}),
	newRoute(http.MethodGet, "/images/get", func(w http.ResponseWriter, r *http.Request, _ routeParams) {
		handleImagesGet(w, r)
	}),
	newRoute(http.MethodGet, "/images/search", handleImageSearch),
	newRoute(http.MethodPost, "/commit", handleCommit),
	newRoute(http.MethodPost, "/build/prune", handleBuildPrune),

	newRoute(http.MethodPost, "/images/*name/tag", byImageID(handleImageTag)),
	newRoute(http.MethodPost, "/images/*name/push", handleImagePush),
	newRoute(http.MethodGet, "/images/*name/get", byImageID(func(w http.ResponseWriter, r *http.Request, p routeParams) {
		handleImageGet(w, r, p["name"])
	})),
	newRoute(http.MethodGet, "/images/*name/json", byImageID(handleImageInspect)),
	newRoute(http.MethodGet, "/distribution/*name/json", handleDistributionInspect),
	newRoute(http.MethodGet, "/images/*name/history", byImageID(handleImageHistory)),
	newRoute(http.MethodDelete, "/images/*name", byImageID(handleImageDelete)),
}

func handleImagesList(w http.ResponseWriter, r *http.Request, _ routeParams) {
	images, err := listDockerImages(r.Context())
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	images = mergeImageSummaries(images)
	filters := parseDockerFilters(r.URL.Query().Get("filters"))
	// Old clients send `docker images <name>` as ?filter=<name>.
	if legacy := r.URL.Query().Get("filter"); legacy != "" {
		if filters["reference"] == nil {
			filters["reference"] = map[string]bool{}
		}
		filters["reference"][legacy] = true
	}
	if err := validateFilterKeys(filters, imageFilterKeys); err != nil {
		writeAPIError(w, err, http.StatusBadRequest)
		return
	}
	// before=/since=: images created before/after the referenced one.
	for _, key := range []string{"before", "since"} {
		for ref := range filters[key] {
			created, ok := imageCreatedOf(images, ref)
			if !ok {
				writeAPIError(w, fmt.Errorf("No such image: %s", ref), http.StatusNotFound)
				return
			}
			kept := images[:0]
			for _, img := range images {
				if (key == "before" && img.Created < created) || (key == "since" && img.Created > created) {
					kept = append(kept, img)
				}
			}
			images = kept
		}
	}
	if len(filters["until"]) > 0 {
		pf, err := newPruneFilter(map[string]map[string]bool{"until": filters["until"]})
		if err != nil {
			writeAPIError(w, err, http.StatusBadRequest)
			return
		}
		kept := images[:0]
		for _, img := range images {
			if pf.keep(nil, time.Unix(img.Created, 0)) {
				kept = append(kept, img)
			}
		}
		images = kept
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(filterImageSummaries(images, filters))
}

var imageFilterKeys = map[string]bool{
	"before": true, "since": true, "until": true, "dangling": true,
	"label": true, "label!": true, "reference": true,
}

// imageCreatedOf finds an image by tag or ID prefix and returns its
// creation time.
func imageCreatedOf(images []dockerImageSummary, ref string) (int64, bool) {
	canon := canonicalizeImageRef(ref)
	for _, img := range images {
		if strings.HasPrefix(img.Id, ref) || strings.HasPrefix(strings.TrimPrefix(img.Id, "sha256:"), ref) {
			return img.Created, true
		}
		for _, t := range img.RepoTags {
			if t == ref || canonicalizeImageRef(t) == canon {
				return img.Created, true
			}
		}
	}
	return 0, false
}

// filterImageSummaries applies docker images' reference, dangling and label
// filters. A reference filter also narrows RepoTags to the matching tags,
// as dockerd does.
func filterImageSummaries(in []dockerImageSummary, filters map[string]map[string]bool) []dockerImageSummary {
	out := make([]dockerImageSummary, 0, len(in))
	for _, img := range in {
		if d := filters["dangling"]; len(d) > 0 {
			dangling := len(img.RepoTags) == 0
			if (d["true"] || d["1"]) != dangling {
				continue
			}
		}
		if !matchesLabelFilters(img.Labels, filters) {
			continue
		}
		if refs := filters["reference"]; len(refs) > 0 {
			var kept []string
			for _, tag := range img.RepoTags {
				for pattern := range refs {
					if familiarMatch(pattern, tag) {
						kept = append(kept, tag)
						break
					}
				}
			}
			if len(kept) == 0 {
				continue
			}
			img.RepoTags = kept
		}
		out = append(out, img)
	}
	return out
}

// familiarMatch is reference.FamiliarMatch: the pattern (path.Match syntax)
// against the short form ("busybox:latest", "me/app:1"), then against the
// name alone ("busybox").
func familiarMatch(pattern, ref string) bool {
	familiar := familiarRef(ref)
	if ok, _ := path.Match(pattern, familiar); ok {
		return true
	}
	name, _ := splitRepoTag(familiar)
	ok, _ := path.Match(pattern, name)
	return ok
}

// familiarRef shortens docker.io references the way the docker CLI prints
// them: docker.io/library/busybox:latest -> busybox:latest.
func familiarRef(ref string) string {
	if rest, ok := strings.CutPrefix(ref, "docker.io/library/"); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(ref, "docker.io/"); ok {
		return rest
	}
	return ref
}

func handleImageCreate(w http.ResponseWriter, r *http.Request, _ routeParams) {
	if r.URL.Query().Get("fromSrc") != "" {
		handleImageImport(w, r) // docker import
		return
	}
	image := r.URL.Query().Get("fromImage")
	if tag := r.URL.Query().Get("tag"); tag != "" {
		// The CLI sends `pull img@sha256:…` as fromImage=img&tag=sha256:…
		if strings.Contains(tag, ":") {
			image += "@" + tag
		} else {
			image += ":" + tag
		}
	}
	if image == "" {
		writeJSONError(w, http.StatusBadRequest, "missing fromImage")
		return
	}
	// Headers and progress go out as the pull runs, as from dockerd.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	prog := newPullProgress(w)
	canonical := canonicalizeImageRef(image)
	repo, tag := splitRepoTag(canonical)
	if tag == "" {
		tag = "latest"
		if _, d, ok := strings.Cut(canonical, "@"); ok {
			tag = d
		}
	}
	prog.send(map[string]any{"status": "Pulling from " + strings.TrimPrefix(repo, "docker.io/"), "id": tag})

	platform, perr := resolveRequestedPlatform(r.URL.Query().Get("platform"))
	status, err := "", perr
	if perr == nil {
		status, err = pullDockerImage(withPullProgress(r.Context(), prog), image, platform, parseRegistryAuth(r))
		if err == nil {
			publishObjectEvent("image", "pull", canonical, map[string]string{"name": image})
		}
	}
	if err != nil {
		// The progress stream must carry the failure in "error"
		// (with errorDetail): a plain status line makes the CLI print the
		// error but still exit 0, masking the failure.
		prog.send(map[string]any{
			"error":       fmt.Sprintf("error pulling %s: %s", image, err.Error()),
			"errorDetail": map[string]string{"message": err.Error()},
		})
		return
	}
	prog.complete()
	if d := localImageDigest(r.Context(), "default", image); d != "" {
		prog.status("Digest: " + d)
	}
	prog.status("Status: " + status)
}

func handleImageTag(w http.ResponseWriter, r *http.Request, p routeParams) {
	target := r.URL.Query().Get("repo")
	if tag := r.URL.Query().Get("tag"); tag != "" {
		target += ":" + tag
	}
	if target == "" {
		writeJSONError(w, http.StatusBadRequest, "missing repo/tag")
		return
	}
	if err := tagDockerImage(r.Context(), p["name"], target); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func handleImagePush(w http.ResponseWriter, r *http.Request, p routeParams) {
	w.Header().Set("Content-Type", "application/json")
	name := p["name"]
	// The CLI sends the tag separately: POST /images/<repo>/push?tag=<tag>.
	// Ignoring it pushed (or failed to find) <repo>:latest.
	if tag := r.URL.Query().Get("tag"); tag != "" {
		if strings.Contains(tag, ":") {
			name += "@" + tag
		} else {
			name += ":" + tag
		}
	}
	// pushDockerImage reports failures as errorDetail lines, which make the
	// CLI exit non-zero (a plain status line looked like success).
	pushDockerImage(r.Context(), name, parseRegistryAuth(r), &pushErrorWriter{w: w}) //nolint:errcheck
}

// pushErrorWriter makes sure a failed push ends with an errorDetail line:
// pushDockerImage writes one on most failures; the handler adds one for
// any path that returns before it.
type pushErrorWriter struct {
	w        io.Writer
	reported bool
}

func (p *pushErrorWriter) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte(`"errorDetail"`)) {
		p.reported = true
	}
	return p.w.Write(b)
}

// handleImageDelete implements docker rmi; force=1 is `rmi -f` (compose down
// --rmi sends it).
func handleImageDelete(w http.ResponseWriter, r *http.Request, p routeParams) {
	name := p["name"]
	// Guard the wildcard from swallowing the literal sub-resources (e.g.
	// DELETE /images/json) when no literal route matched.
	if name == "" || name == "json" || name == "create" {
		http.NotFound(w, r)
		return
	}
	force := queryBool(r.URL.Query(), "force")
	// By ID: every tag of the image goes, which Docker refuses without -f
	// when there is more than one.
	targets := []string{name}
	id := ""
	if names, dgst := imageRecordsByID(r.Context(), p["ref"]); len(names) > 0 {
		targets, id = names, dgst
		if !force && !singleImageReference(names) {
			writeAPIError(w, errConflict("conflict: unable to delete %s (must be forced) - image is referenced in multiple repositories",
				truncateID(strings.TrimPrefix(dgst, "sha256:"))), http.StatusConflict)
			return
		}
	} else if insp, ierr := inspectDockerImage(r.Context(), name); ierr == nil {
		id, _ = insp["Id"].(string)
	}
	resp := []map[string]string{}
	for _, t := range targets {
		if err := removeDockerImage(r.Context(), t, force); err != nil {
			writeAPIError(w, err, http.StatusInternalServerError)
			return
		}
		if !strings.HasPrefix(t, "<none>") {
			untagged := familiarRef(canonicalizeImageRef(t))
			if !slices.ContainsFunc(resp, func(m map[string]string) bool { return m["Untagged"] == untagged }) {
				resp = append(resp, map[string]string{"Untagged": untagged})
			}
		}
	}
	// Deleted only when no other record still points at the image.
	if id != "" {
		if left, _ := imageRecordsByID(r.Context(), id); len(left) == 0 {
			resp = append(resp, map[string]string{"Deleted": id})
		}
	}
	if len(resp) == 0 {
		resp = append(resp, map[string]string{"Deleted": name})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleImagesPrune(w http.ResponseWriter, r *http.Request, _ routeParams) {
	filters := parseDockerFilters(r.URL.Query().Get("filters"))
	dangling := true
	if filters["dangling"]["false"] || filters["dangling"]["0"] {
		dangling = false
	}
	pf, err := newPruneFilter(filters, "dangling")
	if err != nil {
		writeAPIError(w, err, http.StatusBadRequest)
		return
	}
	deleted, reclaimed, err := pruneDockerImages(r.Context(), dangling, pf)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ImagesDeleted":  deleted,
		"SpaceReclaimed": reclaimed,
	})
}

func handleBuildPrune(w http.ResponseWriter, r *http.Request, _ routeParams) {
	opts, err := buildPruneOptions(r.URL.Query())
	if err != nil {
		writeAPIError(w, err, http.StatusBadRequest)
		return
	}
	reclaimed, deleted, err := pruneBuildCache(opts...)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"CachesDeleted":  deleted,
		"SpaceReclaimed": reclaimed,
	})
}

func handleImageInspect(w http.ResponseWriter, r *http.Request, p routeParams) {
	log.Printf("[docker-api] image inspect %q (resolved ns=%q)", p["name"], findImageNamespace(r.Context(), p["name"]))
	info, err := inspectDockerImage(r.Context(), p["name"])
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func handleImageHistory(w http.ResponseWriter, r *http.Request, p routeParams) {
	items, err := imageHistory(r.Context(), p["name"])
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

// byImageID lets an image route take an image ID ("sha256:<hex>" or a hex
// prefix of 12+ characters), as Docker does: the ID is resolved to one of
// the image's records before the handler runs. docker image inspect/rmi/
// tag by ID answered "No such image" for every image.
func byImageID(h func(w http.ResponseWriter, r *http.Request, p routeParams)) func(w http.ResponseWriter, r *http.Request, p routeParams) {
	return func(w http.ResponseWriter, r *http.Request, p routeParams) {
		if name, ok := resolveImageID(r.Context(), p["name"]); ok {
			q := routeParams{}
			for k, v := range p {
				q[k] = v
			}
			q["name"] = name
			q["ref"] = p["name"] // the ID as given
			p = q
		}
		h(w, r, p)
	}
}

// resolveImageID maps an image ID (or a unique hex prefix of one) to an
// image record name — a tagged one when there is any.
func resolveImageID(ctx context.Context, ref string) (string, bool) {
	names, _ := imageRecordsByID(ctx, ref)
	if len(names) == 0 {
		return "", false
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "<none>") {
			return n, true
		}
	}
	return names[0], true
}

// imageRecordsByID returns every image record (sorted, deduplicated) whose
// target digest starts with the ID, and the full ID; nothing when ref is not
// an ID form ("sha256:<hex>" or 12+ hex characters) or matches several
// images.
func imageRecordsByID(ctx context.Context, ref string) ([]string, string) {
	hex := strings.TrimPrefix(ref, "sha256:")
	if ref == "" || strings.Trim(hex, "0123456789abcdef") != "" || (hex == ref && len(hex) < 12) {
		return nil, ""
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, ""
	}
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, ""
	}
	names := map[string]bool{}
	ids := map[string]bool{}
	for _, ns := range nss {
		if ns == saveScratchNs {
			continue
		}
		imgs, err := cl.ImageService().List(namespaces.WithNamespace(ctx, ns))
		if err != nil {
			continue
		}
		for _, img := range imgs {
			if strings.HasPrefix(img.Target.Digest.Encoded(), hex) {
				names[img.Name] = true
				ids[img.Target.Digest.String()] = true
			}
		}
	}
	if len(ids) != 1 {
		return nil, "" // none, or an ambiguous prefix
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	var id string
	for k := range ids {
		id = k
	}
	return out, id
}

// singleImageReference is Docker's isSingleReference over an image's
// records: at most one tag, and digest references only of that tag's
// repository. Records are deduplicated first: a tag is stored under its
// familiar and canonical names, and untagged builds keep a <none> record.
func singleImageReference(records []string) bool {
	tags := map[string]bool{}
	digestRepos := map[string]bool{}
	for _, r := range records {
		if strings.HasPrefix(r, "<none>") {
			continue
		}
		ref := canonicalizeImageRef(r)
		if repo, _, ok := strings.Cut(ref, "@"); ok {
			digestRepos[repo] = true
			continue
		}
		tags[ref] = true
	}
	if len(tags) > 1 {
		return false
	}
	for tag := range tags {
		repo := tag
		if i := strings.LastIndex(tag, ":"); i > strings.LastIndex(tag, "/") {
			repo = tag[:i]
		}
		delete(digestRepos, repo)
	}
	if len(tags) == 1 {
		return len(digestRepos) == 0
	}
	return len(digestRepos) <= 1
}
