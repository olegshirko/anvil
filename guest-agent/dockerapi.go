package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/mdlayher/vsock"
)

const dockerAPIVersion = "1.24"

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
		return true
	}
	return true
}

// streamNerdctlLogsTo runs `nerdctl logs` (and optionally `nerdctl logs -f`)
// and writes output as Docker multiplexed stream frames until the command exits
// or the writer fails.
func streamNerdctlLogsTo(out io.Writer, ns, name string, follow bool) {
	streamNerdctlLogsToTTY(out, ns, name, follow, false)
}

// streamNerdctlLogsToTTY streams `nerdctl logs` output; with tty=true the
// bytes go out raw (Docker sends TTY output unmultiplexed — a mux header
// would corrupt the client terminal), otherwise in the 8-byte-header
// multiplexed stream format.
func streamNerdctlLogsToTTY(out io.Writer, ns, name string, follow bool, tty bool) {
	flusher, _ := out.(http.Flusher)
	buf := make([]byte, 4096)

	// Helper that runs nerdctl logs with given args and copies output to out.
	runLogs := func(extraArgs ...string) (int, error) {
		args := []string{"-n", ns, "logs"}
		args = append(args, extraArgs...)
		args = append(args, name)
		cmd := exec.Command("/opt/containerd/bin/nerdctl", args...)
		cmd.Env = append(cmd.Env, "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return 0, err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return 0, err
		}
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		total := 0
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				total += n
				var writeErr error
				if tty {
					_, writeErr = out.Write(buf[:n])
				} else {
					writeErr = writeDockerStream(out, 1, buf[:n])
				}
				if writeErr != nil {
					cmd.Process.Kill()
					cmd.Wait()
					return total, writeErr
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if err != nil {
				break
			}
		}
		io.Copy(io.Discard, stderr)
		cmd.Wait()
		return total, nil
	}

	// Attach often arrives before the client issues start. Wait briefly for
	// the container to leave the "created" state, otherwise a short-lived
	// container may start, print and exit while we are still replaying
	// nothing — and its output is lost.
	for i := 0; i < 100; i++ {
		status := nerdctlContainerStatus(ns, name)
		if status != "" && status != "created" && status != "paused" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// First replay any output that already exists. Short-lived containers may
	// need a brief moment for the json-file log to be flushed after exit.
	for attempt := 0; attempt < 10; attempt++ {
		n, err := runLogs()
		if err == nil && n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Then follow if requested and the container is still running.
	if follow && isNerdctlContainerRunning(ns, name) {
		runLogs("-f")
	}
}

// handleAttach hijacks the HTTP connection and streams container output using
// Docker's raw-stream multiplexing format. It uses `nerdctl logs` (non-following)
// first to replay output that already exists, then `nerdctl logs -f` only if
// the container is still running and the client asked for a stream. This avoids
// the race where short-lived containers exit before attach is called.
func handleAttach(w http.ResponseWriter, r *http.Request, id string) {
	ns, containerdID, name, err := resolveDockerID(id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
		return
	}
	// Keep AutoRemove from deleting the container (and its logs) while we
	// are replaying output to the client.
	did := dockerID(ns, containerdID)
	attachBegin(did)
	defer attachEnd(did)

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, `{"message":"hijacking not supported"}`, http.StatusInternalServerError)
		return
	}

	conn, bufrw, err := hj.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
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
	streamNerdctlLogsToTTY(bufrw, ns, name, true, getContainerTTY(did))

	// Ensure all buffered output reaches the client before closing.
	bufrw.Flush()
	time.Sleep(100 * time.Millisecond)
}

