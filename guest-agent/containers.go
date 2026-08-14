package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// dockerCreateRequest mirrors the minimal parts of Docker's container creation body.
type dockerCreateRequest struct {
	Hostname     string             `json:"Hostname"`
	Domainname   string             `json:"Domainname"`
	User         string             `json:"User"`
	AttachStdin  bool               `json:"AttachStdin"`
	AttachStdout bool               `json:"AttachStdout"`
	AttachStderr bool               `json:"AttachStderr"`
	Tty          bool               `json:"Tty"`
	OpenStdin    bool               `json:"OpenStdin"`
	StdinOnce    bool               `json:"StdinOnce"`
	Env          []string           `json:"Env"`
	Cmd          []string           `json:"Cmd"`
	Image        string             `json:"Image"`
	Labels       map[string]string  `json:"Labels"`
	HostConfig   dockerHostConfig   `json:"HostConfig"`
	Healthcheck  *dockerHealthcheck `json:"Healthcheck,omitempty"`
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
// We do not pass --rm to nerdctl so that we can capture the exit code before
// the container disappears.
var autoRemoveContainers = struct {
	mu  sync.RWMutex
	ids map[string]struct{}
}{
	ids: make(map[string]struct{}),
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

// waitContainerTask blocks until the containerd task exits and returns its exit
// code. It returns an error if the task does not appear or is already deleted.
func waitContainerTask(ns, containerdID string) (int, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return 0, err
	}
	defer cl.Close()

	nsCtx := namespaces.WithNamespace(context.Background(), ns)

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

	// Use nerdctl wait to reliably block until the task exits and returns its
	// exit code. The containerd client task.Wait can race with short-lived tasks.
	stdout, stderr, code, err := runNerdctl(ns, "wait", containerdID)
	if err != nil || code != 0 {
		return 0, fmt.Errorf("nerdctl wait failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	exit, _ := strconv.Atoi(strings.TrimSpace(stdout))
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
	NetworkSettings dockerNetworkSettings `json:"NetworkSettings"`
}

type dockerContainerState struct {
	Status  string             `json:"Status"`
	Running bool               `json:"Running"`
	Health  *dockerHealthState `json:"Health,omitempty"`
}

type dockerContainerConfig struct {
	Labels      map[string]string  `json:"Labels"`
	Image       string             `json:"Image"`
	Healthcheck *dockerHealthcheck `json:"Healthcheck,omitempty"`
}

type dockerNetworkSettings struct {
	IPAddress string                         `json:"IPAddress"`
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
func resolveDockerID(prefix string) (ns, containerdID, name string, err error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return "", "", "", fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
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
			cname := labels["nerdctl/name"]
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

// runNerdctl runs a nerdctl command in the given namespace and returns output.
func runNerdctl(ns string, args ...string) (stdout, stderr string, exitCode int, err error) {
	cmdArgs := append([]string{"-n", ns}, args...)
	cmd := exec.Command("/opt/containerd/bin/nerdctl", cmdArgs...)
	cmd.Env = append(cmd.Env, "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	return outBuf.String(), errBuf.String(), exitCode, err
}

// containerStateFromInspect parses `nerdctl inspect --format json` output.
// nerdctl >= 2.1 prints a single JSON object; older versions printed an
// array with one element. Accept both.
func containerStateFromInspect(stdout string) (running bool, status string, ok bool) {
	type containerState struct {
		Running bool   `json:"Running"`
		Status  string `json:"Status"`
	}
	type containerInfo struct {
		State containerState `json:"State"`
	}
	var arr []containerInfo
	if err := json.Unmarshal([]byte(stdout), &arr); err == nil && len(arr) > 0 {
		return arr[0].State.Running, arr[0].State.Status, true
	}
	var single containerInfo
	if err := json.Unmarshal([]byte(stdout), &single); err == nil && single.State.Status != "" {
		return single.State.Running, single.State.Status, true
	}
	return false, "", false
}

// isNerdctlContainerRunning reports whether the named container is currently
// running, using nerdctl inspect.
// nerdctlContainerStatus returns the container's status string ("created",
// "running", "exited", ...) or "" when it cannot be determined.
func nerdctlContainerStatus(ns, name string) string {
	stdout, _, code, err := runNerdctl(ns, "inspect", "--format", "json", name)
	if err != nil || code != 0 {
		return ""
	}
	_, status, _ := containerStateFromInspect(stdout)
	return status
}

func isNerdctlContainerRunning(ns, name string) bool {
	stdout, _, code, err := runNerdctl(ns, "inspect", "--format", "json", name)
	if err != nil || code != 0 {
		return false
	}
	running, status, ok := containerStateFromInspect(stdout)
	return ok && (running || status == "running")
}

// findContainerByName returns the Docker ID of a container with the given name
// in the given namespace, or an empty string if none exists.
func findContainerByName(ns, name string) (string, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return "", err
	}
	defer cl.Close()
	nsCtx := namespaces.WithNamespace(context.Background(), ns)
	containers, err := cl.Containers(nsCtx)
	if err != nil {
		return "", err
	}
	for _, c := range containers {
		labels, err := c.Labels(nsCtx)
		if err != nil {
			continue
		}
		if labels["nerdctl/name"] == name {
			return dockerID(ns, c.ID()), nil
		}
	}
	return "", nil
}

// createDockerContainer creates a container via nerdctl and returns its Docker ID.
func createDockerContainer(req dockerCreateRequest, name string) (string, error) {
	networkMode := req.HostConfig.NetworkMode
	ns := namespaceFromNetwork(networkMode)
	// Compose attaches containers to a network named <project>_<network>. The
	// container itself carries the project label, which is the authoritative
	// containerd namespace.
	if project := req.Labels["com.docker.compose.project"]; project != "" {
		ns = project
	}
	log.Printf("[docker-api] create container %q image=%q network=%q namespace=%q", name, req.Image, networkMode, ns)

	// nerdctl invoked directly (not through the wrapper script) does not create
	// the CNI conflist. Make sure it exists before creating the container,
	// otherwise the CNI hook fails with "no such network".
	if err := generateCNIConfig(networkMode); err != nil {
		log.Printf("[docker-api] ensure cni config for %s: %v", networkMode, err)
	}

	// Ensure the image metadata exists in the target namespace. The content
	// store is shared, so when the image already exists elsewhere we copy its
	// metadata instead of re-pulling, which avoids corrupting the shared content
	// store when docker compose creates multiple containers in parallel.
	if err := ensureImageInNamespace(req.Image, ns); err != nil {
		return "", err
	}

	// nerdctl leaves stale name-to-ID files in /var/lib/nerdctl when its metadata
	// and containerd's bolt DB drift (e.g. after cold boot or forced cleanup).
	// Remove any existing name-store file for this name before create, otherwise
	// `nerdctl create --name <name>` fails with "name ... is already used".
	if name != "" {
		removeNerdctlNameStoreByName(ns, name)
	}

	// Docker refuses duplicate names; mimic that to avoid ambiguous lookups later.
	if name != "" {
		if existing, err := findContainerByName(ns, name); err == nil && existing != "" {
			return "", fmt.Errorf("Conflict. The container name \"/%s\" is already in use by container \"%s\". You have to remove (or rename) that container to be able to reuse that name.", name, existing)
		}
	}

	args := []string{"create"}
	if networkMode != "" && networkMode != "default" {
		args = append(args, "--net", networkMode)
	}
	// json-file logger makes `nerdctl logs -f` reliable enough for attach.
	args = append(args, "--log-driver", "json-file")
	if name != "" {
		args = append(args, "--name", name)
	}
	if req.Hostname != "" {
		args = append(args, "--hostname", req.Hostname)
	}
	if req.User != "" {
		args = append(args, "--user", req.User)
	}
	for _, e := range req.Env {
		args = append(args, "-e", e)
	}
	for k, v := range req.Labels {
		args = append(args, "-l", k+"="+v)
	}
	if req.HostConfig.Privileged {
		args = append(args, "--privileged")
	}
	if req.HostConfig.RestartPolicy.Name != "" {
		args = append(args, "--restart", req.HostConfig.RestartPolicy.Name)
	}

	// Bind mounts and named volumes. Host paths under /Users are visible in
	// the guest at the same absolute path via the "macusers" virtiofs share,
	// so binds pass through unchanged (like Docker Desktop). Paths outside
	// the shared trees refer to the guest's own filesystem.
	for _, bind := range req.HostConfig.Binds {
		args = append(args, "-v", bind)
	}
	for _, m := range req.HostConfig.Mounts {
		if m.Type == "tmpfs" || m.Source == "" || m.Target == "" {
			continue
		}
		spec := m.Source + ":" + m.Target
		if m.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "-v", spec)
	}

	// Port bindings: containerPort/proto -> [{HostIp, HostPort}]
	for cportSpec, hostPorts := range req.HostConfig.PortBindings {
		proto := "tcp"
		parts := strings.SplitN(cportSpec, "/", 2)
		cport := parts[0]
		if len(parts) == 2 {
			proto = parts[1]
		}
		for _, hp := range hostPorts {
			host := hp.HostPort
			if host == "" {
				continue
			}
			spec := host + ":" + cport
			if proto != "tcp" {
				spec += "/" + proto
			}
			args = append(args, "-p", spec)
		}
	}

	args = append(args, req.Image)
	args = append(args, req.Cmd...)

	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 {
		return "", fmt.Errorf("nerdctl create failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}

	// nerdctl create prints the container name/ID to stdout.
	createdName := strings.TrimSpace(stdout)
	if createdName == "" {
		return "", fmt.Errorf("nerdctl create returned empty ID")
	}

	// Look up the container by the ID nerdctl printed, then by requested name.
	cl, err := client.New(containerdSocket)
	if err != nil {
		return "", fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nsCtx := namespaces.WithNamespace(ctx, ns)

	var containerdID string
	// nerdctl create usually prints the containerd ID; try loading it directly.
	if c, err := cl.LoadContainer(nsCtx, createdName); err == nil {
		containerdID = c.ID()
	} else {
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			return "", fmt.Errorf("list containers: %w", err)
		}
		searchName := name
		if searchName == "" {
			searchName = createdName
		}
		for _, c := range containers {
			labels, err := c.Labels(nsCtx)
			if err != nil {
				continue
			}
			if labels["nerdctl/name"] == searchName {
				containerdID = c.ID()
				break
			}
		}
	}
	if containerdID == "" {
		return "", fmt.Errorf("could not find created container %s", createdName)
	}

	dockerID := dockerID(ns, containerdID)
	// AutoRemove is handled by guest-agent after we capture the exit code.
	// Passing --rm to nerdctl would delete the container immediately on exit
	// and cause /wait to miss the exit status.
	if req.HostConfig.AutoRemove {
		markAutoRemove(dockerID)
	}
	// nerdctl 2.0.4 does not implement Docker healthchecks, so we run the
	// configured check ourselves via nerdctl exec on a ticker. Store the
	// configuration at create time and start the ticker when the container
	// is started so the first check does not run against a created-but-not
	// yet-running container.
	containerUser := ""
	if c, err := cl.LoadContainer(nsCtx, containerdID); err == nil {
		if spec, err := c.Spec(nsCtx); err == nil && spec != nil && spec.Process != nil {
			containerUser = spec.Process.User.Username
		}
	}
	setHealthcheckConfig(dockerID, req.Healthcheck, containerUser)
	return dockerID, nil
}

// startDockerContainer starts a container by Docker ID or name.
// pruneStaleNetworkStore removes nerdctl network-store entries
// (/var/lib/nerdctl/<hash>/containers/<ns>/<id>) whose container no longer
// exists in containerd. The cold-boot metadata cleanup in stage2 removes
// containers bypassing `nerdctl rm`, so their store entries would otherwise
// accumulate forever on the persistent disk.
func pruneStaleNetworkStore() {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return
	}
	defer cl.Close()

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

	matches, _ := filepath.Glob("/var/lib/nerdctl/*/containers/*/*")
	for _, dir := range matches {
		parts := strings.Split(dir, "/")
		if len(parts) < 2 {
			continue
		}
		id := parts[len(parts)-1]
		ns := parts[len(parts)-2]
		if ids, ok := live[ns]; !ok || !ids[id] {
			debugLog("pruning stale nerdctl store entry %s/%s", ns, id)
			os.RemoveAll(dir)
		}
	}
}

// containerHostPorts returns the TCP host ports published by the container,
// read from the nerdctl port-mapping metadata.
func containerHostPorts(ns, containerdID string) []int {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil
	}
	defer cl.Close()
	nsCtx := namespaces.WithNamespace(context.Background(), ns)
	c, err := cl.LoadContainer(nsCtx, containerdID)
	if err != nil {
		return nil
	}
	portsJSON := getNerdctlPortsLabel(c, nsCtx)
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

func startDockerContainer(id string) error {
	ns, containerdID, _, err := resolveDockerID(id)
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
	if ports := containerHostPorts(ns, containerdID); len(ports) > 0 {
		if conflict, err := findHostPortConflict(ports, containerdID); err == nil && conflict != nil {
			log.Printf("[portcheck] start %s: host port %d already published by container %q (%s) in ns %q",
				containerdID, conflict.HostPort, conflict.Name, conflict.ContainerID, conflict.Namespace)
			return fmt.Errorf("Bind for 0.0.0.0:%d failed: port is already allocated", conflict.HostPort)
		}
		if busy := busyForeignHostPorts(ports); len(busy) > 0 {
			log.Printf("[portcheck] start %s: host port %d bound by a foreign host process", containerdID, busy[0])
			return fmt.Errorf("Bind for 0.0.0.0:%d failed: port is already allocated", busy[0])
		}
	}

	stdout, stderr, code, err := runNerdctl(ns, "start", containerdID)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl start failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
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
			code, _ := waitContainerTask(ns, containerdID)
			cacheContainerExitCode(did, code)
			// Let any attach connection finish replaying the output before
			// the container (and its logs) disappear.
			waitForAttachDrain(did, 30*time.Second)
			if err := deleteDockerContainer(did, true); err != nil {
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
func stopDockerContainer(id string, timeout int) error {
	ns, containerdID, _, err := resolveDockerID(id)
	if err != nil {
		return err
	}
	did := dockerID(ns, containerdID)
	args := []string{"stop"}
	if timeout > 0 {
		args = append(args, "-t", strconv.Itoa(timeout))
	}
	args = append(args, containerdID)
	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl stop failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}

	// Poll containerd until the task stops or disappears.
	waitDeadline := time.Now().Add(30 * time.Second)
	for {
		cl, err := client.New(containerdSocket)
		if err != nil {
			break
		}
		nsCtx := namespaces.WithNamespace(context.Background(), ns)
		c, err := cl.LoadContainer(nsCtx, containerdID)
		cl.Close()
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

	ns, containerdID, _, err := resolveDockerID(id)
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

	exitCode, err := waitContainerTask(ns, containerdID)
	if err != nil {
		log.Printf("[docker-api] wait %s task error: %v", id, err)
	}
	cacheContainerExitCode(id, exitCode)
	log.Printf("[docker-api] wait %s returning StatusCode=%d", id, exitCode)
	fmt.Fprintf(w, `{"StatusCode":%d}`, exitCode)
}

// deleteDockerContainer removes a container by Docker ID or name.
// removeNerdctlNameStore removes stale name-to-ID mappings that nerdctl leaves
// behind after `nerdctl rm`. Without this, `nerdctl create --name <name>` later
// fails with "name <name> is already used by ID <id>".
// nerdctl stores names under /var/lib/nerdctl/<datastore>/<namespace>/<name>.
func removeNerdctlNameStore(ns, containerdID string) {
	base := "/var/lib/nerdctl"
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		nsDir := filepath.Join(base, e.Name(), ns)
		subs, err := os.ReadDir(nsDir)
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if sub.IsDir() {
				continue
			}
			path := filepath.Join(nsDir, sub.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if strings.TrimSpace(string(data)) == containerdID {
				_ = os.Remove(path)
			}
		}
	}
}

// removeNerdctlNameStoreByName removes any name-store file for the given
// namespace and container name, regardless of which container ID it points to.
// This fixes drift between containerd metadata and nerdctl's name store after
// cold boot or snapshot resume.
func removeNerdctlNameStoreByName(ns, name string) {
	base := "/var/lib/nerdctl"
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(base, e.Name(), ns, name)
		if _, err := os.Stat(path); err == nil {
			_ = os.Remove(path)
		}
	}
}

func deleteDockerContainer(id string, force bool) error {
	ns, containerdID, _, err := resolveDockerID(id)
	if err != nil {
		return err
	}
	did := dockerID(ns, containerdID)
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, containerdID)
	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl rm failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	removeNerdctlNameStore(ns, containerdID)
	// nerdctl rm does not always remove the container's network-store entry;
	// drop it so stale port mappings do not accumulate on the persistent disk.
	if matches, _ := filepath.Glob("/var/lib/nerdctl/*/containers/" + ns + "/" + containerdID); len(matches) > 0 {
		for _, dir := range matches {
			os.RemoveAll(dir)
		}
	}
	stopHealthCheck(did)
	unmarkAutoRemove(did)
	takeContainerExitCode(did)
	return nil
}

// renameDockerContainer implements POST /containers/{id}/rename. Compose
// uses it in the recreate flow: the replacement is created under a temporary
// name and renamed once the old container is removed.
func renameDockerContainer(id string, newName string) error {
	ns, _, name, err := resolveDockerID(id)
	if err != nil {
		return err
	}
	if newName == "" {
		return fmt.Errorf("name is required")
	}
	stdout, stderr, code, err := runNerdctl(ns, "rename", name, newName)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl rename failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	return nil
}

// killDockerContainer kills a container by Docker ID or name.
func killDockerContainer(id string, signal string) error {
	ns, containerdID, _, err := resolveDockerID(id)
	if err != nil {
		return err
	}
	args := []string{"kill"}
	if signal != "" {
		args = append(args, "-s", signal)
	}
	args = append(args, containerdID)
	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl kill failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	return nil
}

// restartDockerContainer restarts a container by Docker ID or name.
func restartDockerContainer(id string, timeout int) error {
	ns, containerdID, _, err := resolveDockerID(id)
	if err != nil {
		return err
	}
	args := []string{"restart"}
	if timeout > 0 {
		args = append(args, "-t", strconv.Itoa(timeout))
	}
	args = append(args, containerdID)
	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl restart failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	return nil
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

func listDockerContainers(filters map[string]map[string]bool) ([]dockerContainerSummary, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
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
			name := labels["nerdctl/name"]
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
			if portsJSON := getNerdctlPortsLabel(c, nsCtx); portsJSON != "" {
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
func inspectDockerContainer(prefix string) (*dockerContainerInspect, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
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
			name := labels["nerdctl/name"]
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
			task, err := c.Task(nsCtx, nil)
			if err == nil {
				st, err := task.Status(nsCtx)
				if err == nil {
					status = dockerStatus(string(st.Status))
					running = st.Status == "running"
				}
			}

			imageName := info.Image
			if img, err := c.Image(nsCtx); err == nil && img != nil {
				imageName = img.Name()
			}

			did := dockerID(ns, c.ID())
			networks, containerIP := containerNetworkInfo(ns, c.ID(), name)
			if containerIP == "" {
				containerIP = detectGuestIP()
			}
			return &dockerContainerInspect{
				Id:    did,
				Name:  "/" + name,
				Image: imageName,
				State: dockerContainerState{
					Status:  status,
					Running: running,
					Health:  getHealthState(did),
				},
				Config: dockerContainerConfig{
					Labels:      labels,
					Image:       imageName,
					Healthcheck: getHealthcheckConfig(did),
				},
				NetworkSettings: dockerNetworkSettings{
					IPAddress: containerIP,
					Networks:  networks,
				},
			}, nil
		}
	}
	return nil, fmt.Errorf("No such container: %s", prefix)
}

// containerNetworkInfo returns the container's per-network endpoints for
// docker inspect (NetworkSettings.Networks) and its primary IP. nerdctl's own
// inspect knows the CNI-assigned address, but keys the map by interface name
// ("unknown-eth0") — the real network name comes from HostConfig.NetworkMode.
type nerdctlInspectNetInfo struct {
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		IPAddress   string `json:"IPAddress"`
		IPPrefixLen int    `json:"IPPrefixLen"`
		MacAddress  string `json:"MacAddress"`
		Networks    map[string]struct {
			IPAddress   string `json:"IPAddress"`
			IPPrefixLen int    `json:"IPPrefixLen"`
			MacAddress  string `json:"MacAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func containerNetworkInfo(ns, containerdID, name string) (map[string]dockerEndpointStats, string) {
	stdout, _, code, err := runNerdctl(ns, "inspect", "--format", "json", containerdID)
	if err != nil || code != 0 {
		return nil, ""
	}
	var info nerdctlInspectNetInfo
	if err := json.Unmarshal([]byte(stdout), &info); err != nil {
		return nil, ""
	}

	primary := dockerEndpointStats{
		IPAddress:   info.NetworkSettings.IPAddress,
		IPPrefixLen: info.NetworkSettings.IPPrefixLen,
		MacAddress:  info.NetworkSettings.MacAddress,
	}
	networkName := info.HostConfig.NetworkMode
	if networkName == "" || networkName == "default" || networkName == "bridge" {
		networkName = "bridge"
	}
	networks := map[string]dockerEndpointStats{networkName: primary}
	return networks, primary.IPAddress
}
