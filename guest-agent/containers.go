package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// waitContainerTask blocks until the containerd task exits and returns its exit
// code. It returns an error if the task does not appear or is already deleted.
func waitContainerTask(ctx context.Context, ns, containerdID string) (int, error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return 0, err
	}

	nsCtx := namespaces.WithNamespace(ctx, ns)

	// Poll until the task exists. /wait may be called before /start.
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := cl.LoadContainer(nsCtx, containerdID)
		if err != nil {
			return 0, fmt.Errorf("load container: %w", err)
		}
		_, err = c.Task(nsCtx, nil)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("timeout waiting for task")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Block on the task's Wait channel until it exits. A cached code (e.g.
	// the mapped 137 from docker kill) wins over the task status, which
	// reports 0 for signal deaths.
	did := dockerID(ns, containerdID)
	c, err := cl.LoadContainer(nsCtx, containerdID)
	if err != nil {
		return 0, fmt.Errorf("load container: %w", err)
	}
	task, err := c.Task(nsCtx, nil)
	if err != nil {
		return 0, fmt.Errorf("task: %w", err)
	}
	exitCh, werr := task.Wait(nsCtx)
	if werr != nil {
		return 0, fmt.Errorf("wait: %w", werr)
	}
	st := <-exitCh
	if serr := st.Error(); serr != nil {
		debugLog("[docker-api] wait %s/%s: %v", ns, truncateID(containerdID), serr)
	}
	exit := int(st.ExitCode())
	if cached, ok := takeContainerExitCode(did); ok && cached != 0 {
		return cached, nil
	}
	return exit, nil
}

// createDockerContainer creates a container natively via containerd and
// returns its Docker ID.
func createDockerContainer(ctx context.Context, req dockerCreateRequest, name, platform string, auth *registryAuth) (string, []string, error) {
	networkMode := req.HostConfig.NetworkMode
	ns := namespaceFromNetwork(networkMode)
	// Compose attaches containers to a network named <project>_<network>. The
	// container itself carries the project label, which is the authoritative
	// containerd namespace.
	if project := req.Labels["com.docker.compose.project"]; project != "" {
		ns = project
	}
	log.Printf("[docker-api] create container %q image=%q network=%q namespace=%q", name, req.Image, networkMode, ns)

	// Make sure the per-network CNI conflist exists before creating the
	// container, otherwise CNI attach fails with "no such network".
	if !usesHostNetwork(req) {
		for _, n := range append([]string{effectiveNetworkName(networkMode)}, secondaryNetworksFromCreate(req)...) {
			if n == noneNetwork {
				continue // no bridge: lo only (attachNetwork)
			}
			if err := generateCNIConfig(n); err != nil {
				log.Printf("[docker-api] ensure cni config for %s: %v", n, err)
			}
		}
	}

	// Ensure the image metadata exists in the target namespace. The content
	// store is shared, so when the image already exists elsewhere we copy its
	// metadata instead of re-pulling, which avoids corrupting the shared content
	// store when docker compose creates multiple containers in parallel.
	if err := ensureImageInNamespace(ctx, req.Image, ns, platform, auth); err != nil {
		return "", nil, err
	}

	// Docker refuses duplicate names; mimic that to avoid ambiguous lookups later.
	if name != "" {
		if existing, err := findContainerByName(ctx, ns, name); err == nil && existing != "" {
			return "", nil, fmt.Errorf("Conflict. The container name \"/%s\" is already in use by container \"%s\". You have to remove (or rename) that container to be able to reuse that name.", name, existing)
		}
	}

	containerdID, warnings, err := createNativeContainer(ctx, ns, name, platform, req)
	if err != nil {
		return "", nil, err
	}

	dockerID := dockerID(ns, containerdID)
	// Remember docker --link specs for the start-time hosts update.
	if len(req.HostConfig.Links) > 0 {
		setContainerLinks(dockerID, req.HostConfig.Links)
	}
	// Register the restart policy with our monitor (the runtime has none —
	// restart.go owns the policy; inspect reports it from the registry).
	if spec := req.HostConfig.RestartPolicy.Name; spec != "" {
		p := parseRestartPolicy(spec)
		if p.max < 0 && req.HostConfig.RestartPolicy.MaximumRetryCount > 0 {
			p.max = req.HostConfig.RestartPolicy.MaximumRetryCount
		}
		restarts.registerAt(ns, containerdID, p.name, p.max)
	}
	// Remember TTY for attach (raw stream) and inspect responses.
	setContainerTTY(dockerID, req.Tty)
	// Remember WorkingDir/Entrypoint for inspect responses.
	setContainerEntryPointInfo(dockerID, req.WorkingDir, req.Entrypoint)
	setContainerStopSignal(dockerID, req.StopSignal)
	// AutoRemove is handled by guest-agent after we capture the exit code,
	// so /wait can still read the status.
	if req.HostConfig.AutoRemove {
		markAutoRemove(dockerID)
	}
	// Healthchecks run in-agent via exec on a ticker. Store the configuration
	// at create time and start the ticker when the container is started so
	// the first check does not run against a created-but-not-yet-running
	// container. The exec user mirrors the container process user.
	containerUser := ""
	cl, cerr := pc.get(ctx)
	if cerr == nil {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		if c, lerr := cl.LoadContainer(nsCtx, containerdID); lerr == nil {
			if spec, serr := c.Spec(nsCtx); serr == nil && spec != nil && spec.Process != nil {
				containerUser = spec.Process.User.Username
			}
		}
	}
	setHealthcheckConfig(dockerID, req.Healthcheck, containerUser)
	return dockerID, warnings, nil
}

