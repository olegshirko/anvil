package main

// Container lifecycle endpoints: list/create/inspect/delete plus the
// per-container sub-resources (start, stop, kill, logs, exec, ...).
// Registered in containerRoutes.

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

var containerRoutes = []apiRoute{
	newRoute("", "/containers/json", handleContainersList),
	newRoute(http.MethodPost, "/containers/prune", handleContainersPrune),
	newRoute(http.MethodPost, "/containers/create", handleContainerCreate),

	newRoute(http.MethodPost, "/containers/:id/start", handleContainerStart),
	newRoute(http.MethodPost, "/containers/:id/stop", handleContainerStop),
	newRoute(http.MethodPost, "/containers/:id/kill", handleContainerKill),
	newRoute(http.MethodPost, "/containers/:id/restart", handleContainerRestart),
	newRoute(http.MethodPost, "/containers/:id/wait", func(w http.ResponseWriter, r *http.Request, p routeParams) {
		handleContainerWait(w, r, p["id"])
	}),
	newRoute(http.MethodPost, "/containers/:id/rename", handleContainerRename),
	newRoute(http.MethodPost, "/containers/:id/pause", handleContainerPause),
	newRoute(http.MethodPost, "/containers/:id/unpause", handleContainerUnpause),
	newRoute(http.MethodGet, "/containers/:id/top", func(w http.ResponseWriter, r *http.Request, p routeParams) {
		handleContainerTop(r.Context(), w, p["id"])
	}),
	newRoute(http.MethodGet, "/containers/:id/stats", func(w http.ResponseWriter, r *http.Request, p routeParams) {
		handleContainerStats(r.Context(), w, p["id"], r.URL.Query().Get("stream") == "1")
	}),
	newRoute(http.MethodPost, "/containers/:id/resize", func(w http.ResponseWriter, r *http.Request, p routeParams) {
		handleContainerResize(w, r, p["id"])
	}),
	newRoute(http.MethodPost, "/containers/:id/attach", func(w http.ResponseWriter, r *http.Request, p routeParams) {
		handleAttach(w, r, p["id"])
	}),
	newRoute(http.MethodGet, "/containers/:id/logs", func(w http.ResponseWriter, r *http.Request, p routeParams) {
		handleLogs(w, r, p["id"])
	}),
	newRoute(http.MethodHead, "/containers/:id/archive", containerArchive),
	newRoute(http.MethodGet, "/containers/:id/archive", containerArchive),
	newRoute(http.MethodPut, "/containers/:id/archive", containerArchive),
	newRoute(http.MethodPost, "/containers/:id/exec", handleContainerExecCreate),

	newRoute(http.MethodPost, "/exec/:id/start", handleContainerExecStart),
	newRoute(http.MethodGet, "/exec/:id/json", handleContainerExecInspect),
	newRoute(http.MethodPost, "/exec/:id/resize", func(w http.ResponseWriter, r *http.Request, p routeParams) {
		handleExecResize(w, r, p["id"])
	}),

	newRoute(http.MethodDelete, "/containers/:id", handleContainerDelete),
	newRoute(http.MethodGet, "/containers/:id/json", handleContainerInspect),
}

func containerArchive(w http.ResponseWriter, r *http.Request, p routeParams) {
	handleContainerArchive(w, r, p["id"])
}

func handleContainersList(w http.ResponseWriter, r *http.Request, _ routeParams) {
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	filters := parseDockerFilters(r.URL.Query().Get("filters"))
	containers, err := listDockerContainers(r.Context(), filters)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !all {
		running := make([]dockerContainerSummary, 0)
		for _, c := range containers {
			if c.State == "running" {
				running = append(running, c)
			}
		}
		containers = running
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(containers)
}

func handleContainersPrune(w http.ResponseWriter, r *http.Request, _ routeParams) {
	deleted, reclaimed, err := pruneDockerContainers(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ContainersDeleted": deleted,
		"SpaceReclaimed":    reclaimed,
	})
}

func handleContainerCreate(w http.ResponseWriter, r *http.Request, _ routeParams) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if verr := validateHostConfig(body); verr != nil {
		writeJSONError(w, http.StatusBadRequest, verr.Error())
		return
	}
	if perr := validateCreatePlatform(r); perr != nil {
		writeJSONError(w, http.StatusBadRequest, perr.Error())
		return
	}
	var req dockerCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := r.URL.Query().Get("name")
	id, err := createDockerContainer(r.Context(), req, name, parseRegistryAuth(r))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dockerCreateResponse{
		Id:       id,
		Warnings: unsupportedHostConfigWarnings(req.HostConfig),
	})
}

func handleContainerStart(w http.ResponseWriter, r *http.Request, p routeParams) {
	if err := startDockerContainer(r.Context(), p["id"]); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerStop(w http.ResponseWriter, r *http.Request, p routeParams) {
	timeout := 10
	if t := r.URL.Query().Get("t"); t != "" {
		if v, err := strconv.Atoi(t); err == nil {
			timeout = v
		}
	}
	if err := stopDockerContainer(r.Context(), p["id"], timeout); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerKill(w http.ResponseWriter, r *http.Request, p routeParams) {
	signal := r.URL.Query().Get("signal")
	if signal == "" {
		signal = "SIGKILL"
	}
	if err := killDockerContainer(r.Context(), p["id"], signal); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerRestart(w http.ResponseWriter, r *http.Request, p routeParams) {
	timeout := 0
	if t := r.URL.Query().Get("t"); t != "" {
		if v, err := strconv.Atoi(t); err == nil {
			timeout = v
		}
	}
	if err := restartDockerContainer(r.Context(), p["id"], timeout); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerRename(w http.ResponseWriter, r *http.Request, p routeParams) {
	newName := strings.TrimPrefix(r.URL.Query().Get("name"), "/")
	if err := renameDockerContainer(r.Context(), p["id"], newName); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerPause(w http.ResponseWriter, r *http.Request, p routeParams) {
	if err := pauseDockerContainer(r.Context(), p["id"], true); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerUnpause(w http.ResponseWriter, r *http.Request, p routeParams) {
	if err := pauseDockerContainer(r.Context(), p["id"], false); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerExecCreate(w http.ResponseWriter, r *http.Request, p routeParams) {
	var req dockerExecCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := createDockerExec(r.Context(), p["id"], req)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dockerExecCreateResponse{Id: id})
}

func handleContainerExecStart(w http.ResponseWriter, r *http.Request, p routeParams) {
	var req dockerExecStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Detach {
		if err := startDetachedExec(p["id"]); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	handleExecStart(w, r, p["id"])
}

func handleContainerExecInspect(w http.ResponseWriter, _ *http.Request, p routeParams) {
	info, err := inspectDockerExec(p["id"])
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func handleContainerDelete(w http.ResponseWriter, r *http.Request, p routeParams) {
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	if err := deleteDockerContainer(r.Context(), p["id"], force); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerInspect(w http.ResponseWriter, r *http.Request, p routeParams) {
	inspect, err := inspectDockerContainer(r.Context(), p["id"])
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(inspect)
}
