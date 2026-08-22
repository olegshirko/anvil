package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// dockerCreateRequest mirrors the minimal parts of Docker's container creation body.
type dockerCreateRequest struct {
	Hostname         string                `json:"Hostname"`
	Domainname       string                `json:"Domainname"`
	User             string                `json:"User"`
	AttachStdin      bool                  `json:"AttachStdin"`
	AttachStdout     bool                  `json:"AttachStdout"`
	AttachStderr     bool                  `json:"AttachStderr"`
	Tty              bool                  `json:"Tty"`
	OpenStdin        bool                  `json:"OpenStdin"`
	StdinOnce        bool                  `json:"StdinOnce"`
	Env              []string              `json:"Env"`
	Cmd              []string              `json:"Cmd"`
	Entrypoint       []string              `json:"Entrypoint"`
	WorkingDir       string                `json:"WorkingDir"`
	StopSignal       string                `json:"StopSignal"`
	Image            string                `json:"Image"`
	Labels           map[string]string     `json:"Labels"`
	NetworkingConfig *dockerNetworkingConf `json:"NetworkingConfig,omitempty"`
	HostConfig       dockerHostConfig      `json:"HostConfig"`
	Healthcheck      *dockerHealthcheck    `json:"Healthcheck,omitempty"`
}

type dockerNetworkingConf struct {
	EndpointsConfig map[string]dockerEndpoint `json:"EndpointsConfig"`
}

type dockerEndpoint struct {
	Aliases []string `json:"Aliases"`
}

type dockerHostConfig struct {
	Binds           []string                    `json:"Binds"`
	Mounts          []dockerMount               `json:"Mounts"`
	NetworkMode     string                      `json:"NetworkMode"`
	PortBindings    map[string][]dockerHostPort `json:"PortBindings"`
	RestartPolicy   dockerRestartPolicy         `json:"RestartPolicy"`
	AutoRemove      bool                        `json:"AutoRemove"`
	Privileged      bool                        `json:"Privileged"`
	PublishAllPorts bool                        `json:"PublishAllPorts"`
	ExtraHosts      []string                    `json:"ExtraHosts"`
	Memory          int64                       `json:"Memory"`
	NanoCpus        int64                       `json:"NanoCpus"`
	CapAdd          []string                    `json:"CapAdd"`
	CapDrop         []string                    `json:"CapDrop"`
	ReadonlyRootfs  bool                        `json:"ReadonlyRootfs"`
	PidMode         string                      `json:"PidMode"`
	TmpFs           map[string]string           `json:"Tmpfs"`
	Dns             []string                    `json:"Dns"`
	Sysctls         map[string]string           `json:"Sysctls"`
	Devices         []dockerDevice              `json:"Devices"`
	Links           []string                    `json:"Links"`
}

type dockerHostPort struct {
	HostIp   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerMount struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}

type dockerDevice struct {
	PathOnHost        string `json:"PathOnHost"`
	PathInContainer   string `json:"PathInContainer"`
	CgroupPermissions string `json:"CgroupPermissions"`
}

type dockerRestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

type dockerHealthcheck struct {
	Test        []string `json:"Test"`
	Interval    int64    `json:"Interval"`
	Timeout     int64    `json:"Timeout"`
	Retries     int      `json:"Retries"`
	StartPeriod int64    `json:"StartPeriod,omitempty"`
}

