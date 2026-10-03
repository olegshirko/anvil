package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
	}
	for constraint := range filters["label!"] {
		if key, value, hasValue := strings.Cut(constraint, "="); hasValue {
			if v, ok := labels[key]; ok && v == value {
				return false
			}
		} else if _, ok := labels[constraint]; ok {
			return false
		}
	}
	return true
}

// attachStreamOptions are the attach query parameters that shape the output.
type attachStreamOptions struct {
	logs, stream   bool
	stdout, stderr bool
	detached       func() bool // true once the client typed the detach keys
}

// streamTaskLogToTTY streams the task log; with tty=true the bytes go out raw
// (Docker sends TTY output unmultiplexed — a mux header would corrupt the
// client terminal), otherwise in the 8-byte-header multiplexed format.
// logs replays what was written before the attach; without it the stream
// starts at the current end (`docker start -a` of an exited container shows
// only the new run). stream follows the log until the task exits.
func streamTaskLogToTTY(out io.Writer, ns, id string, tty bool, ao attachStreamOptions) {
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
	quiet := stopQuietDefault
	if tty {
		quiet = stopQuietTTY
	}
	tail := -1
	if !ao.logs {
		tail = 0
	}
	baseStop := taskExitedStopper(ns, id, ao.stream)
	stop := baseStop
	if ao.detached != nil {
		stop = func() bool { return ao.detached() || baseStop() }
	}
	opts := logReadOptions{follow: ao.stream, tail: tail, stop: stop, stopQuiet: quiet,
		stream: func(s byte) bool { return (s == 1 && ao.stdout) || (s == 2 && ao.stderr) }}
	err := readTaskLog(logPath, opts, emitWrap)
	debugLog("attach %s: readTaskLog done err=%v emitted=%d tty=%v follow=%v", id, err, n, tty, ao.stream)
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

// handleAttach hijacks the HTTP connection and streams container output
// using Docker's raw-stream multiplexing format, and feeds the client's
// stdin to an OpenStdin container. Output is the json-file task log: with
// logs=1 it is replayed first; with stream=1 it is followed until the task
// exits. docker run attaches before start, so following from the current
// end (an empty log) still shows a short-lived container's whole output.
func handleAttach(w http.ResponseWriter, r *http.Request, id string) {
	ns, containerdID, _, err := resolveDockerID(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	q := r.URL.Query()
	ao := attachStreamOptions{
		logs:   queryBool(q, "logs"),
		stream: queryBool(q, "stream"),
		stdout: queryBool(q, "stdout"),
		stderr: queryBool(q, "stderr"),
	}
	if !q.Has("stdout") && !q.Has("stderr") {
		ao.stdout, ao.stderr = true, true
	}
	if !q.Has("stream") && !q.Has("logs") {
		ao.logs, ao.stream = true, true // bare attach from older clients
	}
	did := dockerID(ns, containerdID)
	tty := getContainerTTY(did)
	meta, _ := loadContainerMeta(ns, containerdID)
	var stdin *containerStdin
	if queryBool(q, "stdin") && meta != nil && meta.OpenStdin {
		// A stopped container is about to run again (start -ai, run):
		// its input belongs to the next run's FIFO.
		running, _, _ := containerTaskState(r.Context(), ns, containerdID)
		if stdin, err = openContainerStdin(ns, containerdID, !running); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	detachKeys, err := parseDetachKeys(q.Get("detachKeys"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Keep AutoRemove from deleting the container (and its logs) while we
	// are replaying output to the client.
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

	if err := writeHijackHeader(bufrw, r, tty); err != nil {
		return
	}
	// Client stdin: into the container's stdin, or drained so the client
	// never blocks. Read from the raw connection (plus whatever the HTTP
	// server already buffered), not bufrw's writer side, which the output
	// goroutine below uses.
	var detached atomic.Bool
	in := io.MultiReader(io.LimitReader(bufrw.Reader, int64(bufrw.Reader.Buffered())), conn)
	go func() {
		if stdin == nil {
			io.Copy(io.Discard, in) //nolint:errcheck
			return
		}
		if tty {
			if d, _ := copyStdinUntilDetach(stdin, in, detachKeys); d {
				detached.Store(true)
				return
			}
		} else {
			io.Copy(stdin, in) //nolint:errcheck
		}
		// The client's stdin ended (EOF or a closed connection).
		if meta.StdinOnce {
			stdin.close()
		}
	}()
	ao.detached = detached.Load

	streamTaskLogToTTY(bufrw, ns, containerdID, tty, ao)

	// Ensure all buffered output reaches the client before closing.
	bufrw.Flush()
	time.Sleep(100 * time.Millisecond)
}

// queryBool reads a boolean query parameter the way Docker's BoolValue
// does: any value except "", 0, no, false and none is true.
func queryBool(q url.Values, key string) bool {
	switch strings.ToLower(strings.TrimSpace(q.Get(key))) {
	case "", "0", "no", "false", "none":
		return false
	}
	return true
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
	if q.Has("stdout") || q.Has("stderr") {
		wantOut, wantErr := queryBool(q, "stdout"), queryBool(q, "stderr")
		opts.stream = func(s byte) bool { return (s == 1 && wantOut) || (s == 2 && wantErr) }
	}
	// A TTY container's log is one raw stream, as Docker serves it: the CLI
	// copies the body straight to the terminal, so mux headers showed up
	// as garbage before every line.
	tty := getContainerTTY(dockerID(ns, containerdID))

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
		var werr error
		if tty {
			_, werr = w.Write(line)
		} else {
			werr = writeDockerStream(w, stream, line)
		}
		if werr != nil {
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
		if freesDiskSpace(r.Method, path) {
			scheduleDiskTrim()
		}
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

	srv := &http.Server{Handler: mux}
	// The same API inside the VM, for containers that mount the Docker
	// socket (Testcontainers' Ryuk, devcontainers, Traefik, Portainer).
	if ul, err := listenGuestDockerSocket(); err != nil {
		log.Printf("[docker-api] %s: %v", guestDockerSocket, err)
	} else {
		defer ul.Close()
		go func() {
			if err := srv.Serve(ul); err != nil {
				log.Printf("[docker-api] serve %s: %v", guestDockerSocket, err)
			}
		}()
	}

	l, err := vsock.Listen(dockerAPIPort, nil)
	if err != nil {
		log.Printf("[docker-api] listen: %v", err)
		return
	}
	defer l.Close() // a restart after a panic binds again
	log.Printf("[docker-api] listening on vsock port %d", dockerAPIPort)
	if err := srv.Serve(l); err != nil {
		log.Printf("[docker-api] serve: %v", err)
	}
}

// guestDockerSocket is the Docker API socket inside the VM. Bind mounts of
// any Docker socket path resolve to it (dockerSocketBindSource).
const guestDockerSocket = "/run/docker.sock"

func listenGuestDockerSocket() (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(guestDockerSocket), 0o755); err != nil {
		return nil, err
	}
	os.Remove(guestDockerSocket) //nolint:errcheck — stale socket from a previous boot
	l, err := net.Listen("unix", guestDockerSocket)
	if err != nil {
		return nil, err
	}
	// Only containers that mount the socket can reach it, and those are
	// root-equivalent by design (as with Docker); non-root users inside
	// them (devcontainers' vscode user) must be able to connect.
	os.Chmod(guestDockerSocket, 0o666) //nolint:errcheck
	// /var/run is a plain directory in this rootfs, not a link to /run.
	if fi, err := os.Lstat("/var/run"); err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
		os.Remove("/var/run/docker.sock")                     //nolint:errcheck
		os.Symlink(guestDockerSocket, "/var/run/docker.sock") //nolint:errcheck
	}
	log.Printf("[docker-api] listening on %s", guestDockerSocket)
	return l, nil
}

// dockerSocketBindSource maps a bind-mount source naming a Docker socket to
// the guest's own socket. Clients pass whichever path they talk to: the
// conventional /var/run/docker.sock, Docker Desktop's docker.sock.raw, or —
// Testcontainers with a docker context — the Mac-side ~/.anvil-vz/docker.sock,
// which is a unix socket on the virtiofs share that nothing in the VM can
// connect to.
func dockerSocketBindSource(src string) (string, bool) {
	switch filepath.Clean(src) {
	case "/var/run/docker.sock", "/run/docker.sock", "/var/run/docker.sock.raw":
		return guestDockerSocket, true
	}
	if strings.HasSuffix(filepath.Clean(src), "/.anvil-vz/docker.sock") {
		return guestDockerSocket, true
	}
	return src, false
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
func pruneDockerContainers(ctx context.Context, pf pruneFilter) ([]string, int64, error) {
	containers, err := listDockerContainers(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	var deleted []string
	for _, c := range containers {
		if c.State == "running" || c.State == "paused" {
			continue
		}
		if !pf.keep(c.Labels, time.Unix(0, c.created)) {
			continue
		}
		if err := deleteDockerContainer(ctx, c.Id, true, false); err != nil {
			log.Printf("[docker-api] prune container %s: %v", c.Id, err)
			continue
		}
		deleted = append(deleted, c.Id)
	}
	return deleted, 0, nil
}

// pruneDockerNetworks removes unused non-default networks.
func pruneDockerNetworks(ctx context.Context, pf pruneFilter) ([]string, error) {
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
		if !pf.keep(nw.Labels, fileModTime(cniConflistPath(nw.Name))) {
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
func pruneDockerVolumes(ctx context.Context, filters map[string]map[string]bool) ([]string, int64, error) {
	volumes, err := listDockerVolumes(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	// In use = mounted by any container, running or not, read from the
	// containers' specs. (The labels this used to rely on were never set,
	// so every volume looked unused — mounted ones included.)
	mounted, err := mountedBindSources(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list container mounts: %w", err)
	}
	// Docker 23+: only anonymous volumes unless all=true (docker volume
	// prune --all).
	all := filters["all"]["true"] || filters["all"]["1"]
	deleted := []string{}
	var reclaimed int64
	for _, v := range volumes {
		if volumeInUse(mounted, v.Mountpoint) {
			continue
		}
		// A bind-backed volume is mounted from its device, not its own
		// directory: without this it always looked unused, and pruning it
		// dropped the options its next mount needs.
		if dev, ok := bindDeviceOption(v.Options); ok && mounted[filepath.Clean(dev)] {
			continue
		}
		if _, anon := v.Labels[labelAnonymousVolume]; !anon && !all {
			continue
		}
		if !matchesLabelFilters(v.Labels, filters) {
			continue
		}
		if len(filters["until"]) > 0 {
			if pf, err := newPruneFilter(map[string]map[string]bool{"until": filters["until"]}); err == nil &&
				!pf.keep(nil, parseCreatedAt(v.CreatedAt)) {
				continue
			}
		}
		size := dirSize(v.Mountpoint)
		if err := removeDockerVolume(ctx, v.Name); err != nil {
			log.Printf("[docker-api] prune volume %s: %v", v.Name, err)
			continue
		}
		deleted = append(deleted, v.Name)
		reclaimed += size
	}
	return deleted, reclaimed, nil
}

// dirSize sums the regular files under dir (best effort).
func dirSize(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err == nil && d.Type().IsRegular() {
			if fi, ierr := d.Info(); ierr == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

// writeJSONError writes a Docker-API style error body. Hand-concatenated
// JSON breaks whenever the message itself contains quotes (e.g. %q-formatted
// validator errors), so this always marshals properly.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// writeHijackHeader starts a hijacked attach/exec stream as dockerd does:
// 101 UPGRADED only when the client asked to upgrade, otherwise 200 OK
// with the raw stream following (docker-java execs without stdin send no
// Upgrade header and parse a 101 as the end of the response). Non-TTY
// output is announced as multiplexed.
func writeHijackHeader(bufrw *bufio.ReadWriter, r *http.Request, tty bool) error {
	contentType := "application/vnd.docker.multiplexed-stream"
	if tty {
		contentType = "application/vnd.docker.raw-stream"
	}
	if r.Header.Get("Upgrade") != "" {
		fmt.Fprintf(bufrw, "HTTP/1.1 101 UPGRADED\r\nContent-Type: %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", contentType)
	} else {
		fmt.Fprintf(bufrw, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\n\r\n", contentType)
	}
	return bufrw.Flush()
}