// expandPortRange parses "80" or "80-81" into a list of port numbers.
func expandPortRange(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("empty port")
	}
	lo, hi, isRange := strings.Cut(spec, "-")
	if !isRange {
		p, err := strconv.Atoi(spec)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("bad port %q", spec)
		}
		return []int{p}, nil
	}
	l, err1 := strconv.Atoi(strings.TrimSpace(lo))
	r, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil || l < 1 || r < l || r > 65535 || r-l > 1000 {
		return nil, fmt.Errorf("bad port range %q", spec)
	}
	out := make([]int, 0, r-l+1)
	for p := l; p <= r; p++ {
		out = append(out, p)
	}
	return out, nil
}

// pruneStaleContainerMeta removes anvil metadata directories
// (/var/lib/anvil/containers/<ns>/<id>) whose container no longer exists in
// containerd. Cold-boot cleanup deletes containers directly through the
// containerd API, so their metadata would otherwise accumulate forever on
// the persistent disk.
func pruneStaleContainerMeta() {
	cl, err := pc.get(context.Background())
	if err != nil {
		return
	}

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return
	}
	live := make(map[string]map[string]bool, len(nss))
	for _, ns := range nss {
		containers, err := cl.Containers(namespaces.WithNamespace(ctx, ns))
		if err != nil {
			continue
		}
		live[ns] = make(map[string]bool, len(containers))
		for _, c := range containers {
			live[ns][c.ID()] = true
		}
	}

	metas, err := containerMetas()
	if err != nil {
		return
	}
	for _, m := range metas {
		if ids, ok := live[m.Namespace]; !ok || !ids[m.ID] {
			debugLog("pruning stale anvil metadata %s/%s", m.Namespace, truncateID(m.ID))
			deleteContainerMeta(m.Namespace, m.ID)
			releaseNamedNetNS(m.ID)
		}
	}
}

// containerHostPorts returns the TCP host ports published by the container,
// read from the agent-persisted port-mapping metadata.
func containerHostPorts(ctx context.Context, ns, containerdID string) []int {
	cl, err := pc.get(ctx)
	if err != nil {
		return nil
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, containerdID)
	if err != nil {
		return nil
	}
	portsJSON := portsLabel(c, nsCtx)
	if portsJSON == "" {
		return nil
	}
	var mapped []cniPortMapping
	if err := json.Unmarshal([]byte(portsJSON), &mapped); err != nil {
		return nil
	}
	var ports []int
	for _, m := range mapped {
		if m.HostPort <= 0 {
			continue
		}
		if m.Protocol != "" && m.Protocol != "tcp" {
			continue
		}
		ports = append(ports, m.HostPort)
	}
	return ports
}

