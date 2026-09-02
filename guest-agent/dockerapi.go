package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/mdlayher/vsock"
)

// Advertise a modern API version: the CLI downgrades to the server's
// version (ping header), and compose requires >= 1.40. The handlers only
// implement a subset, but stripAPIVersion routes any /vX.Y path the same.
const dockerAPIVersion = "1.51"
const dockerMinAPIVersion = "1.24"

// writeDockerStream writes a Docker multiplexed stream frame.
// streamType: 0=stdin, 1=stdout, 2=stderr.
// unixToRFC3339 converts a unix-seconds timestamp (the form the docker CLI
// sends for logs since/until) to RFC3339. Returns "" when the input is not
// a plain number (RFC3339 or a relative duration pass through as is).
func unixToRFC3339(raw string) string {
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return ""
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC().Format(time.RFC3339Nano)
}

func writeDockerStream(w io.Writer, streamType byte, data []byte) error {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:], uint32(len(data)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// stripAPIVersion removes a leading /v1.XX prefix so the same handlers work
// for any Docker client API version.
func stripAPIVersion(path string) string {
	if idx := strings.Index(path, "/v1."); idx == 0 {
		if slash := strings.Index(path[4:], "/"); slash != -1 {
			return path[4+slash:]
		}
	}
	return path
}

// parseDockerFilters parses the URL-encoded JSON filter blob sent by Docker CLI
// into a map of filter key -> allowed values. Docker clients use either
// {"label":{"k=v":true}} or {"label":["k=v"]}; both are accepted.
func parseDockerFilters(query string) map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	if query == "" {
		return result
	}
	decoded, err := url.QueryUnescape(query)
	if err != nil {
		return result
	}

	// First try the canonical Docker object form: {"label":{"k=v":true}}.
	var objFilters map[string]map[string]bool
	if err := json.Unmarshal([]byte(decoded), &objFilters); err == nil {
		for k, m := range objFilters {
			if result[k] == nil {
				result[k] = make(map[string]bool)
			}
			for v := range m {
				result[k][v] = true
			}
		}
		return result
	}

	// Fall back to the array form: {"label":["k=v"]}.
	var arrFilters map[string][]string
	if err := json.Unmarshal([]byte(decoded), &arrFilters); err != nil {
		return result
	}
	for k, values := range arrFilters {
		if result[k] == nil {
			result[k] = make(map[string]bool)
		}
		for _, v := range values {
			result[k][v] = true
		}
	}
	return result
}

// matchesLabelFilters reports whether the given labels satisfy the "label"
// filters from parseDockerFilters. An empty filters map matches everything.
// Docker filters are AND-ed: every specified label constraint must match.
func matchesLabelFilters(labels map[string]string, filters map[string]map[string]bool) bool {
	if len(filters) == 0 {
		return true
	}
	if labelFilters, ok := filters["label"]; ok && len(labelFilters) > 0 {
		for constraint := range labelFilters {
			matched := false
			if idx := strings.Index(constraint, "="); idx >= 0 {
				key, value := constraint[:idx], constraint[idx+1:]
				if labels[key] == value {
					matched = true
				}
			} else if _, hasKey := labels[constraint]; hasKey {
				matched = true
			}
			if !matched {
				return false
			}
		}
		return true
	}
	return true
}

// streamTaskLogTo replays the container's json-file log (and optionally
// follows it), writing output as Docker multiplexed stream frames until the
// container exits or the writer fails.
func streamTaskLogTo(out io.Writer, ns, id string, follow bool) {
	streamTaskLogToTTY(out, ns, id, follow, false)
}