// handleLogs streams container logs using Docker's multiplexed stream format.
func handleLogs(w http.ResponseWriter, r *http.Request, id string) {
	ns, _, name, err := resolveDockerID(id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
		return
	}

	args := []string{"-n", ns, "logs"}
	if r.URL.Query().Get("timestamps") == "1" || r.URL.Query().Get("timestamps") == "true" {
		args = append(args, "-t")
	}
	follow := r.URL.Query().Get("follow") == "1" || r.URL.Query().Get("follow") == "true"
	if follow {
		args = append(args, "-f")
	}
	// tail=N: buffer the replayed output and emit only the last N lines.
	// nerdctl has no --tail; implemented post-hoc on the replayed output
	// (follow-mode streams everything that comes after, like Docker).
	tail := 0
	if v := r.URL.Query().Get("tail"); v != "" && v != "all" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			tail = n
		}
	}
	args = append(args, name)

	cmd := exec.Command("/opt/containerd/bin/nerdctl", args...)
	cmd.Env = append(cmd.Env, "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer cmd.Wait()

	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	buf := make([]byte, 4096)
	var pending []byte
	var curLine []byte
	lineDone := func(line []byte) {
		if tail > 0 && !follow {
			pending = append(pending, line...)
			pending = append(pending, '\n')
			return
		}
		writeDockerStream(w, 1, line)
		writeDockerStream(w, 1, []byte("\n"))
	}
	for {
		n, err := stdout.Read(buf)
		for _, b := range buf[:n] {
			if b == '\n' {
				lineDone(curLine)
				curLine = curLine[:0]
			} else {
				curLine = append(curLine, b)
			}
		}
		if err != nil {
			break
		}
	}
	if len(curLine) > 0 {
		lineDone(curLine)
	}
	// Emit the tail: last N lines of the buffered replay.
	if tail > 0 && !follow {
		lines := strings.Split(strings.TrimSuffix(string(pending), "\n"), "\n")
		if len(lines) > tail {
			lines = lines[len(lines)-tail:]
		}
		writeDockerStream(w, 1, []byte(strings.Join(lines, "\n")+"\n"))
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// runDockerAPIServer starts the Docker-compatible HTTP server on vsock:1025.
func runDockerAPIServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := stripAPIVersion(r.URL.Path)
		log.Printf("[docker-api] %s %s", r.Method, path)

		// Docker CLI reuses a single connection for multiple API calls. Our
		// vsock proxy maps one host connection to one guest connection, so a
		// blocking /wait would deadlock the next /start on the same connection.
		// Force the client to open a new connection after each response.
		w.Header().Set("Connection", "close")

		// Pre-compute container sub-resource IDs.
		containerSubresource := func(prefix, suffix string) (string, bool) {
			if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
				return "", false
			}
			return strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix), true
		}
		startID, isStart := containerSubresource("/containers/", "/start")
		stopID, isStop := containerSubresource("/containers/", "/stop")
		waitID, isWait := containerSubresource("/containers/", "/wait")
		deleteID, isDelete := containerSubresource("/containers/", "")
		inspectID, isInspect := containerSubresource("/containers/", "/json")
		attachID, isAttach := containerSubresource("/containers/", "/attach")
		logsID, isLogs := containerSubresource("/containers/", "/logs")
		execCreateID, isExecCreate := containerSubresource("/containers/", "/exec")
		killID, isKill := containerSubresource("/containers/", "/kill")
		restartID, isRestart := containerSubresource("/containers/", "/restart")
		renameID, isRename := containerSubresource("/containers/", "/rename")
		pauseID, isPause := containerSubresource("/containers/", "/pause")
		unpauseID, isUnpause := containerSubresource("/containers/", "/unpause")
		topID, isTop := containerSubresource("/containers/", "/top")
		statsID, isStats := containerSubresource("/containers/", "/stats")
		archiveID, isArchive := containerSubresource("/containers/", "/archive")
		execStartID, isExecStart := containerSubresource("/exec/", "/start")
		execInspectID, isExecInspect := containerSubresource("/exec/", "/json")
		netConnectID, isNetConnect := containerSubresource("/networks/", "/connect")
		netDisconnectID, isNetDisconnect := containerSubresource("/networks/", "/disconnect")

		// Pre-compute image sub-resource names. Avoid matching /images/json and /images/create.
		imageSubresource := func(prefix, suffix string) (string, bool) {
			if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
				return "", false
			}
			name := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
			if name == "" || name == "json" || name == "create" {
				return "", false
			}
			return name, true
		}
		tagName, isTag := imageSubresource("/images/", "/tag")
		pushName, isPush := imageSubresource("/images/", "/push")
		getName, isGet := imageSubresource("/images/", "/get")
		imageInspectName, isImageInspect := imageSubresource("/images/", "/json")
		rmiName := ""
		isRMI := false
		if r.Method == http.MethodDelete && strings.HasPrefix(path, "/images/") {
			name := strings.TrimPrefix(path, "/images/")
			if name != "" && name != "json" && name != "create" {
				rmiName, isRMI = name, true
			}
		}

		// Pre-compute network/volume sub-resource names.
		networkInspectID, isNetworkInspect := containerSubresource("/networks/", "")
		volumeInspectName, isVolumeInspect := containerSubresource("/volumes/", "")

		switch {
		case path == "/_ping":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		case path == "/version":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"Version":       "24.0.0",
				"ApiVersion":    dockerAPIVersion,
				"MinAPIVersion": dockerAPIVersion,
				"GitCommit":     "anvil",
				"Os":            "linux",
				"Arch":          "arm64",
				"KernelVersion": "",
				"BuildTime":     "",
			})
		case path == "/info" && r.Method == http.MethodGet:
			handleDockerInfo(w, r)
		case path == "/system/df" && r.Method == http.MethodGet:
			handleSystemDF(w)
		case path == "/system/prune" && r.Method == http.MethodPost:
			// docker system prune = containers + networks + volumes(?) + images
			// (volumes only with --volumes, sent as a filter).
			filters := parseDockerFilters(r.URL.Query().Get("filters"))
			withVolumes := false
			if v, ok := filters["volumes"]["true"]; ok && v {
				withVolumes = true
			} // container prune: remove all stopped containers.
			stopped, _, _ := pruneDockerContainers()
			nets, _ := pruneDockerNetworks()
			var vols []string
			if withVolumes {
				vols, _, _ = pruneDockerVolumes()
			}
			imgs, reclaimed, _ := pruneDockerImages(false)
			_ = imgs
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ContainersDeleted": stopped,
				"NetworksDeleted":   nets,
				"VolumesDeleted":    vols,
				"SpaceReclaimed":    reclaimed,
			})
		case path == "/containers/json":
			all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
			filters := parseDockerFilters(r.URL.Query().Get("filters"))
			containers, err := listDockerContainers(filters)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
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
		case path == "/containers/prune" && r.Method == http.MethodPost:
			deleted, reclaimed, err := pruneDockerContainers()
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ContainersDeleted": deleted,
				"SpaceReclaimed":    reclaimed,
			})
		case path == "/images/json":
			images, err := listDockerImages()
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(images)
		case path == "/images/create":
			image := r.URL.Query().Get("fromImage")
			if tag := r.URL.Query().Get("tag"); tag != "" {
				image += ":" + tag
			}
			if image == "" {
				http.Error(w, `{"message":"missing fromImage"}`, http.StatusBadRequest)
				return
			}
			status, err := pullDockerImage(image)
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				// Docker CLI expects a stream of progress objects; send one error line.
				json.NewEncoder(w).Encode(map[string]string{
					"status": fmt.Sprintf("error pulling %s: %s", image, err.Error()),
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{
				"status": status,
			})
		case isTag && r.Method == http.MethodPost:
			target := r.URL.Query().Get("repo")
			if tag := r.URL.Query().Get("tag"); tag != "" {
				target += ":" + tag
			}
			if target == "" {
				http.Error(w, `{"message":"missing repo/tag"}`, http.StatusBadRequest)
				return
			}
			if err := tagDockerImage(tagName, target); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case isPush && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			if err := pushDockerImage(pushName, w); err != nil {
				fmt.Fprintf(w, "{\"status\":\"error pushing %s: %s\"}\n", pushName, err.Error())
				return
			}
		case isRMI:
			if err := removeDockerImage(rmiName); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]string{{"Deleted": rmiName}})
		case path == "/images/prune" && r.Method == http.MethodPost:
			filters := parseDockerFilters(r.URL.Query().Get("filters"))
			dangling := true
			if filters["dangling"]["false"] {
				dangling = false
			}
			deleted, reclaimed, err := pruneDockerImages(dangling)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ImagesDeleted":  deleted,
				"SpaceReclaimed": reclaimed,
			})
		case path == "/images/load" && r.Method == http.MethodPost:
			handleImageLoad(w, r)
		case path == "/events" && r.Method == http.MethodGet:
			handleEvents(w, r)
		case path == "/build" && r.Method == http.MethodPost:
			handleBuild(w, r)
		case path == "/images/get" && r.Method == http.MethodGet:
			handleImagesGet(w, r)
		case isGet && r.Method == http.MethodGet:
			handleImageGet(w, r, getName)
		case path == "/build/prune" && r.Method == http.MethodPost:
			reclaimed, err := pruneBuildCache()
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"CachesDeleted":  []string{},
				"SpaceReclaimed": reclaimed,
			})
		case isImageInspect && r.Method == http.MethodGet:
			log.Printf("[docker-api] image inspect %q (resolved ns=%q)", imageInspectName, findImageNamespace(imageInspectName))
			info, err := inspectDockerImage(imageInspectName)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(info)
		case path == "/networks":
			filters := parseDockerFilters(r.URL.Query().Get("filters"))
			networks, err := listDockerNetworks(filters)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(networks)
		case path == "/networks/create" && r.Method == http.MethodPost:
			var req dockerNetworkCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusBadRequest)
				return
			}
			nw, err := createDockerNetwork(req)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"Id": nw.Id, "Warning": ""})
		case isNetworkInspect && r.Method == http.MethodGet:
			nw, err := inspectDockerNetwork(networkInspectID)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(nw)
		case isNetworkInspect && r.Method == http.MethodDelete:
			if err := removeDockerNetwork(networkInspectID); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isNetConnect && r.Method == http.MethodPost:
			var req struct {
				Container string `json:"Container"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Container == "" {
				http.Error(w, `{"message":"missing Container"}`, http.StatusBadRequest)
				return
			}
			if err := connectContainerNetwork(netConnectID, req.Container); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isNetDisconnect && r.Method == http.MethodPost:
			var req struct {
				Container string `json:"Container"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Container == "" {
				http.Error(w, `{"message":"missing Container"}`, http.StatusBadRequest)
				return
			}
			if err := disconnectContainerNetwork(netDisconnectID, req.Container); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case path == "/networks/prune" && r.Method == http.MethodPost:
			deleted, err := pruneDockerNetworks()
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string][]string{"NetworksDeleted": deleted})
		case path == "/volumes":
			filters := parseDockerFilters(r.URL.Query().Get("filters"))
			volumes, err := listDockerVolumes(filters)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(dockerVolumeList{Volumes: volumes})
		case path == "/volumes/prune" && r.Method == http.MethodPost:
			deleted, reclaimed, err := pruneDockerVolumes()
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"VolumesDeleted": deleted,
				"SpaceReclaimed": reclaimed,
			})
		case path == "/volumes/create" && r.Method == http.MethodPost:
			var req dockerVolumeCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusBadRequest)
				return
			}
			vol, err := createDockerVolume(req)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(vol)
		case isVolumeInspect && r.Method == http.MethodGet:
			vol, err := inspectDockerVolume(volumeInspectName)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(vol)
		case isVolumeInspect && r.Method == http.MethodDelete:
			if err := removeDockerVolume(volumeInspectName); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case path == "/containers/create" && r.Method == http.MethodPost:
			var req dockerCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusBadRequest)
				return
			}
			name := r.URL.Query().Get("name")
			id, err := createDockerContainer(req, name)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(dockerCreateResponse{Id: id})
		case isStart && r.Method == http.MethodPost:
			if err := startDockerContainer(startID); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isStop && r.Method == http.MethodPost:
			timeout := 10
			if t := r.URL.Query().Get("t"); t != "" {
				if v, err := strconv.Atoi(t); err == nil {
					timeout = v
				}
			}
			if err := stopDockerContainer(stopID, timeout); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isKill && r.Method == http.MethodPost:
			signal := r.URL.Query().Get("signal")
			if signal == "" {
				signal = "SIGKILL"
			}
			if err := killDockerContainer(killID, signal); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isRestart && r.Method == http.MethodPost:
			timeout := 0
			if t := r.URL.Query().Get("t"); t != "" {
				if v, err := strconv.Atoi(t); err == nil {
					timeout = v
				}
			}
			if err := restartDockerContainer(restartID, timeout); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isWait && r.Method == http.MethodPost:
			handleContainerWait(w, r, waitID)
		case isRename && r.Method == http.MethodPost:
			newName := strings.TrimPrefix(r.URL.Query().Get("name"), "/")
			if err := renameDockerContainer(renameID, newName); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isPause && r.Method == http.MethodPost:
			if err := pauseDockerContainer(pauseID, true); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isUnpause && r.Method == http.MethodPost:
			if err := pauseDockerContainer(unpauseID, false); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isTop && r.Method == http.MethodGet:
			handleContainerTop(w, topID)
		case isStats && r.Method == http.MethodGet:
			handleContainerStats(w, statsID, r.URL.Query().Get("stream") == "1")
		case isAttach && r.Method == http.MethodPost:
			handleAttach(w, r, attachID)
		case isLogs && r.Method == http.MethodGet:
			handleLogs(w, r, logsID)
		case isArchive && (r.Method == http.MethodHead || r.Method == http.MethodGet || r.Method == http.MethodPut):
			handleContainerArchive(w, r, archiveID)
		case isExecCreate && r.Method == http.MethodPost:
			var req dockerExecCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusBadRequest)
				return
			}
			id, err := createDockerExec(execCreateID, req)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(dockerExecCreateResponse{Id: id})
		case isExecStart && r.Method == http.MethodPost:
			var req dockerExecStartRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusBadRequest)
				return
			}
			if req.Detach {
				if err := startDetachedExec(execStartID); err != nil {
					http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusOK)
				return
			}
			handleExecStart(w, r, execStartID)
		case isExecInspect && r.Method == http.MethodGet:
			info, err := inspectDockerExec(execInspectID)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(info)
		case isDelete && r.Method == http.MethodDelete:
			force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
			if err := deleteDockerContainer(deleteID, force); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isInspect && r.Method == http.MethodGet:
			inspect, err := inspectDockerContainer(inspectID)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(inspect)
		default:
			http.NotFound(w, r)
		}
	})

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

