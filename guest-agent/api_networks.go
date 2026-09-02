package main

// Network endpoints: list/create/inspect/delete/connect/disconnect/prune.
// Registered in networkRoutes.

import (
	"encoding/json"
	"net/http"
)

var networkRoutes = []apiRoute{
	newRoute(http.MethodGet, "/networks", handleNetworksList),
	newRoute(http.MethodPost, "/networks/create", handleNetworkCreate),
	newRoute(http.MethodPost, "/networks/prune", handleNetworksPrune),
	newRoute(http.MethodGet, "/networks/:id", handleNetworkInspect),
	newRoute(http.MethodDelete, "/networks/:id", handleNetworkDelete),
	newRoute(http.MethodPost, "/networks/:id/connect", handleNetworkConnect),
	newRoute(http.MethodPost, "/networks/:id/disconnect", handleNetworkDisconnect),
}

func handleNetworksList(w http.ResponseWriter, r *http.Request, _ routeParams) {
	filters := parseDockerFilters(r.URL.Query().Get("filters"))
	networks, err := listDockerNetworks(r.Context(), filters)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(networks)
}

func handleNetworkCreate(w http.ResponseWriter, r *http.Request, _ routeParams) {
	var req dockerNetworkCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	nw, err := createDockerNetwork(r.Context(), req)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"Id": nw.Id, "Warning": ""})
}

func handleNetworkInspect(w http.ResponseWriter, r *http.Request, p routeParams) {
	nw, err := inspectDockerNetwork(r.Context(), p["id"])
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(nw)
}

func handleNetworkDelete(w http.ResponseWriter, r *http.Request, p routeParams) {
	if err := removeDockerNetwork(r.Context(), p["id"]); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleNetworkConnect(w http.ResponseWriter, r *http.Request, p routeParams) {
	var req struct {
		Container string `json:"Container"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Container == "" {
		writeJSONError(w, http.StatusBadRequest, "missing Container")
		return
	}
	if err := connectContainerNetwork(p["id"], req.Container); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleNetworkDisconnect(w http.ResponseWriter, r *http.Request, p routeParams) {
	var req struct {
		Container string `json:"Container"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Container == "" {
		writeJSONError(w, http.StatusBadRequest, "missing Container")
		return
	}
	if err := disconnectContainerNetwork(p["id"], req.Container); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleNetworksPrune(w http.ResponseWriter, r *http.Request, _ routeParams) {
	deleted, err := pruneDockerNetworks(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string][]string{"NetworksDeleted": deleted})
}