// streamTaskLogToTTY streams the task log; with tty=true the bytes go out raw
// (Docker sends TTY output unmultiplexed — a mux header would corrupt the
// client terminal), otherwise in the 8-byte-header multiplexed format.
func streamTaskLogToTTY(out io.Writer, ns, id string, follow bool, tty bool) {
	flusher, _ := out.(http.Flusher)
	emit := func(stream byte, line []byte) {
		var writeErr error
		if tty {
			_, writeErr = out.Write(line)
		} else {
			writeErr = writeDockerStream(out, stream, line)
		}
		if writeErr != nil {
			panic(errWriteFailed) // unwind readTaskLog's follow loop
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	defer func() {
		recover() //nolint:errcheck — errWriteFailed unwinds the follow loop
	}()

	logPath := containerLogPath(ns, id)

	n := 0
	emitWrap := func(stream byte, line []byte) {
		n++
		emit(stream, line)
	}
	err := readTaskLog(logPath, logReadOptions{follow: follow, tail: -1, stop: taskExitedStopper(ns, id, follow)}, emitWrap)
	debugLog("attach %s: readTaskLog done err=%v emitted=%d tty=%v follow=%v", id, err, n, tty, follow)
}

// taskExitedStopper ends a log follow stream when the container's task has
// exited. Follow mode must not end while the container is merely "created"
// (docker run attaches BEFORE start), so a task that never ran keeps the
// stream open until it does.
func taskExitedStopper(ns, id string, follow bool) func() bool {
	wasRunning := false
	return func() bool {
		if !follow {
			return true
		}
		running, status, ok := containerTaskState(context.Background(), ns, id)
		if ok && running {
			wasRunning = true
			return false
		}
		if !wasRunning && (!ok || status == "" || status == "created" || status == "paused") {
			return false // created but not started yet; keep waiting
		}
		return true
	}
}

// errWriteFailed unwinds readTaskLog when the HTTP client disconnects.
var errWriteFailed = errors.New("write failed")

// handleAttach hijacks the HTTP connection and streams container output using
// Docker's raw-stream multiplexing format. It replays the json-file task log
// first, then follows it only if the container is still running and the
// client asked for a stream. This avoids the race where short-lived
// containers exit before attach is called.
func handleAttach(w http.ResponseWriter, r *http.Request, id string) {
	ns, containerdID, _, err := resolveDockerID(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	// Keep AutoRemove from deleting the container (and its logs) while we
	// are replaying output to the client.
	did := dockerID(ns, containerdID)
	attachBegin(did)
	defer attachEnd(did)

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "hijacking not supported")
		return
	}

	conn, bufrw, err := hj.Hijack()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer conn.Close()

	fmt.Fprintf(bufrw, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	if err := bufrw.Flush(); err != nil {
		return
	}
	// Drain any stdin the client sends so the half-duplex pipe does not block.
	// Use the raw connection, not bufrw, because bufrw is used for writing below
	// and bufio types are not safe for concurrent read/write.
	go func() {
		io.Copy(io.Discard, conn)
	}()

	// Replay existing logs then follow if the container is still running.
	// TTY containers stream raw bytes (no docker mux headers).
	streamTaskLogToTTY(bufrw, ns, containerdID, true, getContainerTTY(did))

	// Ensure all buffered output reaches the client before closing.
	bufrw.Flush()
	time.Sleep(100 * time.Millisecond)
}

// handleLogs streams container logs using Docker's multiplexed stream format.
func handleLogs(w http.ResponseWriter, r *http.Request, id string) {
	ns, containerdID, _, err := resolveDockerID(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}

	opts := logReadOptions{tail: -1}
	q := r.URL.Query()
	opts.timestamps = q.Get("timestamps") == "1" || q.Get("timestamps") == "true"
	opts.follow = q.Get("follow") == "1" || q.Get("follow") == "true"
	// since/until: the docker CLI sends unix timestamps (possibly with
	// fractional seconds); RFC3339 is accepted as a fallback.
	if v := q.Get("since"); v != "" {
		if ts := parseLogTime(v); !ts.IsZero() {
			opts.since = ts
		}
	}
	if v := q.Get("until"); v != "" {
		if ts := parseLogTime(v); !ts.IsZero() {
			opts.until = ts
		}
	}
	if v := q.Get("tail"); v != "" && v != "all" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			opts.tail = n
		}
	}

	// Log driver "none" discards: there is deliberately no log file, so
	// logs must return an empty stream (and an empty follow) instead of
	// waiting 30s for a file that never appears.
	if meta, merr := loadContainerMeta(ns, containerdID); merr == nil && meta.HostConfig != nil &&
		meta.HostConfig.LogConfig.Type == "none" {
		if opts.follow {
			<-r.Context().Done()
		}
		return
	}

	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	flusher, _ := w.(http.Flusher)
	emit := func(stream byte, line []byte) {
		if writeDockerStream(w, stream, line) != nil {
			panic(errWriteFailed) // unwind readTaskLog's follow loop
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	defer func() {
		recover() //nolint:errcheck — errWriteFailed unwinds the follow loop
	}()
	// A followed container may stay quiet forever; the follow loop then ends
	// on client disconnect (context canceled) or container exit, not on a
	// write error alone.
	baseStop := taskExitedStopper(ns, containerdID, opts.follow)
	opts.stop = func() bool {
		return r.Context().Err() != nil || baseStop()
	}
	err = readTaskLog(containerLogPath(ns, containerdID), opts, emit)
	debugLog("logs %s: readTaskLog done err=%v follow=%v ctxErr=%v", containerdID, err, opts.follow, r.Context().Err())
}

// parseLogTime accepts unix seconds (optionally fractional) and RFC3339.
func parseLogTime(v string) time.Time {
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Unix(int64(f), int64((f-float64(int64(f)))*float64(time.Second)))
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// newDockerAPIHandler builds the HTTP handler for the Docker-compatible API:
// request logging, connection teardown semantics and the route table in
// router.go. Separate from runDockerAPIServer so tests can exercise the full
// HTTP layer with httptest (no vsock, no containerd).
func newDockerAPIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := stripAPIVersion(r.URL.Path)
		log.Printf("[docker-api] %s %s", r.Method, path)

		// Docker CLI reuses a single connection for multiple API calls. Our
		// vsock proxy maps one host connection to one guest connection, so a
		// blocking /wait would deadlock the next /start on the same connection.
		// Force the client to open a new connection after each response.
		w.Header().Set("Connection", "close")

		dispatchDockerAPI(w, r, path)
	})
	return mux
}

