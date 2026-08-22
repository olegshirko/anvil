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
// We do not pass --rm to nerdctl so that we can capture the exit code before
// the container disappears.
var autoRemoveContainers = struct {
	mu  sync.RWMutex
	ids map[string]struct{}
}{
	ids: make(map[string]struct{}),
}

// containerTTYFlags remembers which containers were created with Tty=true.
// nerdctl's inspect does not report the flag, but attach needs it to pick
// the raw (non-multiplexed) stream format, and inspect must return it.
var containerTTYFlags = struct {
	mu sync.RWMutex
	m  map[string]bool
}{
	m: make(map[string]bool),
}

// containerEntryPoints remembers WorkingDir/Entrypoint per container for
// inspect responses (nerdctl's inspect does not report them).
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

	// Use nerdctl wait to reliably block until the task exits and returns its
	// exit code. The containerd client task.Wait can race with short-lived tasks.
	// A cached code (e.g. the mapped 137 from docker kill) wins over the
	// task status, which reports 0 for signal deaths.
	did := dockerID(ns, containerdID)
	stdout, stderr, code, err := runNerdctl(ns, "wait", containerdID)
	if err != nil || code != 0 {
		return 0, fmt.Errorf("nerdctl wait failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
	}
	exit, _ := strconv.Atoi(strings.TrimSpace(stdout))
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
		if labels["nerdctl/name"] == name {
			return dockerID(ns, c.ID()), nil
		}
	}
	return "", nil
}