// pruneDockerContainers removes stopped/created containers and returns their
// Docker IDs. It mirrors the response shape of POST /containers/prune.
func pruneDockerContainers() ([]string, int64, error) {
	containers, err := listDockerContainers(nil)
	if err != nil {
		return nil, 0, err
	}
	var deleted []string
	for _, c := range containers {
		if c.State == "running" {
			continue
		}
		if err := deleteDockerContainer(c.Id, true); err != nil {
			log.Printf("[docker-api] prune container %s: %v", c.Id, err)
			continue
		}
		deleted = append(deleted, c.Id)
	}
	return deleted, 0, nil
}

// pruneDockerNetworks removes unused non-default networks.
func pruneDockerNetworks() ([]string, error) {
	networks, err := listDockerNetworks(nil)
	if err != nil {
		return nil, err
	}
	containers, err := listDockerContainers(nil)
	if err != nil {
		return nil, err
	}
	inUse := map[string]struct{}{}
	for _, c := range containers {
		labels := c.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		netsJSON := labels["nerdctl/networks"]
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
		if err := removeDockerNetwork(nw.Name); err != nil {
			log.Printf("[docker-api] prune network %s: %v", nw.Name, err)
			continue
		}
		deleted = append(deleted, nw.Name)
	}
	return deleted, nil
}

// pruneDockerVolumes removes volumes not referenced by any container.
func pruneDockerVolumes() ([]string, int64, error) {
	volumes, err := listDockerVolumes(nil)
	if err != nil {
		return nil, 0, err
	}
	containers, err := listDockerContainers(nil)
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
		anonJSON := labels["nerdctl/anonymous-volumes"]
		if anonJSON != "" {
			var vols []string
			if err := json.Unmarshal([]byte(anonJSON), &vols); err == nil {
				for _, v := range vols {
					inUse[v] = struct{}{}
				}
			}
		}
		// Named mounts.
		mountsJSON := labels["nerdctl/mounts"]
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
		if err := removeDockerVolume(v.Name); err != nil {
			log.Printf("[docker-api] prune volume %s: %v", v.Name, err)
			continue
		}
		deleted = append(deleted, v.Name)
	}
	return deleted, 0, nil
}
