package main

// Image endpoints: list/pull/tag/push/rmi/prune/save(load is in images.go).
// Registered in imageRoutes. Image references contain slashes, so the
// sub-resource patterns use *name wildcards.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"
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

	newRoute(http.MethodPost, "/images/*name/tag", handleImageTag),
	newRoute(http.MethodPost, "/images/*name/push", handleImagePush),
	newRoute(http.MethodGet, "/images/*name/get", func(w http.ResponseWriter, r *http.Request, p routeParams) {
		handleImageGet(w, r, p["name"])
	}),
	newRoute(http.MethodGet, "/images/*name/json", handleImageInspect),
	newRoute(http.MethodGet, "/images/*name/history", handleImageHistory),
	newRoute(http.MethodDelete, "/images/*name", handleImageDelete),
}

func handleImagesList(w http.ResponseWriter, r *http.Request, _ routeParams) {
	images, err := listDockerImages(r.Context())
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	filters := parseDockerFilters(r.URL.Query().Get("filters"))
	// Old clients send `docker images <name>` as ?filter=<name>.
	if legacy := r.URL.Query().Get("filter"); legacy != "" {
		if filters["reference"] == nil {
			filters["reference"] = map[string]bool{}
		}
		filters["reference"][legacy] = true
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(filterImageSummaries(images, filters))
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
	platform, perr := resolveRequestedPlatform(r.URL.Query().Get("platform"))
	status, err := "", perr
	if perr == nil {
		status, err = pullDockerImage(r.Context(), image, platform, parseRegistryAuth(r))
		if err == nil {
			publishObjectEvent("image", "pull", canonicalizeImageRef(image), map[string]string{"name": image})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		// The progress stream must carry the failure in "error"
		// (with errorDetail): a plain status line makes the CLI print the
		// error but still exit 0, masking the failure.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":       fmt.Sprintf("error pulling %s: %s", image, err.Error()),
			"errorDetail": map[string]string{"message": err.Error()},
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{
		"status": status,
	})
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
	pushDockerImage(r.Context(), name, parseRegistryAuth(r), w) //nolint:errcheck
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
	if err := removeDockerImage(r.Context(), name, queryBool(r.URL.Query(), "force")); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode([]map[string]string{{"Deleted": name}})
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

func handleBuildPrune(w http.ResponseWriter, _ *http.Request, _ routeParams) {
	reclaimed, err := pruneBuildCache()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"CachesDeleted":  []string{},
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