// createDockerContainer creates a container via nerdctl and returns its Docker ID.
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
	if err := ensureImageInNamespace(ctx, req.Image, ns); err != nil {
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
		if existing, err := findContainerByName(ctx, ns, name); err == nil && existing != "" {
			return "", fmt.Errorf("Conflict. The container name \"/%s\" is already in use by container \"%s\". You have to remove (or rename) that container to be able to reuse that name.", name, existing)
		}
	}

	args := []string{"create"}
	if networkMode != "" && networkMode != "default" {
		args = append(args, "--net", networkMode)
	}
	// json-file logger makes `nerdctl logs -f` reliable enough for attach.
	args = append(args, "--log-driver", "json-file")
	// TTY changes the stream contract: with -t, attach output must be raw
	// bytes instead of the multiplexed docker stream format.
	if req.Tty {
		args = append(args, "-t")
	}
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
	// Restart policy is NOT passed to nerdctl: nerdctl/containerd arms its
	// own supervisor that races our monitor and re-restarts containers the
	// user stopped (on-failure sees exit 143 from stop). Our monitor in
	// restart.go owns the policy; inspect reports it from the registry.
	// Entrypoint override (docker run --entrypoint, compose entrypoint:).
	// nerdctl's --entrypoint is a single binary path, while Docker allows a
	// list ([bin, args...]). Translate: first element becomes --entrypoint,
	// the rest are prepended to the command argv.
	entrypointArgs := []string{}
	if len(req.Entrypoint) > 0 {
		args = append(args, "--entrypoint", req.Entrypoint[0])
		entrypointArgs = req.Entrypoint[1:]
	}
	if req.WorkingDir != "" {
		args = append(args, "-w", req.WorkingDir)
	}
	// /etc/hosts extras (docker run --add-host, compose extra_hosts:).
	for _, h := range req.HostConfig.ExtraHosts {
		args = append(args, "--add-host", h)
	}
	// Resource limits (compose mem_limit/cpus). nerdctl takes bytes and
	// fractional CPUs; Docker sends Memory in bytes and NanoCpus (1e9 = 1 CPU).
	if req.HostConfig.Memory > 0 {
		args = append(args, "--memory", fmt.Sprintf("%db", req.HostConfig.Memory))
	}
	if req.HostConfig.NanoCpus > 0 {
		cpus := float64(req.HostConfig.NanoCpus) / 1e9
		args = append(args, "--cpus", strconv.FormatFloat(cpus, 'f', -1, 64))
	}
	for _, c := range req.HostConfig.CapAdd {
		args = append(args, "--cap-add", c)
	}
	for _, c := range req.HostConfig.CapDrop {
		args = append(args, "--cap-drop", c)
	}
	if req.HostConfig.ReadonlyRootfs {
		args = append(args, "--read-only")
	}
	// PID namespace sharing (docker --pid host / compose pid: host).
	if mode := req.HostConfig.PidMode; mode != "" && mode != "private" {
		args = append(args, "--pid", mode)
	}
	if req.StopSignal != "" {
		args = append(args, "--stop-signal", req.StopSignal)
	}
	// tmpfs mounts with options (docker --tmpfs /x:ro,size=1m). nerdctl
	// takes the same path:opts syntax via --tmpfs.
	for path, opts := range req.HostConfig.TmpFs {
		spec := path
		if opts != "" {
			spec += ":" + opts
		}
		args = append(args, "--tmpfs", spec)
	}
	// Custom DNS servers (docker --dns / compose dns:).
	for _, d := range req.HostConfig.Dns {
		args = append(args, "--dns", d)
	}
	// Namespaced kernel parameters (docker --sysctl / compose sysctls:).
	for k, v := range req.HostConfig.Sysctls {
		args = append(args, "--sysctl", k+"="+v)
	}
	// Device passthrough (docker --device). nerdctl uses the same
	// host[:container] syntax.
	for _, d := range req.HostConfig.Devices {
		if d.PathOnHost == "" {
			continue
		}
		spec := d.PathOnHost
		if d.PathInContainer != "" && d.PathInContainer != d.PathOnHost {
			spec += ":" + d.PathInContainer
		}
		args = append(args, "--device", spec)
	}

	// Bind mounts and named volumes. Host paths under /Users are visible in
	// the guest at the same absolute path via the "macusers" virtiofs share,
	// so binds pass through unchanged (like Docker Desktop). Paths outside
	// the shared trees refer to the guest's own filesystem.
	for _, bind := range req.HostConfig.Binds {
		args = append(args, "-v", bind)
	}
	for _, m := range req.HostConfig.Mounts {
		// tmpfs-type Mounts (compose tmpfs:) become --tmpfs specs.
		if m.Type == "tmpfs" && m.Target != "" {
			spec := m.Target
			if m.ReadOnly {
				spec += ":ro"
			}
			args = append(args, "--tmpfs", spec)
			continue
		}
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
	// Ports are NOT passed to nerdctl: nerdctl reserves host ports at CREATE
	// time with an inherited listener fd, which breaks the Docker flow where
	// the check belongs to start (compose creates the replacement container
	// before stopping the old one). We persist the mappings into nerdctl's
	// network store instead — the same place nerdctl itself uses — so ps,
	// inspect and the port scanner see them, while the guest-side port proxy
	// + host PortForwarder own the actual publishing (see portproxy.go).
	var portMappings []cniPortMapping
	for cportSpec, hostPorts := range req.HostConfig.PortBindings {
		proto := "tcp"
		parts := strings.SplitN(cportSpec, "/", 2)
		cport := parts[0]
		if len(parts) == 2 {
			proto = parts[1]
		}
		for _, hp := range hostPorts {
			hostIP := hp.HostIp
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}
			// Docker allows ranges ("8100-8101:80-81"): expand pairwise.
			cPorts, err := expandPortRange(cport)
			if err != nil {
				return "", fmt.Errorf("invalid container port %q", cport)
			}
			hPorts, err := expandPortRange(hp.HostPort)
			if err != nil || (len(hPorts) > 1 && len(hPorts) != len(cPorts)) {
				return "", fmt.Errorf("invalid host port %q", hp.HostPort)
			}
			for i, c := range cPorts {
				h := hPorts[0]
				if len(hPorts) == len(cPorts) {
					h = hPorts[i]
				}
				if h == 0 {
					continue
				}
				portMappings = append(portMappings, cniPortMapping{
					HostPort:      h,
					ContainerPort: c,
					Protocol:      proto,
					HostIP:        hostIP,
				})
			}
		}
	}

	args = append(args, req.Image)
	args = append(args, entrypointArgs...)
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
	cl, err := pc.get(ctx)
	if err != nil {
		return "", fmt.Errorf("containerd client: %w", err)
	}

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
	// Persist the port mappings into nerdctl's network store so list/ps and
	// the port scanner (and nerdctl's own tooling) see the published ports.
	if len(portMappings) > 0 {
		if err := writeContainerPortMappings(ns, containerdID, portMappings); err != nil {
			log.Printf("[docker-api] persist ports for %s: %v", containerdID, err)
		}
	}
	// Remember compose service aliases for the start-time /etc/hosts update.
	if aliases := requestedNetworkAliases(req); len(aliases) > 0 {
		setNetworkAliases(dockerID, aliases)
	}
	// Remember docker --link specs for the start-time hosts update.
	if len(req.HostConfig.Links) > 0 {
		setContainerLinks(dockerID, req.HostConfig.Links)
	}
	// Register the restart policy with our monitor (nerdctl has none).
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

