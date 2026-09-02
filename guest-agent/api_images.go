package main

// Image endpoints: list/pull/tag/push/rmi/prune/save(load is in images.go).
// Registered in imageRoutes. Image references contain slashes, so the
// sub-resource patterns use *name wildcards.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	newRoute(http.MethodPost, "/build/prune", handleBuildPrune),

	newRoute(http.MethodPost, "/images/*name/tag", handleImageTag),
	newRoute(http.MethodPost, "/images/*name/push", handleImagePush),
	newRoute(http.MethodGet, "/images/*name/get", func(w http.ResponseWriter, r *http.Request, p routeParams) {
		handleImageGet(w, r, p["name"])
	}),
	newRoute(http.MethodGet, "/images/*name/json", handleImageInspect),
	newRoute(http.MethodDelete, "/images/*name", handleImageDelete),
}

func handleImagesList(w http.ResponseWriter, r *http.Request, _ routeParams) {
	images, err := listDockerImages(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(images)
}

func handleImageCreate(w http.ResponseWriter, r *http.Request, _ routeParams) {
	image := r.URL.Query().Get("fromImage")
	if tag := r.URL.Query().Get("tag"); tag != "" {
		image += ":" + tag
	}
	if image == "" {
		writeJSONError(w, http.StatusBadRequest, "missing fromImage")
		return
	}
	status, err := pullDockerImage(r.Context(), image, parseRegistryAuth(r))
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
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func handleImagePush(w http.ResponseWriter, r *http.Request, p routeParams) {
	w.Header().Set("Content-Type", "application/json")
	if err := pushDockerImage(r.Context(), p["name"], parseRegistryAuth(r), w); err != nil {
		fmt.Fprintf(w, "{\"status\":\"error pushing %s: %s\"}\n", p["name"], err.Error())
		return
	}
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
	if err := removeDockerImage(r.Context(), name, r.URL.Query().Get("force") == "1"); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode([]map[string]string{{"Deleted": name}})
}

func handleImagesPrune(w http.ResponseWriter, r *http.Request, _ routeParams) {
	filters := parseDockerFilters(r.URL.Query().Get("filters"))
	dangling := true
	if filters["dangling"]["false"] {
		dangling = false
	}
	deleted, reclaimed, err := pruneDockerImages(r.Context(), dangling)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
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
		writeJSONError(w, http.StatusInternalServerError, err.Error())
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