type dockerCreateResponse struct {
	Id       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

type dockerWaitResponse struct {
	StatusCode int `json:"StatusCode"`
}

// autoRemoveContainers tracks Docker IDs that were created with AutoRemove.
// Removal happens in-agent after the exit code is captured so /wait can
// still read it.
var autoRemoveContainers = struct {
	mu  sync.RWMutex
	ids map[string]struct{}
}{
	ids: make(map[string]struct{}),
}

// containerTTYFlags remembers which containers were created with Tty=true:
// attach needs it to pick the raw (non-multiplexed) stream format, and
// inspect must return it.
var containerTTYFlags = struct {
	mu sync.RWMutex
	m  map[string]bool
}{
	m: make(map[string]bool),
}

// containerEntryPoints remembers WorkingDir/Entrypoint per container for
// inspect responses.
var containerEntryPoints = struct {
	mu    sync.RWMutex
	dirs  map[string]string
	eps   map[string][]string
	stops map[string]string
}{
	dirs:  make(map[string]string),
	eps:   make(map[string][]string),
	stops: make(map[string]string),
}

func setContainerStopSignal(dockerID, sig string) {
	containerEntryPoints.mu.Lock()
	if sig != "" {
		containerEntryPoints.stops[dockerID] = sig
	} else {
		delete(containerEntryPoints.stops, dockerID)
	}
	containerEntryPoints.mu.Unlock()
}

func getContainerStopSignal(dockerID string) string {
	containerEntryPoints.mu.RLock()
	defer containerEntryPoints.mu.RUnlock()
	return containerEntryPoints.stops[dockerID]
}

func setContainerEntryPointInfo(dockerID, workdir string, entrypoint []string) {
	containerEntryPoints.mu.Lock()
	containerEntryPoints.dirs[dockerID] = workdir
	if len(entrypoint) > 0 {
		containerEntryPoints.eps[dockerID] = entrypoint
	} else {
		delete(containerEntryPoints.eps, dockerID)
	}
	containerEntryPoints.mu.Unlock()
}

func getContainerWorkingDir(dockerID string) string {
	containerEntryPoints.mu.RLock()
	defer containerEntryPoints.mu.RUnlock()
	return containerEntryPoints.dirs[dockerID]
}

func getContainerEntrypoint(dockerID string) []string {
	containerEntryPoints.mu.RLock()
	defer containerEntryPoints.mu.RUnlock()
	return containerEntryPoints.eps[dockerID]
}

func setContainerTTY(dockerID string, tty bool) {
	containerTTYFlags.mu.Lock()
	if tty {
		containerTTYFlags.m[dockerID] = true
	} else {
		delete(containerTTYFlags.m, dockerID)
	}
	containerTTYFlags.mu.Unlock()
}

func getContainerTTY(dockerID string) bool {
	containerTTYFlags.mu.RLock()
	defer containerTTYFlags.mu.RUnlock()
	return containerTTYFlags.m[dockerID]
}

func markAutoRemove(dockerID string) {
	autoRemoveContainers.mu.Lock()
	autoRemoveContainers.ids[dockerID] = struct{}{}
	autoRemoveContainers.mu.Unlock()
}

func isAutoRemove(dockerID string) bool {
	autoRemoveContainers.mu.RLock()
	defer autoRemoveContainers.mu.RUnlock()
	_, ok := autoRemoveContainers.ids[dockerID]
	return ok
}

func unmarkAutoRemove(dockerID string) {
	autoRemoveContainers.mu.Lock()
	delete(autoRemoveContainers.ids, dockerID)
	autoRemoveContainers.mu.Unlock()
}

// attachTracker counts active attach connections per container. AutoRemove
// deletion waits for attaches to drain so a fast-exiting --rm container is
// not deleted before its output is replayed to the client.
var attachTracker = struct {
	mu     sync.Mutex
	counts map[string]int
}{
	counts: make(map[string]int),
}

func attachBegin(dockerID string) {
	attachTracker.mu.Lock()
	attachTracker.counts[dockerID]++
	attachTracker.mu.Unlock()
}

func attachEnd(dockerID string) {
	attachTracker.mu.Lock()
	if attachTracker.counts[dockerID] > 0 {
		attachTracker.counts[dockerID]--
	}
	attachTracker.mu.Unlock()
}

// waitForAttachDrain blocks until no attach connections remain for the
// container or the timeout elapses.
func waitForAttachDrain(dockerID string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		attachTracker.mu.Lock()
		n := attachTracker.counts[dockerID]
		attachTracker.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// containerExitCodes caches exit codes for containers that may be auto-removed
// before /wait can read their task status.
var containerExitCodes = struct {
	mu    sync.RWMutex
	codes map[string]int
}{
	codes: make(map[string]int),
}

func cacheContainerExitCode(dockerID string, code int) {
	containerExitCodes.mu.Lock()
	containerExitCodes.codes[dockerID] = code
	containerExitCodes.mu.Unlock()
}

func takeContainerExitCode(dockerID string) (int, bool) {
	containerExitCodes.mu.Lock()
	defer containerExitCodes.mu.Unlock()
	code, ok := containerExitCodes.codes[dockerID]
	if ok {
		delete(containerExitCodes.codes, dockerID)
	}
	return code, ok
}

// peekContainerExitCode reads the cached exit code without consuming it.
func peekContainerExitCode(dockerID string) (int, bool) {
	containerExitCodes.mu.RLock()
	defer containerExitCodes.mu.RUnlock()
	code, ok := containerExitCodes.codes[dockerID]
	return code, ok
}

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

// dockerContainerSummary matches the JSON returned by GET /containers/json.
type dockerContainerSummary struct {
	Id      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Command string            `json:"Command"`
	Created int64             `json:"Created"`
	Ports   []dockerPort      `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
}

// dockerContainerInspect is a minimal subset of GET /containers/{id}/json.
type dockerContainerInspect struct {
	Id              string                `json:"Id"`
	Name            string                `json:"Name"`
	Image           string                `json:"Image"`
	State           dockerContainerState  `json:"State"`
	Config          dockerContainerConfig `json:"Config"`
	HostConfig      dockerHostConfig      `json:"HostConfig"`
	NetworkSettings dockerNetworkSettings `json:"NetworkSettings"`
	// Docker exposes RestartCount at the top level (not under State).
	RestartCount int `json:"RestartCount"`
}

type dockerContainerState struct {
	Status   string             `json:"Status"`
	Running  bool               `json:"Running"`
	Pid      int                `json:"Pid"`
	ExitCode int                `json:"ExitCode"`
	Health   *dockerHealthState `json:"Health,omitempty"`
}

type dockerContainerConfig struct {
	Labels      map[string]string  `json:"Labels"`
	Image       string             `json:"Image"`
	Healthcheck *dockerHealthcheck `json:"Healthcheck,omitempty"`
	Tty         bool               `json:"Tty,omitempty"`
	OpenStdin   bool               `json:"OpenStdin,omitempty"`
	Env         []string           `json:"Env,omitempty"`
	Cmd         []string           `json:"Cmd,omitempty"`
	Entrypoint  []string           `json:"Entrypoint,omitempty"`
	WorkingDir  string             `json:"WorkingDir,omitempty"`
	StopSignal  string             `json:"StopSignal,omitempty"`
}

// dockerHostConfig (defined above with the create request) doubles as the
// inspect response shape: the docker CLI and compose dereference
// HostConfig.AutoRemove / PortBindings on inspect (e.g.
// cli/command/container/start.go — a missing object nil-panics the CLI).
// dockerRestartPolicy likewise already exists with the create request.

type dockerNetworkSettings struct {
	IPAddress string                         `json:"IPAddress"`
	Ports     map[string][]dockerHostPort    `json:"Ports,omitempty"`
	Networks  map[string]dockerEndpointStats `json:"Networks,omitempty"`
}

type dockerEndpointStats struct {
	IPAddress   string `json:"IPAddress"`
	IPPrefixLen int    `json:"IPPrefixLen"`
	MacAddress  string `json:"MacAddress,omitempty"`
}

// dockerPort matches the Docker API port binding shape.
type dockerPort struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}

// resolveDockerID maps a Docker ID prefix or container name to a containerd ID
// and its namespace. It returns an error if no unique match is found.
func resolveDockerID(ctx context.Context, prefix string) (ns, containerdID, name string, err error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("containerd client: %w", err)
	}

	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("list namespaces: %w", err)
	}

	type match struct {
		ns, id, name string
	}
	var matches []match
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			continue
		}
		for _, c := range containers {
			info, err := c.Info(nsCtx)
			if err != nil {
				continue
			}
			labels := info.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			cname := labels[labelName]
			if cname == "" {
				cname = c.ID()
			}

			if strings.HasPrefix(dockerID(ns, c.ID()), prefix) {
				matches = append(matches, match{ns, c.ID(), cname})
				continue
			}
			searchName := prefix
			if strings.HasPrefix(searchName, "/") {
				searchName = searchName[1:]
			}
			if cname == searchName {
				matches = append(matches, match{ns, c.ID(), cname})
			}
		}
	}

	if len(matches) == 0 {
		return "", "", "", fmt.Errorf("No such container: %s", prefix)
	}
	if len(matches) > 1 {
		return "", "", "", fmt.Errorf("multiple containers match %s", prefix)
	}
	return matches[0].ns, matches[0].id, matches[0].name, nil
}

// containerTaskState reads the task state for a container directly from
// containerd. Returns ("", false) when the task does not exist.
func containerTaskState(ctx context.Context, ns, id string) (running bool, status string, ok bool) {
	cl, err := pc.get(ctx)
	if err != nil {
		return false, "", false
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return false, "", false
	}
	task, err := c.Task(nsCtx, nil)
	if err != nil {
		// No task: the container exists but was never started (or its last
		// run's task is gone) — Docker reports "created"/"exited" here.
		return false, "created", true
	}
	st, err := task.Status(nsCtx)
	if err != nil {
		return false, "", false
	}
	return st.Status == "running", dockerStatus(string(st.Status)), true
}

// containerStatus returns the container's status string ("created",
// "running", "exited", ...) or "" when it cannot be determined. It reads
// containerd task state directly.
func containerStatus(ns, name string) string {
	id := resolveContainerdIDByName(context.Background(), ns, name)
	if id == "" {
		return ""
	}
	_, status, ok := containerTaskState(context.Background(), ns, id)
	if !ok {
		return ""
	}
	return status
}

func isNerdctlContainerRunning(ns, name string) bool {
	running, _, ok := func() (bool, string, bool) {
		ctx := context.Background()
		id := resolveContainerdIDByName(ctx, ns, name)
		if id == "" {
			return false, "", false
		}
		return containerTaskState(ctx, ns, id)
	}()
	return ok && running
}

// resolveContainerdIDByName finds the containerd ID for a container name
// (label lookup) within one namespace.
func resolveContainerdIDByName(ctx context.Context, ns, name string) string {
	cl, err := pc.get(ctx)
	if err != nil {
		return ""
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	containers, err := cl.Containers(nsCtx)
	if err != nil {
		return ""
	}
	for _, c := range containers {
		labels, err := c.Labels(nsCtx)
		if err != nil {
			continue
		}
		cn := labels[labelName]
		if cn == "" {
			cn = c.ID()
		}
		if cn == name || c.ID() == name {
			return c.ID()
		}
	}
	return ""
}

// findContainerByName returns the Docker ID of a container with the given name
// in the given namespace, or an empty string if none exists.
func findContainerByName(ctx context.Context, ns, name string) (string, error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return "", err
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	containers, err := cl.Containers(nsCtx)
	if err != nil {
		return "", err
	}
	for _, c := range containers {
		labels, err := c.Labels(nsCtx)
		if err != nil {
			continue
		}
		if labels[labelName] == name {
			return dockerID(ns, c.ID()), nil
		}
	}
	return "", nil
}

// createDockerContainer creates a container natively via containerd and
// returns its Docker ID.
func createDockerContainer(ctx context.Context, req dockerCreateRequest, name string) (string, error) {
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
		if err := generateCNIConfig(effectiveNetworkName(networkMode)); err != nil {
			log.Printf("[docker-api] ensure cni config for %s: %v", networkMode, err)
		}
	}

	// Ensure the image metadata exists in the target namespace. The content
	// store is shared, so when the image already exists elsewhere we copy its
	// metadata instead of re-pulling, which avoids corrupting the shared content
	// store when docker compose creates multiple containers in parallel.
	if err := ensureImageInNamespace(ctx, req.Image, ns); err != nil {
		return "", err
	}

	// Docker refuses duplicate names; mimic that to avoid ambiguous lookups later.
	if name != "" {
		if existing, err := findContainerByName(ctx, ns, name); err == nil && existing != "" {
			return "", fmt.Errorf("Conflict. The container name \"/%s\" is already in use by container \"%s\". You have to remove (or rename) that container to be able to reuse that name.", name, existing)
		}
	}

	containerdID, err := createNativeContainer(ctx, ns, name, req)
	if err != nil {
		return "", err
	}

	dockerID := dockerID(ns, containerdID)
	// Remember compose service aliases for the start-time /etc/hosts update.
	if aliases := requestedNetworkAliases(req); len(aliases) > 0 {
		setNetworkAliases(dockerID, aliases)
	}
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
		restarts.register(dockerID, p.name, p.max)
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
	return dockerID, nil
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
// read from the nerdctl port-mapping metadata.
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
	// records). nerdctl prepares the hosts file at create; the post-start
	// application below covers restarts and any nerdctl rewrites.
	if links := pendingLinkEntries(dockerID(ns, containerdID)); len(links) > 0 {
		applyLinkAliases(ctx, ns, containerdID, links)
	}
	// Native start: CNI attach + task creation + json-file logging.
	if err := startNativeTask(ctx, ns, containerdID); err != nil {
		return err
	}
	// Service-name DNS: append the container's compose aliases (e.g. "web")
	// to the /etc/hosts bind mounts of every running container on the same
	// network. nerdctl has no --network-alias, and compose resolves service
	// dependencies by name (`db`, `web`), not by container name.
	if len(pendingNetworkAliases(containerdID)) > 0 {
		go applyNetworkAliases(ns, containerdID)
	}
	// docker --link: append alias -> target-IP entries to THIS container's
	// /etc/hosts (legacy link semantics are plain /etc/hosts records).
	if links := pendingLinkEntries(dockerID(ns, containerdID)); len(links) > 0 {
		go applyLinkAliases(context.Background(), ns, containerdID, links)
	}
	// Re-attach the health monitor after a stop/start cycle.
	did := dockerID(ns, containerdID)
	if hc := getHealthcheckConfig(did); hc != nil {
		startHealthCheck(did, ns, containerdID, hc, getHealthcheckUser(did))
	}

	// For AutoRemove containers we wait for the exit code ourselves and then
	// delete the container. nerdctl --rm would delete too early for /wait.
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
// the containerd task actually reaches the stopped state. nerdctl stop can
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
	// AutoRemove containers may already be deleted; their exit code was cached.
	if code, ok := takeContainerExitCode(id); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"StatusCode":%d}`, code)
		return
	}

	ns, containerdID, _, err := resolveDockerID(r.Context(), id)
	if err != nil {
		// The container may have been auto-removed before we could cache the code.
		if code, ok := takeContainerExitCode(id); ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"StatusCode":%d}`, code)
			return
		}
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
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
	}
	cacheContainerExitCode(id, exitCode)
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
	stopHealthCheck(did)
	restarts.clear(did)
	unmarkAutoRemove(did)
	takeContainerExitCode(did)
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
	if err := stopNativeTask(ctx, ns, containerdID, timeout); err != nil {
		return err
	}
	return startNativeTask(ctx, ns, containerdID)
}

// listDockerContainers scans all containerd namespaces and returns a
// Docker-compatible summary for each container.
// matchesContainerFilters applies the ps filters Docker CLI sends: keys are
// AND-ed, values within a key are OR-ed. Supported: label (see
// matchesLabelFilters), name (substring), id (prefix), status (exact).
func matchesContainerFilters(s dockerContainerSummary, filters map[string]map[string]bool) bool {
	if !matchesLabelFilters(s.Labels, filters) {
		return false
	}
	if pats := filters["name"]; len(pats) > 0 {
		name := strings.TrimPrefix(s.Names[0], "/")
		matched := false
		for pat := range pats {
			if pat != "" && strings.Contains(name, pat) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if pats := filters["id"]; len(pats) > 0 {
		matched := false
		for pat := range pats {
			if strings.HasPrefix(s.Id, pat) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if statuses := filters["status"]; len(statuses) > 0 {
		matched := false
		for st := range statuses {
			if st == s.State {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func listDockerContainers(ctx context.Context, filters map[string]map[string]bool) ([]dockerContainerSummary, error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}

	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	result := make([]dockerContainerSummary, 0)
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			log.Printf("[docker-api] list containers in %s: %v", ns, err)
			continue
		}

		for _, c := range containers {
			info, err := c.Info(nsCtx)
			if err != nil {
				continue
			}

			labels := info.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			name := labels[labelName]
			if name == "" {
				name = c.ID()
			}

			state := "created"
			status := "created"
			task, err := c.Task(nsCtx, nil)
			if err == nil {
				st, err := task.Status(nsCtx)
				if err == nil {
					state = dockerState(string(st.Status))
					status = dockerStatus(string(st.Status))
				}
			}

			var ports []dockerPort
			if portsJSON := portsLabel(c, nsCtx); portsJSON != "" {
				var pm []cniPortMapping
				if err := json.Unmarshal([]byte(portsJSON), &pm); err == nil {
					for _, p := range pm {
						proto := p.Protocol
						if proto == "" {
							proto = "tcp"
						}
						hostIP := p.HostIP
						if hostIP == "" {
							hostIP = "0.0.0.0"
						}
						ports = append(ports, dockerPort{
							IP:          hostIP,
							PrivatePort: p.ContainerPort,
							PublicPort:  p.HostPort,
							Type:        proto,
						})
					}
				}
			}

			imageName := info.Image
			if img, err := c.Image(nsCtx); err == nil && img != nil {
				imageName = img.Name()
			}

			did := dockerID(ns, c.ID())
			summary := dockerContainerSummary{
				Id:      did,
				Names:   []string{"/" + name},
				Image:   imageName,
				ImageID: info.Image,
				Command: "", // not trivial to extract from OCI spec; left empty
				Created: info.CreatedAt.Unix(),
				Ports:   ports,
				Labels:  labels,
				State:   state,
				Status:  formatHealthStatus(did, status),
			}
			if matchesContainerFilters(summary, filters) {
				result = append(result, summary)
			}
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Id < result[j].Id
	})
	return result, nil
}

// inspectDockerContainer returns a minimal inspect payload for a container.
// The lookup accepts a Docker ID prefix, a container name, or a leading-slash
// name as returned by `docker ps`.
func inspectDockerContainer(ctx context.Context, prefix string) (*dockerContainerInspect, error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}

	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			continue
		}
		for _, c := range containers {
			info, err := c.Info(nsCtx)
			if err != nil {
				continue
			}

			labels := info.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			name := labels[labelName]
			if name == "" {
				name = c.ID()
			}

			// Match by Docker ID prefix or by name (with or without leading slash).
			matched := strings.HasPrefix(dockerID(ns, c.ID()), prefix)
			if !matched {
				searchName := prefix
				if strings.HasPrefix(searchName, "/") {
					searchName = searchName[1:]
				}
				if name == searchName {
					matched = true
				}
			}
			if !matched {
				continue
			}

			status := "created"
			running := false
			exitCode := 0
			pid := 0
			task, err := c.Task(nsCtx, nil)
			if err == nil {
				if st, serr := task.Status(nsCtx); serr == nil {
					status = dockerStatus(string(st.Status))
					running = st.Status == "running"
					if st.Status == "stopped" {
						exitCode = int(st.ExitStatus)
					}
					pid = int(task.Pid())
				}
			}

			imageName := info.Image
			if img, err := c.Image(nsCtx); err == nil && img != nil {
				imageName = img.Name()
			}

			did := dockerID(ns, c.ID())
			if exitCode == 0 && !running {
				if cached, ok := peekContainerExitCode(did); ok {
					exitCode = cached
				}
			}
			meta, _ := loadContainerMeta(ns, c.ID())

			// Process config comes from the OCI spec; env/cmd reflect what
			// will actually run (image config merged with overrides).
			envList, cmdList, openStdin := []string{}, []string{}, false
			if spec, err := c.Spec(nsCtx); err == nil && spec != nil && spec.Process != nil {
				envList = spec.Process.Env
				cmdList = spec.Process.Args
			}
			networkName := "bridge"
			if meta != nil && len(meta.Networks) > 0 {
				networkName = meta.Networks[0]
			}
			endpoint := dockerEndpointStats{}
			if ni, ok := loadNetInfo(ns, c.ID()); ok {
				endpoint = dockerEndpointStats{IPAddress: ni.IP, MacAddress: ni.Mac}
			} else if usesHostNetworkName(networkName) {
				endpoint = dockerEndpointStats{IPAddress: detectGuestIP()}
			}
			containerIP := endpoint.IPAddress
			if containerIP == "" {
				containerIP = detectGuestIP()
			}
			var portBindings map[string][]dockerHostPort
			if meta != nil {
				portBindings = portBindingsFromMeta(meta)
			}
			return &dockerContainerInspect{
				Id:    did,
				Name:  "/" + name,
				Image: imageName,
				State: dockerContainerState{
					Status:   status,
					Running:  running,
					Pid:      pid,
					ExitCode: exitCode,
					Health:   getHealthState(did),
				},
				RestartCount: restarts.countFor(did),
				Config: dockerContainerConfig{
					Labels:      labels,
					Image:       imageName,
					Healthcheck: getHealthcheckConfig(did),
					Tty:         getContainerTTY(did),
					OpenStdin:   openStdin,
					Env:         envList,
					Cmd:         cmdList,
					Entrypoint:  getContainerEntrypoint(did),
					WorkingDir:  getContainerWorkingDir(did),
					StopSignal:  getContainerStopSignal(did),
				},
				HostConfig: dockerHostConfig{
					AutoRemove:   isAutoRemove(did),
					NetworkMode:  networkName,
					PortBindings: portBindings,
					// The restart policy is owned by our monitor; report it
					// from the registry.
					RestartPolicy: restarts.policySpecFor(did),
				},
				NetworkSettings: dockerNetworkSettings{
					IPAddress: containerIP,
					Ports:     portBindings,
					Networks:  map[string]dockerEndpointStats{networkName: endpoint},
				},
			}, nil
		}
	}
	return nil, fmt.Errorf("No such container: %s", prefix)
}

// containerNetworkInfo returns the container's primary network endpoint and
// its CNI-assigned address. State comes from the persisted net.json written
// at start time (the CNI attach result), not from a runtime round-trip.
func containerNetworkInfo(ns, containerdID, name string) (map[string]dockerEndpointStats, string) {
	primary := dockerEndpointStats{}
	networkName := "bridge"
	if meta, err := loadContainerMeta(ns, containerdID); err == nil && len(meta.Networks) > 0 {
		networkName = meta.Networks[0]
	}
	if ni, ok := loadNetInfo(ns, containerdID); ok {
		primary = dockerEndpointStats{IPAddress: ni.IP, MacAddress: ni.Mac}
	} else if usesHostNetworkName(networkName) {
		primary.IPAddress = detectGuestIP()
	}
	networks := map[string]dockerEndpointStats{networkName: primary}
	return networks, primary.IPAddress
}

// portBindingsFromMeta renders the persisted port mappings back into the
// Docker HostConfig.PortBindings shape: "<containerPort>/<proto>" ->
// [{HostIp, HostPort}].
func portBindingsFromMeta(meta *containerMeta) map[string][]dockerHostPort {
	bindings := map[string][]dockerHostPort{}
	if meta == nil {
		return bindings
	}
	for _, m := range meta.Ports {
		proto := m.Protocol
		if proto == "" {
			proto = "tcp"
		}
		key := fmt.Sprintf("%d/%s", m.ContainerPort, proto)
		hostIP := m.HostIP
		if hostIP == "" {
			hostIP = "0.0.0.0"
		}
		bindings[key] = append(bindings[key], dockerHostPort{
			HostIp:   hostIP,
			HostPort: strconv.Itoa(m.HostPort),
		})
	}
	return bindings
}