// runDockerAPIServer starts the Docker-compatible HTTP server on vsock:1025.
// It waits for boot finalize (containerd up + stale-container cleanup) before
// listening; see runBootFinalize. Endpoint logic lives in the api_*.go
// handler files and is wired up by the route table in router.go.
func runDockerAPIServer(containerdReady <-chan struct{}) {
	mux := newDockerAPIHandler()

	// Container operations must not race the boot-time stale-container
	// cleanup (and need containerd reachable); the host-side docker proxy
	// retries the vsock connect, so waiting here serializes cleanly without
	// delaying the status/health control channel.
	select {
	case <-containerdReady:
	case <-time.After(10 * time.Second):
		log.Printf("[docker-api] boot finalize timeout, serving anyway")
	}

	l, err := vsock.Listen(dockerAPIPort, nil)
	if err != nil {
		log.Printf("[docker-api] listen: %v", err)
		return
	}
	log.Printf("[docker-api] listening on vsock port %d", dockerAPIPort)
	srv := &http.Server{Handler: mux}
	if err := srv.Serve(l); err != nil {
		log.Printf("[docker-api] serve: %v", err)
	}
}

// parseResizeQuery extracts the h/w terminal dimensions from a resize request.
func parseResizeQuery(r *http.Request) (uint32, uint32, error) {
	q := r.URL.Query()
	h, herr := strconv.ParseUint(q.Get("h"), 10, 32)
	w, werr := strconv.ParseUint(q.Get("w"), 10, 32)
	if herr != nil || werr != nil || h == 0 || w == 0 {
		return 0, 0, fmt.Errorf("invalid resize dimensions (h=%q w=%q)", q.Get("h"), q.Get("w"))
	}
	return uint32(w), uint32(h), nil
}

