package main

// System-level endpoints: ping/version handshake, info, df, prune, events,
// auth, build. Registered in systemRoutes.

import (
	"encoding/json"
	"net/http"
)

var systemRoutes = []apiRoute{
	newRoute("", "/_ping", handlePing),
	newRoute("", "/version", handleVersion),
	newRoute(http.MethodGet, "/info", func(w http.ResponseWriter, r *http.Request, _ routeParams) {
		handleDockerInfo(w, r)
	}),
	newRoute(http.MethodGet, "/system/df", func(w http.ResponseWriter, r *http.Request, _ routeParams) {
		handleSystemDF(r.Context(), w)
	}),
	newRoute(http.MethodPost, "/system/prune", handleSystemPrune),
	newRoute(http.MethodGet, "/events", func(w http.ResponseWriter, r *http.Request, _ routeParams) {
		handleEvents(w, r)
	}),
	newRoute(http.MethodPost, "/auth", func(w http.ResponseWriter, r *http.Request, _ routeParams) {
		handleAuth(w, r)
	}),
	newRoute(http.MethodPost, "/build", func(w http.ResponseWriter, r *http.Request, _ routeParams) {
		handleBuild(w, r)
	}),
	newRoute(http.MethodPost, "/grpc", func(w http.ResponseWriter, r *http.Request, _ routeParams) {
		handleBuildkitGRPC(w, r)
	}),
	newRoute(http.MethodPost, "/session", func(w http.ResponseWriter, r *http.Request, _ routeParams) {
		handleBuildkitGRPC(w, r)
	}),
}

// handlePing answers the CLI handshake. The docker CLI reads server metadata
// from ping headers instead of extra round-trips: without "Ostype" it fails
// `docker run --device` with "unknown server OS" (client-side parsing).
func handlePing(w http.ResponseWriter, _ *http.Request, _ routeParams) {
	w.Header().Set("Api-Version", dockerAPIVersion)
	w.Header().Set("Ostype", "linux")
	w.Header().Set("Docker-Experimental", "false")
	w.Header().Set("Builder-Version", "2")
	// Real dockerd 23+ reports its integrated buildkit here; buildx checks
	// this header to decide whether the daemon can build via the classic
	// POST /build path (docker driver). Without it docker CLI 29 synthesizes
	// a docker-container "context builder" for every `docker build`, spawning
	// a buildkitd-in-container that cannot resolve registries behind the VZ
	// NAT DNS forwarder.
	w.Header().Set("BuildKit-Version", "v0.32.2")
	w.Header().Set("Server", "Docker/anvil (guest-agent)")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func handleVersion(w http.ResponseWriter, _ *http.Request, _ routeParams) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"Version":       "24.0.0",
		"ApiVersion":    dockerAPIVersion,
		"MinAPIVersion": dockerMinAPIVersion,
		"GitCommit":     "anvil",
		"Os":            "linux",
		"Arch":          "arm64",
		"KernelVersion": "",
		"BuildTime":     "",
	})
}

// handleSystemPrune implements `docker system prune`: containers + networks +
// volumes (only with --volumes, sent as a filter) + images.
func handleSystemPrune(w http.ResponseWriter, r *http.Request, _ routeParams) {
	filters := parseDockerFilters(r.URL.Query().Get("filters"))
	withVolumes := filters["volumes"]["true"]
	stopped, _, _ := pruneDockerContainers(r.Context())
	nets, _ := pruneDockerNetworks(r.Context())
	var vols []string
	if withVolumes {
		vols, _, _ = pruneDockerVolumes(r.Context())
	}
	_, reclaimed, _ := pruneDockerImages(r.Context(), false)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ContainersDeleted": stopped,
		"NetworksDeleted":   nets,
		"VolumesDeleted":    vols,
		"SpaceReclaimed":    reclaimed,
	})
}
