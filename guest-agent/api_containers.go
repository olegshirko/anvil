package main

// Container lifecycle endpoints: list/create/inspect/delete plus the
// per-container sub-resources (start, stop, kill, logs, exec, ...).
// Registered in containerRoutes.

import (
	"encoding/json"
	"fmt"
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
	newRoute(http.MethodPost, "/containers/:id/update", handleContainerUpdate),
	newRoute(http.MethodPost, "/containers/:id/pause", handleContainerPause),
	newRoute(http.MethodPost, "/containers/:id/unpause", handleContainerUnpause),
	newRoute(http.MethodGet, "/containers/:id/top", func(w http.ResponseWriter, r *http.Request, p routeParams) {
		handleContainerTop(r.Context(), w, p["id"])
	}),
	newRoute(http.MethodGet, "/containers/:id/stats", func(w http.ResponseWriter, r *http.Request, p routeParams) {
		// Docker streams unless told otherwise (docker-py and docker-java
		// omit the parameter).
		q := r.URL.Query()
		handleContainerStats(r.Context(), w, p["id"], !q.Has("stream") || queryBool(q, "stream"))
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
	newRoute(http.MethodGet, "/containers/:id/export", handleContainerExport),
	newRoute(http.MethodGet, "/containers/:id/changes", handleContainerChanges),

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
	all := queryBool(r.URL.Query(), "all")
	filters := parseDockerFilters(r.URL.Query().Get("filters"))
	if err := validateFilterKeys(filters, containerFilterKeys); err != nil {
		writeAPIError(w, err, http.StatusBadRequest)
		return
	}
	// A status filter selects among all containers (docker ps -f
	// status=exited needs no -a), as in Docker.
	if len(filters["status"]) > 0 {
		all = true
	}
	before, since := filters["before"], filters["since"]
	delete(filters, "before")
	delete(filters, "since")
	containers, err := listDockerContainers(r.Context(), filters)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	if len(before) > 0 || len(since) > 0 {
		everyone, err := listDockerContainers(r.Context(), nil)
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError)
			return
		}
		if containers, err = filterByCreatedRef(containers, everyone, before, since); err != nil {
			writeAPIError(w, err, http.StatusBadRequest)
			return
		}
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
	pf, err := newPruneFilter(parseDockerFilters(r.URL.Query().Get("filters")))
	if err != nil {
		writeAPIError(w, err, http.StatusBadRequest)
		return
	}
	deleted, reclaimed, err := pruneDockerContainers(r.Context(), pf)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
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
	platform, perr := resolveRequestedPlatform(r.URL.Query().Get("platform"))
	if perr != nil {
		writeJSONError(w, http.StatusBadRequest, perr.Error())
		return
	}
	var req dockerCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateNetworkMode(req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := r.URL.Query().Get("name")
	id, platformWarnings, err := createDockerContainer(r.Context(), req, name, platform, parseRegistryAuth(r))
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dockerCreateResponse{
		Id:       id,
		Warnings: append(unsupportedHostConfigWarnings(req.HostConfig), platformWarnings...),
	})
}

func handleContainerStart(w http.ResponseWriter, r *http.Request, p routeParams) {
	if err := startDockerContainer(r.Context(), p["id"]); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	// A user start puts the restart policy back in force (the monitor's
	// restarts call startDockerContainer directly and must not re-arm).
	if ns, cid, _, err := resolveDockerID(r.Context(), p["id"]); err == nil {
		restarts.rearm(dockerID(ns, cid))
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerStop(w http.ResponseWriter, r *http.Request, p routeParams) {
	timeout := -1 // the container's StopTimeout, else 10 s
	if t := r.URL.Query().Get("t"); t != "" {
		if v, err := strconv.Atoi(t); err == nil {
			timeout = v
		}
	}
	if err := stopDockerContainer(r.Context(), p["id"], timeout); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
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
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerRestart(w http.ResponseWriter, r *http.Request, p routeParams) {
	timeout := -1 // as docker stop: no -t means the container's grace period
	if t := r.URL.Query().Get("t"); t != "" {
		if v, err := strconv.Atoi(t); err == nil {
			timeout = v
		}
	}
	if err := restartDockerContainer(r.Context(), p["id"], timeout); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerRename(w http.ResponseWriter, r *http.Request, p routeParams) {
	newName := strings.TrimPrefix(r.URL.Query().Get("name"), "/")
	if err := renameDockerContainer(r.Context(), p["id"], newName); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerPause(w http.ResponseWriter, r *http.Request, p routeParams) {
	if err := pauseDockerContainer(r.Context(), p["id"], true); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleContainerUnpause(w http.ResponseWriter, r *http.Request, p routeParams) {
	if err := pauseDockerContainer(r.Context(), p["id"], false); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
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
		writeAPIError(w, err, http.StatusNotFound)
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
			writeAPIError(w, err, http.StatusInternalServerError)
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
	q := r.URL.Query()
	force := queryBool(q, "force")
	removeVolumes := queryBool(q, "v")
	if err := deleteDockerContainer(r.Context(), p["id"], force, removeVolumes); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError)
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

// filterByCreatedRef applies before=/since=: containers created before or
// after the referenced one (by name or ID prefix).
func filterByCreatedRef(list, everyone []dockerContainerSummary, before, since map[string]bool) ([]dockerContainerSummary, error) {
	createdOf := func(ref string) (int64, error) {
		ref = strings.TrimPrefix(ref, "/")
		for _, c := range everyone {
			if strings.TrimPrefix(c.Names[0], "/") == ref || strings.HasPrefix(c.Id, ref) {
				return c.created, nil
			}
		}
		return 0, fmt.Errorf("No such container: %s", ref)
	}
	out := list[:0]
	for _, c := range list {
		keep := true
		for ref := range before {
			t, err := createdOf(ref)
			if err != nil {
				return nil, err
			}
			keep = keep && c.created < t
		}
		for ref := range since {
			t, err := createdOf(ref)
			if err != nil {
				return nil, err
			}
			keep = keep && c.created > t
		}
		if keep {
			out = append(out, c)
		}
	}
	return out, nil
}