func startDockerContainer(ctx context.Context, id string) error {
	ns, containerdID, _, err := resolveDockerID(ctx, id)
	if err != nil {
		return err
	}

	// Enforce host-port availability at start, like Docker does (create must
	// NOT check: compose --force-recreate creates the replacement while the
	// old container still holds the port). Two cases: the port is published
	// by another running container, or it is bound on the host by a foreign
	// process (Docker Desktop, Lima, a local postgres) — the port forwarder
	// can only log that bind failure, leaving the container silently
	// unreachable on localhost.
	if ports := containerHostPorts(ctx, ns, containerdID); len(ports) > 0 {
		if conflict, err := findHostPortConflict(ctx, ports, containerdID); err == nil && conflict != nil {
			log.Printf("[portcheck] start %s: host port %d already published by container %q (%s) in ns %q",
				containerdID, conflict.HostPort, conflict.Name, conflict.ContainerID, conflict.Namespace)
			return fmt.Errorf("Bind for 0.0.0.0:%d failed: port is already allocated", conflict.HostPort)
		}
		if busy := busyForeignHostPorts(ports); len(busy) > 0 {
			log.Printf("[portcheck] start %s: host port %d bound by a foreign host process", containerdID, busy[0])
			return fmt.Errorf("Bind for 0.0.0.0:%d failed: port is already allocated", busy[0])
		}
	}

	// docker --link: append alias -> target-IP entries to THIS container's
	// /etc/hosts BEFORE the task runs, so the very first lookup inside the
	// container already resolves (legacy link semantics are plain /etc/hosts
	// records). The hosts file is prepared at create; the post-start
	// application below covers restarts.
	if links := pendingLinkEntries(dockerID(ns, containerdID)); len(links) > 0 {
		applyLinkAliases(ctx, ns, containerdID, links)
	}
	// Native start: CNI attach + task creation + json-file logging.
	if err := startNativeTask(ctx, ns, containerdID); err != nil {
		return err
	}
	// Cross-container name resolution: regenerate the managed /etc/hosts
	// section for every network this container joined. Runs after the CNI
	// attach, so the address is known; done in the background so start is
	// not delayed by the mesh rewrite.
	go refreshHostsForContainer(ns, containerdID)
	// docker --link: append alias -> target-IP entries to THIS container's
	// /etc/hosts (legacy link semantics are plain /etc/hosts records).
	if links := pendingLinkEntries(dockerID(ns, containerdID)); len(links) > 0 {
		go applyLinkAliases(context.Background(), ns, containerdID, links)
	}
	did := dockerID(ns, containerdID)

	// For AutoRemove containers we wait for the exit code ourselves and then
	// delete the container. Deleting earlier would break /wait.
	if isAutoRemove(did) {
		go func() {
			code, _ := waitContainerTask(context.Background(), ns, containerdID)
			cacheContainerExitCode(did, code)
			// Let any attach connection finish replaying the output before
			// the container (and its logs) disappear.
			waitForAttachDrain(did, 30*time.Second)
			if err := deleteDockerContainer(context.Background(), did, true); err != nil {
				log.Printf("[docker-api] auto-remove %s: %v", did, err)
			}
			unmarkAutoRemove(did)
		}()
	}
	return nil
}