// handleContainerResize implements POST /containers/{id}/resize — resizes the
// container task's TTY (SIGWINCH propagates through the shim to the process).
func handleContainerResize(w http.ResponseWriter, r *http.Request, id string) {
	cols, rows, err := parseResizeQuery(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	ns, containerdID, _, err := resolveDockerID(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	cl, err := pc.get(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	nsCtx := namespaces.WithNamespace(r.Context(), ns)
	container, err := cl.LoadContainer(nsCtx, containerdID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	task, err := container.Task(nsCtx, nil)
	if err != nil {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("no running task: %s", err.Error()))
		return
	}
	if err := task.Resize(nsCtx, cols, rows); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"Message": ""}) //nolint:errcheck
}

// handleExecResize implements POST /exec/{id}/resize — resizes the exec
// process's TTY while the exec session is running.
func handleExecResize(w http.ResponseWriter, r *http.Request, id string) {
	cols, rows, err := parseResizeQuery(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	spec := execs.get(id)
	if spec == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("No such exec instance: %s", id))
		return
	}
	process := spec.currentProcess()
	if process == nil {
		writeJSONError(w, http.StatusConflict, "exec is not running")
		return
	}
	nsCtx := namespaces.WithNamespace(context.Background(), spec.Namespace)
	if err := process.Resize(nsCtx, cols, rows); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"Message": ""}) //nolint:errcheck
}

// pruneDockerContainers removes stopped/created containers and returns their
// Docker IDs. It mirrors the response shape of POST /containers/prune.
func pruneDockerContainers(ctx context.Context) ([]string, int64, error) {
	containers, err := listDockerContainers(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	var deleted []string
	for _, c := range containers {
		if c.State == "running" {
			continue
		}
		if err := deleteDockerContainer(ctx, c.Id, true); err != nil {
			log.Printf("[docker-api] prune container %s: %v", c.Id, err)
			continue
		}
		deleted = append(deleted, c.Id)
	}
	return deleted, 0, nil
}

// pruneDockerNetworks removes unused non-default networks.
func pruneDockerNetworks(ctx context.Context) ([]string, error) {
	networks, err := listDockerNetworks(ctx, nil)
	if err != nil {
		return nil, err
	}
	containers, err := listDockerContainers(ctx, nil)
	if err != nil {
		return nil, err
	}
	inUse := map[string]struct{}{}
	for _, c := range containers {
		labels := c.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		netsJSON := labels[labelNetworks]
		if netsJSON == "" {
			continue
		}
		var nets []string
		if err := json.Unmarshal([]byte(netsJSON), &nets); err != nil {
			continue
		}
		for _, n := range nets {
			inUse[n] = struct{}{}
		}
	}

	protected := map[string]struct{}{
		"bridge": {},
		"host":   {},
		"none":   {},
	}
	var deleted []string
	for _, nw := range networks {
		if _, ok := protected[nw.Name]; ok {
			continue
		}
		if _, ok := inUse[nw.Name]; ok {
			continue
		}
		if err := removeDockerNetwork(ctx, nw.Name); err != nil {
			log.Printf("[docker-api] prune network %s: %v", nw.Name, err)
			continue
		}
		deleted = append(deleted, nw.Name)
	}
	return deleted, nil
}

// pruneDockerVolumes removes volumes not referenced by any container.
func pruneDockerVolumes(ctx context.Context) ([]string, int64, error) {
	volumes, err := listDockerVolumes(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	containers, err := listDockerContainers(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	inUse := map[string]struct{}{}
	for _, c := range containers {
		labels := c.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		// Anonymous volumes.
		anonJSON := labels[labelAnonymousVolumes]
		if anonJSON != "" {
			var vols []string
			if err := json.Unmarshal([]byte(anonJSON), &vols); err == nil {
				for _, v := range vols {
					inUse[v] = struct{}{}
				}
			}
		}
		// Named mounts.
		mountsJSON := labels[labelMounts]
		if mountsJSON != "" {
			var mounts []struct {
				Type string `json:"Type"`
				Name string `json:"Name"`
			}
			if err := json.Unmarshal([]byte(mountsJSON), &mounts); err == nil {
				for _, m := range mounts {
					if m.Type == "volume" && m.Name != "" {
						inUse[m.Name] = struct{}{}
					}
				}
			}
		}
	}

	var deleted []string
	for _, v := range volumes {
		if _, ok := inUse[v.Name]; ok {
			continue
		}
		if err := removeDockerVolume(ctx, v.Name); err != nil {
			log.Printf("[docker-api] prune volume %s: %v", v.Name, err)
			continue
		}
		deleted = append(deleted, v.Name)
	}
	return deleted, 0, nil
}

// writeJSONError writes a Docker-API style error body. Hand-concatenated
// JSON breaks whenever the message itself contains quotes (e.g. %q-formatted
// validator errors), so this always marshals properly.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}