// nerdctlStoreRoot is the nerdctl data root (override in tests).
var nerdctlStoreRoot = "/var/lib/nerdctl"

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

// writeContainerPortMappings stores the published port mappings in nerdctl's
// network store (network-config.json) — the same file nerdctl itself writes
// for `-p` containers. We create containers without `-p` (nerdctl's
// create-time host-port reservation breaks compose-style recreate), so this
// is the authoritative record. The file is read-modify-written to preserve
// any fields nerdctl stores alongside portMappings.
func writeContainerPortMappings(ns, containerdID string, mappings []cniPortMapping) error {
	// The datastore directory is /var/lib/nerdctl/<addrHash>; locate it by
	// glob (only one exists in practice) or create it under the first
	// datastore root found.
	matches, _ := filepath.Glob(filepath.Join(nerdctlStoreRoot, "*", "containers"))
	var dir string
	if len(matches) > 0 {
		dir = filepath.Join(matches[0], ns, containerdID)
	} else {
		roots, _ := filepath.Glob(nerdctlStoreRoot + "/*")
		if len(roots) == 0 {
			return fmt.Errorf("nerdctl datastore not found")
		}
		dir = filepath.Join(roots[0], "containers", ns, containerdID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "network-config.json")

	raw := map[string]interface{}{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &raw)
	}
	encoded, err := json.Marshal(mappings)
	if err != nil {
		return err
	}
	// Round-trip through json.RawMessage to keep the exact cni shape.
	raw["portMappings"] = json.RawMessage(encoded)
	out, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// startDockerContainer starts a container by Docker ID or name.
// pruneStaleNetworkStore removes nerdctl network-store entries
// (/var/lib/nerdctl/<hash>/containers/<ns>/<id>) whose container no longer
// exists in containerd. The cold-boot metadata cleanup in stage2 removes
// containers bypassing `nerdctl rm`, so their store entries would otherwise
// accumulate forever on the persistent disk.
func pruneStaleNetworkStore() {
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
	stdout, stderr, code, err := runNerdctl(ns, "start", containerdID)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl start failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
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

func deleteDockerContainer(ctx context.Context, id string, force bool) error {
	ns, containerdID, _, err := resolveDockerID(ctx, id)
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
	restarts.clear(did)
	unmarkAutoRemove(did)
	takeContainerExitCode(did)
	return nil
}

// renameDockerContainer implements POST /containers/{id}/rename. Compose
// uses it in the recreate flow: the replacement is created under a temporary
// name and renamed once the old container is removed.
func renameDockerContainer(ctx context.Context, id string, newName string) error {
	ns, _, name, err := resolveDockerID(ctx, id)
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
func killDockerContainer(ctx context.Context, id string, signal string) error {
	ns, containerdID, _, err := resolveDockerID(ctx, id)
	if err != nil {
		return err
	}
	// A user signal wins over any restart policy (see stopDockerContainer).
	restarts.clear(dockerID(ns, containerdID))
	args := []string{"kill"}
	if signal != "" {
		args = append(args, "-s", signal)
	}
	args = append(args, containerdID)
	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl kill failed (%d): %s%s", code, stripANSI(stdout), stripANSI(stderr))
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

// restartDockerContainer restarts a container by Docker ID or name.
func restartDockerContainer(ctx context.Context, id string, timeout int) error {
	ns, containerdID, _, err := resolveDockerID(ctx, id)
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
			inspectDetails := inspectDetailsFor(ns, c.ID())
			exitCode := inspectDetails.State.ExitCode
			if exitCode == 0 && !running {
				if cached, ok := peekContainerExitCode(did); ok {
					exitCode = cached
				}
			}
			networkName := inspectDetails.HostConfig.NetworkMode
			if networkName == "" || networkName == "default" || networkName == "bridge" {
				networkName = "bridge"
			}
			endpoint := dockerEndpointStats{
				IPAddress:   inspectDetails.NetworkSettings.IPAddress,
				IPPrefixLen: inspectDetails.NetworkSettings.IPPrefixLen,
				MacAddress:  inspectDetails.NetworkSettings.MacAddress,
			}
			containerIP := endpoint.IPAddress
			if containerIP == "" {
				containerIP = detectGuestIP()
			}
			return &dockerContainerInspect{
				Id:    did,
				Name:  "/" + name,
				Image: imageName,
				State: dockerContainerState{
					Status:   status,
					Running:  running,
					Pid:      inspectDetails.State.Pid,
					ExitCode: exitCode,
					Health:   getHealthState(did),
				},
				RestartCount: restarts.countFor(did),
				Config: dockerContainerConfig{
					Labels:      labels,
					Image:       imageName,
					Healthcheck: getHealthcheckConfig(did),
					Tty:         getContainerTTY(did),
					OpenStdin:   inspectDetails.Config.OpenStdin,
					Env:         inspectDetails.Config.Env,
					Cmd:         inspectDetails.Config.Cmd,
					Entrypoint:  getContainerEntrypoint(did),
					WorkingDir:  getContainerWorkingDir(did),
					StopSignal:  getContainerStopSignal(did),
				},
				HostConfig: dockerHostConfig{
					AutoRemove:   isAutoRemove(did),
					NetworkMode:  inspectDetails.HostConfig.NetworkMode,
					PortBindings: portBindingsFromStore(ns, c.ID()),
					// nerdctl never sees --restart (it arms its own supervisor
					// that fights ours); report the policy from our registry.
					RestartPolicy: restarts.policySpecFor(did),
				},
				NetworkSettings: dockerNetworkSettings{
					IPAddress: containerIP,
					Ports:     portBindingsFromStore(ns, c.ID()),
					Networks:  map[string]dockerEndpointStats{networkName: endpoint},
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
	Config struct {
		Tty        bool     `json:"Tty"`
		OpenStdin  bool     `json:"OpenStdin"`
		Env        []string `json:"Env"`
		Cmd        []string `json:"Cmd"`
		Entrypoint []string `json:"Entrypoint"`
	} `json:"Config"`
	State struct {
		ExitCode int `json:"ExitCode"`
		Pid      int `json:"Pid"`
	} `json:"State"`
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
	info := inspectDetailsFor(ns, containerdID)
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

// inspectDetailsFor returns the nerdctl inspect payload for a container
// (network info plus Config fields like Tty/Env/Cmd). The unused `name`
// parameter keeps call sites stable; nerdctl resolves by container ID.
func inspectDetailsFor(ns, containerdID string) nerdctlInspectNetInfo {
	stdout, _, code, err := runNerdctl(ns, "inspect", "--format", "json", containerdID)
	if err != nil || code != 0 {
		return nerdctlInspectNetInfo{}
	}
	var info nerdctlInspectNetInfo
	_ = json.Unmarshal([]byte(stdout), &info)
	return info
}

// portBindingsFromStore renders the persisted port mappings back into the
// Docker HostConfig.PortBindings shape: "<containerPort>/<proto>" ->
// [{HostIp, HostPort}].
func portBindingsFromStore(ns, containerdID string) map[string][]dockerHostPort {
	bindings := map[string][]dockerHostPort{}
	portsJSON := readNetworkStorePorts(ns, containerdID)
	if portsJSON == "" {
		return bindings
	}
	var mappings []cniPortMapping
	if err := json.Unmarshal([]byte(mappingsJSON(portsJSON)), &mappings); err != nil {
		return bindings
	}
	for _, m := range mappings {
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

// mappingsJSON is a no-op cast keeping the call site readable.
func mappingsJSON(s string) []byte { return []byte(s) }
