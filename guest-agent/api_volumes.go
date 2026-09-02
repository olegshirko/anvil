package main

// Volume endpoints: list/create/inspect/delete/prune. Registered in
// volumeRoutes.

import (
	"encoding/json"
	"net/http"
)

var volumeRoutes = []apiRoute{
	newRoute(http.MethodGet, "/volumes", handleVolumesList),
	newRoute(http.MethodPost, "/volumes/create", handleVolumeCreate),
	newRoute(http.MethodPost, "/volumes/prune", handleVolumesPrune),
	newRoute(http.MethodGet, "/volumes/:name", handleVolumeInspect),
	newRoute(http.MethodDelete, "/volumes/:name", handleVolumeDelete),
}

func handleVolumesList(w http.ResponseWriter, r *http.Request, _ routeParams) {
	filters := parseDockerFilters(r.URL.Query().Get("filters"))
	volumes, err := listDockerVolumes(r.Context(), filters)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dockerVolumeList{Volumes: volumes})
}

func handleVolumeCreate(w http.ResponseWriter, r *http.Request, _ routeParams) {
	var req dockerVolumeCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	vol, err := createDockerVolume(r.Context(), req)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(vol)
}

func handleVolumeInspect(w http.ResponseWriter, r *http.Request, p routeParams) {
	vol, err := inspectDockerVolume(r.Context(), p["name"])
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(vol)
}

func handleVolumeDelete(w http.ResponseWriter, r *http.Request, p routeParams) {
	if err := removeDockerVolume(r.Context(), p["name"]); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleVolumesPrune(w http.ResponseWriter, r *http.Request, _ routeParams) {
	deleted, reclaimed, err := pruneDockerVolumes(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"VolumesDeleted": deleted,
		"SpaceReclaimed": reclaimed,
	})
}
