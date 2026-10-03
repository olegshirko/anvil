package main

// Network endpoints: list/create/inspect/delete/connect/disconnect/prune.
// Registered in networkRoutes.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	if err := validateFilterKeys(filters, networkFilterKeys); err != nil {
		writeAPIError(w, err, http.StatusBadRequest)
		return
	}
	networks, err := listDockerNetworks(r.Context(), filters)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	networks = filterNetworks(r.Context(), networks, filters)
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
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	// Unsupported options are said, not silently dropped.
	var warnings []string
	if !req.ipv6Enabled() && hasIPv6Subnet(req.IPAM.Config) {
		warnings = append(warnings, "IPv6 subnet ignored: IPv6 is not enabled on the network (--ipv6)")
	}
	if req.Driver != "" && req.Driver != "bridge" {
		warnings = append(warnings, fmt.Sprintf("driver %q is not supported: created a bridge network", req.Driver))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"Id": nw.Id, "Warning": strings.Join(warnings, "; ")})
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
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleNetworksPrune(w http.ResponseWriter, r *http.Request, _ routeParams) {
	pf, err := newPruneFilter(parseDockerFilters(r.URL.Query().Get("filters")))
	if err != nil {
		writeAPIError(w, err, http.StatusBadRequest)
		return
	}
	deleted, err := pruneDockerNetworks(r.Context(), pf)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string][]string{"NetworksDeleted": deleted})
}