// stopDockerContainer stops a container by Docker ID or name and waits until
// the containerd task actually reaches the stopped state.
// return before the task exits, which makes a subsequent `docker rm` fail with
// "container is in running status".
func stopDockerContainer(ctx context.Context, id string, timeout int) error {
	ns, containerdID, _, err := resolveDockerID(ctx, id)
	if err != nil {
		return err
	}
	did := dockerID(ns, containerdID)
	// A user stop wins over any restart policy: drop it before signalling
	// so the restart monitor cannot race a restart between the exit and
	// the cleanup below.
	restarts.clear(did)
	// Native stop: stop signal, then SIGKILL after the grace period.
	if err := stopNativeTask(ctx, ns, containerdID, timeout); err != nil {
		return err
	}

	// Poll containerd until the task stops or disappears.
	waitDeadline := time.Now().Add(30 * time.Second)
	cl, cerr := pc.get(ctx)
	nsCtx := namespaces.WithNamespace(ctx, ns)
	for cerr == nil {
		c, err := cl.LoadContainer(nsCtx, containerdID)
		if err != nil {
			break
		}
		task, err := c.Task(nsCtx, nil)
		if err != nil {
			break
		}
		st, err := task.Status(nsCtx)
		if err != nil || st.Status == "stopped" {
			break
		}
		if time.Now().After(waitDeadline) {
			log.Printf("[docker-api] stop %s: timeout waiting for task to stop", id)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	stopHealthCheck(did)
	restarts.clear(did)
	return nil
}

// handleContainerWait implements POST /containers/{id}/wait.
//
// Docker CLI calls /wait before /start for `docker run`. If we block the HTTP
// response, the client cannot send /start on the same connection and the
// container never starts. To avoid this deadlock we send the response headers
// immediately (chunked encoding) and then stream the final exit code once the
// container actually exits. The client sees an in-flight response and opens a
// separate connection for /start.
func handleContainerWait(w http.ResponseWriter, r *http.Request, id string) {
	writeCode := func(code int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"StatusCode":%d}`, code)
	}

	// Exit codes are cached by Docker ID, so resolve names first.
	ns, containerdID, _, err := resolveDockerID(r.Context(), id)
	if err != nil {
		// An AutoRemove container may already be deleted; its exit code
		// was cached before the removal.
		if code, ok := takeContainerExitCode(id); ok {
			writeCode(code)
			return
		}
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	did := dockerID(ns, containerdID)
	// A cached code belongs to the current run: every start clears it, so
	// the container has exited since its last start.
	if code, ok := takeContainerExitCode(did); ok {
		writeCode(code)
		return
	}

	// Send headers immediately so the client can proceed with /start on a
	// separate connection while we wait for the exit code.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	exitCode, err := waitContainerTask(r.Context(), ns, containerdID)
	if err != nil {
		log.Printf("[docker-api] wait %s task error: %v", id, err)
	} else {
		// Only a real exit is cached. `docker run -d` opens a /wait and
		// drops it right after /start; caching that aborted wait's 0 made
		// the next `docker wait <name>` return 0 immediately.
		cacheContainerExitCode(did, exitCode)
	}
	log.Printf("[docker-api] wait %s returning StatusCode=%d", id, exitCode)
	fmt.Fprintf(w, `{"StatusCode":%d}`, exitCode)
}

// deleteDockerContainer removes a container by Docker ID or name.
func deleteDockerContainer(ctx context.Context, id string, force bool) error {
	ns, containerdID, _, err := resolveDockerID(ctx, id)
	if err != nil {
		return err
	}
	did := dockerID(ns, containerdID)
	// Native delete: task + snapshot + CNI + netns + metadata cleanup.
	if err := deleteNativeContainer(ctx, ns, containerdID, force); err != nil {
		return err
	}
	forgetContainerState(did)
	return nil
}

// renameDockerContainer implements POST /containers/{id}/rename. Compose
// uses it in the recreate flow: the replacement is created under a temporary
// name and renamed once the old container is removed.
func renameDockerContainer(ctx context.Context, id string, newName string) error {
	ns, containerdID, _, err := resolveDockerID(ctx, id)
	if err != nil {
		return err
	}
	if newName == "" {
		return fmt.Errorf("name is required")
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return err
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, containerdID)
	if err != nil {
		return err
	}
	labels, err := c.Labels(nsCtx)
	if err != nil {
		return err
	}
	oldName := labels[labelName]
	labels[labelName] = newName
	if _, err := c.SetLabels(nsCtx, labels); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	if meta, merr := loadContainerMeta(ns, containerdID); merr == nil {
		meta.Name = newName
		saveContainerMeta(meta)
		refreshHostsForContainer(ns, containerdID)
	}
	debugLog("[docker-api] renamed %s/%s: %s -> %s", ns, truncateID(containerdID), oldName, newName)
	return nil
}

// killDockerContainer kills a container by Docker ID or name.
func killDockerContainer(ctx context.Context, id string, signal string) error {
	ns, containerdID, _, err := resolveDockerID(ctx, id)
	if err != nil {
		return err
	}
	// A user signal wins over any restart policy (see stopDockerContainer).
	restarts.clear(dockerID(ns, containerdID))
	// Native kill: signal the running task directly.
	{
		cl, cerr := pc.get(ctx)
		if cerr != nil {
			return fmt.Errorf("containerd client: %w", cerr)
		}
		nsCtx := namespaces.WithNamespace(ctx, ns)
		c, lerr := cl.LoadContainer(nsCtx, containerdID)
		if lerr != nil {
			return lerr
		}
		task, terr := c.Task(nsCtx, nil)
		if terr != nil {
			return fmt.Errorf("container is not running")
		}
		sig := syscall.SIGKILL
		if signal != "" {
			if s, ok := signalValue(signal); ok {
				sig = s
			}
		}
		if kerr := task.Kill(nsCtx, sig); kerr != nil {
			return kerr
		}
	}
	// Docker reports SIGKILL'd containers with exit code 137 (128+9);
	// compose and the CLI rely on it in events and /wait. containerd's task
	// status often reports 0 for signal deaths, so cache the mapped code.
	if strings.EqualFold(signal, "SIGKILL") || signal == "9" || signal == "" {
		cacheContainerExitCode(dockerID(ns, containerdID), 137)
	}
	restarts.clear(dockerID(ns, containerdID))
	return nil
}

// restartDockerContainer restarts a container by Docker ID or name. Native
// implementation: graceful stop (the stopped task is deleted by the next
// start) followed by a fresh start with CNI re-attach.
func restartDockerContainer(ctx context.Context, id string, timeout int) error {
	ns, containerdID, _, err := resolveDockerID(ctx, id)
	if err != nil {
		return err
	}
	// Disarm during the stop so the monitor cannot race its own start in
	// between; re-armed below, as Docker keeps the policy across restart.
	did := dockerID(ns, containerdID)
	restarts.clear(did)
	if err := stopNativeTask(ctx, ns, containerdID, timeout); err != nil {
		restarts.rearm(did)
		return err
	}
	if err := startNativeTask(ctx, ns, containerdID); err != nil {
		return err
	}
	restarts.rearm(did)
	return nil
}
