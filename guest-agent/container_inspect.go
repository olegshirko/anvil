package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// GET /containers/json and GET /containers/{id}/json: list, filters and the
// inspect payload.

// listDockerContainers scans all containerd namespaces and returns a
// Docker-compatible summary for each container.
// containerFilterKeys are the ps filters Docker accepts; any other key is a
// 400, as with Docker (silently ignoring it listed everything — and
// `docker rm $(docker ps -aq -f ancestor=x)` removed everything).
var containerFilterKeys = map[string]bool{
	"ancestor": true, "before": true, "expose": true, "exited": true, "health": true,
	"id": true, "isolation": true, "is-task": true, "label": true, "label!": true,
	"name": true, "network": true, "publish": true, "since": true, "status": true,
	"volume": true,
}

// validateFilterKeys returns Docker's error for an unknown filter key.
func validateFilterKeys(filters map[string]map[string]bool, allowed map[string]bool) error {
	for k := range filters {
		if !allowed[k] {
			return &apiError{status: http.StatusBadRequest, msg: fmt.Sprintf("invalid filter '%s'", k)}
		}
	}
	return nil
}

// anyMatch reports whether pred holds for one of the filter's values (values
// of one key are OR-ed); true when the key is absent.
func anyMatch(values map[string]bool, pred func(v string) bool) bool {
	if len(values) == 0 {
		return true
	}
	for v := range values {
		if pred(v) {
			return true
		}
	}
	return false
}

// matchesContainerFilters applies the ps filters: keys are AND-ed, values
// within a key are OR-ed. before/since are applied by the caller (they need
// the referenced container).
func matchesContainerFilters(s dockerContainerSummary, filters map[string]map[string]bool) bool {
	if !matchesLabelFilters(s.Labels, filters) {
		return false
	}
	name := strings.TrimPrefix(s.Names[0], "/")
	checks := []bool{
		anyMatch(filters["name"], func(v string) bool { return v != "" && (nameFilterMatch(v, name) || nameFilterMatch(v, "/"+name)) }),
		anyMatch(filters["id"], func(v string) bool { return strings.HasPrefix(s.Id, v) }),
		anyMatch(filters["status"], func(v string) bool { return v == s.State }),
		anyMatch(filters["ancestor"], func(v string) bool { return imageMatchesAncestor(s.Image, s.ImageID, v) }),
		anyMatch(filters["network"], func(v string) bool {
			for _, n := range s.networks {
				if n == v || (s.networkIDs[n] != "" && strings.HasPrefix(s.networkIDs[n], v)) {
					return true
				}
			}
			return false
		}),
		anyMatch(filters["health"], func(v string) bool {
			h := s.health
			if h == "" {
				h = "none"
			}
			return v == h
		}),
		anyMatch(filters["exited"], func(v string) bool {
			code, err := strconv.Atoi(v)
			return err == nil && s.State == "exited" && s.exitCode == code
		}),
		anyMatch(filters["volume"], func(v string) bool {
			for _, m := range s.Mounts {
				if m.Name == v || m.Destination == v || m.Source == v {
					return true
				}
			}
			return false
		}),
		anyMatch(filters["publish"], func(v string) bool { return portFilterMatches(s.Ports, v, true) }),
		anyMatch(filters["expose"], func(v string) bool {
			for _, e := range s.exposed {
				if e == v || strings.TrimSuffix(e, "/tcp") == v {
					return true
				}
			}
			return portFilterMatches(s.Ports, v, false)
		}),
		anyMatch(filters["is-task"], func(v string) bool { return v == "false" }),
	}
	for _, ok := range checks {
		if !ok {
			return false
		}
	}
	return true
}

// imageMatchesAncestor matches ancestor=<image>: the reference with or
// without a tag (":latest" implied), or an image ID prefix.
func imageMatchesAncestor(image, imageID, want string) bool {
	if want == "" {
		return false
	}
	if strings.HasPrefix(imageID, want) || strings.HasPrefix(strings.TrimPrefix(imageID, "sha256:"), want) {
		return true
	}
	ci, cw := canonicalizeImageRef(image), canonicalizeImageRef(want)
	return ci == cw || image == want
}

// portFilterMatches matches publish/expose values: "80", "80/tcp",
// "8000-8080/tcp". publish compares the host port, expose the container port.
func portFilterMatches(ports []dockerPort, v string, public bool) bool {
	spec, proto, _ := strings.Cut(v, "/")
	lo, hi := 0, 0
	if a, b, isRange := strings.Cut(spec, "-"); isRange {
		lo, _ = strconv.Atoi(a)
		hi, _ = strconv.Atoi(b)
	} else {
		lo, _ = strconv.Atoi(spec)
		hi = lo
	}
	for _, p := range ports {
		port := p.PrivatePort
		if public {
			port = p.PublicPort
		}
		if port >= lo && port <= hi && (proto == "" || proto == p.Type) {
			return true
		}
	}
	return false
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
	netIDs := networkIDsByName()
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			log.Printf("[docker-api] list containers in %s: %v", ns, err)
			continue
		}
		// One task list per namespace instead of Task+Status per container.
		taskStates, tasksKnown := namespaceTaskStates(nsCtx, cl)
		// Image name -> ID (its target digest), looked up once per name.
		imageIDs := map[string]string{}
		imageIDOf := func(name string) string {
			if id, ok := imageIDs[name]; ok {
				return id
			}
			id := ""
			if img, err := cl.GetImage(nsCtx, name); err == nil {
				id = img.Target().Digest.String()
			}
			imageIDs[name] = id
			return id
		}

		for _, c := range containers {
			// The list call already returned the record: no per-container
			// re-fetch for info, labels, image or spec.
			info, err := c.Info(nsCtx, client.WithoutRefreshedMetadata)
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

			did := dockerID(ns, c.ID())
			meta, _ := loadContainerMeta(ns, c.ID())
			state := "created"
			exitCode := 0
			if st, ok := taskStates[c.ID()]; ok {
				state = dockerState(st.status)
				exitCode = st.exitCode
				if cached, ok := peekContainerExitCode(did); ok && state == "exited" && exitCode == 0 {
					exitCode = cached
				}
			}
			var startedAt, finishedAt time.Time
			if meta != nil {
				startedAt, finishedAt = meta.StartedAt, meta.FinishedAt
				if _, hasTask := taskStates[c.ID()]; tasksKnown && !hasTask && !startedAt.IsZero() {
					// Ran before a cold boot took its task: exited.
					state, exitCode = "exited", meta.ExitCode
				}
			}
			status := dockerStatusText(state, exitCode, startedAt, finishedAt, time.Now())

			var ports []dockerPort
			if portsJSON := labels[labelPorts]; portsJSON != "" {
				var pm []cniPortMapping
				if err := json.Unmarshal([]byte(portsJSON), &pm); err == nil {
					for _, p := range pm {
						if p.HostPort <= 0 {
							continue // ephemeral binding not assigned yet
						}
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

			// c.Image() only looked the image up by this same name.
			imageName := info.Image

			// docker ps COMMAND: the OCI process args (image entrypoint/cmd
			// merged at create time), quoted like the docker CLI does.
			command := ""
			var spec specs.Spec
			if info.Spec != nil && json.Unmarshal(info.Spec.GetValue(), &spec) == nil && spec.Process != nil {
				userArgs := userProcessArgs(spec.Process.Args)
				args := make([]string, len(userArgs))
				for i, a := range userArgs {
					if strings.ContainsAny(a, " \t") {
						args[i] = `"` + a + `"`
					} else {
						args[i] = a
					}
				}
				command = strings.Join(args, " ")
			}

			summary := dockerContainerSummary{
				Id:      did,
				Names:   []string{"/" + name},
				Image:   imageName,
				ImageID: imageIDOf(info.Image),
				Command: command,
				Created: info.CreatedAt.Unix(),
				Ports:   ports,
				Labels:  labels,
				State:   state,
				Status:  formatHealthStatus(did, status),
			}
			summary.exitCode = exitCode
			summary.networkIDs = netIDs
			summary.created = info.CreatedAt.UnixNano()
			if hs := getHealthState(did); hs != nil {
				summary.health = hs.Status
			}
			if meta != nil {
				summary.networks = meta.Networks
				summary.exposed = meta.ExposedPorts
			}
			summary.NetworkSettings.Networks = map[string]dockerEndpointStats{}
			if meta != nil && len(meta.Networks) > 0 {
				summary.HostConfig.NetworkMode = meta.Networks[0]
				if !isContainerNetworkMode(meta.Networks[0]) {
					ni, _ := loadNetInfo(ns, c.ID())
					for _, n := range meta.Networks {
						ep, _ := ni.endpointOn(n)
						ep.NetworkID = netIDs[n]
						summary.NetworkSettings.Networks[n] = ep
					}
				}
			}
			if meta != nil {
				summary.Mounts = inspectMountPoints(meta)
			} else {
				summary.Mounts = []dockerMountPoint{}
			}
			if matchesContainerFilters(summary, filters) {
				result = append(result, summary)
			}
		}
	}

	// Newest first, as docker ps lists them (ties broken by ID).
	sort.Slice(result, func(i, j int) bool {
		if result[i].created != result[j].created {
			return result[i].created > result[j].created
		}
		return result[i].Id < result[j].Id
	})
	return result, nil
}

// taskSnapshot is one task's state from a namespace-wide task listing.
type taskSnapshot struct {
	status   string // "running", "stopped", "created", "paused"
	exitCode int
}

// namespaceTaskStates maps containerd container ID -> task state for one
// namespace.
func namespaceTaskStates(nsCtx context.Context, cl *client.Client) (map[string]taskSnapshot, bool) {
	out := map[string]taskSnapshot{}
	resp, err := cl.TaskService().List(nsCtx, &tasks.ListTasksRequest{})
	if err != nil {
		return out, false
	}
	for _, p := range resp.Tasks {
		out[p.ID] = taskSnapshot{status: strings.ToLower(p.Status.String()), exitCode: int(p.ExitStatus)}
	}
	return out, true
}

// inspectDockerContainer returns a minimal inspect payload for a container.
// The lookup accepts a Docker ID prefix, a container name, or a leading-slash
// name as returned by `docker ps`.
func inspectDockerContainer(ctx context.Context, prefix string) (*dockerContainerInspect, error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}

	// Same resolution order as every other endpoint; the loop below then
	// only builds the result for that one container.
	rns, rid, _, err := resolveDockerID(ctx, prefix)
	if err != nil {
		return nil, err
	}

	for _, ns := range []string{rns} {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			continue
		}
		for _, c := range containers {
			if c.ID() != rid {
				continue
			}
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

			status := "created"
			running := false
			exitCode := 0
			pid := 0
			task, err := c.Task(nsCtx, nil)
			// Only "not found" means no task: a transient containerd error
			// must not show a running container as exited.
			hasTask := err == nil || !errdefs.IsNotFound(err)
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
			imageID := ""
			if img, err := c.Image(nsCtx); err == nil && img != nil {
				imageName = img.Name()
				imageID = img.Target().Digest.String()
			}
			if imageID == "" {
				imageID = imageName
			}

			did := dockerID(ns, c.ID())
			if exitCode == 0 && !running {
				if cached, ok := peekContainerExitCode(did); ok {
					exitCode = cached
				}
			}
			meta, _ := loadContainerMeta(ns, c.ID())
			if !hasTask && meta != nil && !meta.StartedAt.IsZero() {
				// Ran before a cold boot took its task: exited.
				status, exitCode = "exited", meta.ExitCode
			}

			// Process config comes from the OCI spec; env/cmd reflect what
			// will actually run (image config merged with overrides).
			envList, cmdList, openStdin := []string{}, []string{}, false
			hostname := ""
			if spec, err := c.Spec(nsCtx); err == nil && spec != nil && spec.Process != nil {
				envList = spec.Process.Env
				cmdList = userProcessArgs(spec.Process.Args)
				hostname = spec.Hostname
			}
			if meta != nil {
				openStdin = meta.OpenStdin
			}
			path, args := "", []string{}
			if len(cmdList) > 0 {
				path, args = cmdList[0], cmdList[1:]
			}
			cfg := dockerContainerConfig{Hostname: hostname}
			startedAt, finishedAt := dockerTime(time.Time{}), dockerTime(time.Time{})
			if meta != nil {
				cfg.User = meta.ConfigUser
				cfg.Domainname = meta.Domainname
				cfg.StdinOnce = meta.StdinOnce
				cfg.AttachStdin = meta.OpenStdin
				cfg.AttachStdout, cfg.AttachStderr = true, true
				if len(meta.ExposedPorts) > 0 {
					cfg.ExposedPorts = map[string]struct{}{}
					for _, p := range meta.ExposedPorts {
						cfg.ExposedPorts[p] = struct{}{}
					}
				}
				startedAt, finishedAt = dockerTime(meta.StartedAt), dockerTime(meta.FinishedAt)
			}
			networkName := "bridge"
			if meta != nil && len(meta.Networks) > 0 {
				networkName = meta.Networks[0]
			}
			endpoint := dockerEndpointStats{}
			ni, niOK := loadNetInfo(ns, c.ID())
			if niOK {
				endpoint = dockerEndpointStats{IPAddress: ni.IP, MacAddress: ni.Mac}
			} else if usesHostNetworkName(networkName) && !isContainerNetworkMode(networkName) {
				endpoint = dockerEndpointStats{IPAddress: detectGuestIP()}
			}
			endpoints := map[string]dockerEndpointStats{networkName: endpoint}
			if isContainerNetworkMode(networkName) {
				endpoints = map[string]dockerEndpointStats{} // as Docker: the target owns them
			}
			if meta != nil && len(meta.Networks) > 1 {
				for _, n := range meta.Networks[1:] {
					endpoints[n], _ = ni.endpointOn(n)
				}
			}
			enrichEndpoints(endpoints, meta, name, did)
			containerIP := endpoint.IPAddress
			if containerIP == "" && networkName != noneNetwork {
				containerIP = detectGuestIP()
			}
			var portBindings map[string][]dockerHostPort
			if meta != nil {
				portBindings = portBindingsFromMeta(meta)
			}
			return &dockerContainerInspect{
				Id:             did,
				Created:        dockerTime(info.CreatedAt),
				Path:           path,
				Args:           args,
				Name:           "/" + name,
				Image:          imageID,
				ResolvConfPath: containerResolvPath(ns, c.ID()),
				HostnamePath:   filepath.Join(containerMetaDir(ns, c.ID()), "hostname"),
				HostsPath:      containerHostsPath(ns, c.ID()),
				LogPath:        containerLogPath(ns, c.ID()),
				Driver:         "overlayfs",
				Platform:       "linux",
				State: dockerContainerState{
					Status:     status,
					Running:    running,
					Paused:     status == "paused",
					Pid:        pid,
					ExitCode:   exitCode,
					StartedAt:  startedAt,
					FinishedAt: finishedAt,
					Health:     getHealthState(did),
				},
				RestartCount: restarts.countFor(did),
				Mounts:       inspectMountPoints(meta),
				Config: func() dockerContainerConfig {
					cfg.Labels = labels
					cfg.Image = imageName
					cfg.Healthcheck = getHealthcheckConfig(did)
					cfg.Tty = getContainerTTY(did)
					cfg.OpenStdin = openStdin
					cfg.Env = envList
					cfg.Cmd = cmdList
					cfg.Entrypoint = getContainerEntrypoint(did)
					cfg.WorkingDir = getContainerWorkingDir(did)
					cfg.StopSignal = getContainerStopSignal(did)
					return cfg
				}(),
				HostConfig: inspectHostConfig(meta, dockerHostConfig{
					AutoRemove:   isAutoRemove(did),
					NetworkMode:  networkName,
					PortBindings: requestedPortBindings(meta),
					// The restart policy is owned by our monitor; report it
					// from the registry.
					RestartPolicy: restarts.policySpecFor(did),
				}),
				NetworkSettings: dockerNetworkSettings{
					IPAddress: containerIP,
					Ports:     portBindings,
					Networks:  endpoints,
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
	var secondary []string
	if meta, err := loadContainerMeta(ns, containerdID); err == nil && len(meta.Networks) > 0 {
		networkName = meta.Networks[0]
		secondary = meta.Networks[1:]
	}
	ni, niOK := loadNetInfo(ns, containerdID)
	if niOK {
		primary = dockerEndpointStats{IPAddress: ni.IP, MacAddress: ni.Mac}
	} else if usesHostNetworkName(networkName) && !isContainerNetworkMode(networkName) {
		primary.IPAddress = detectGuestIP()
	}
	networks := map[string]dockerEndpointStats{networkName: primary}
	for _, n := range secondary {
		networks[n], _ = ni.endpointOn(n)
	}
	return networks, primary.IPAddress
}

// portBindingsFromMeta renders the persisted port mappings back into the
// Docker NetworkSettings.Ports shape: "<containerPort>/<proto>" ->
// [{HostIp, HostPort}], with the host port actually assigned. An ephemeral
// binding not assigned yet (never started) shows as an exposed port with no
// binding, as Docker reports it.
func portBindingsFromMeta(meta *containerMeta) map[string][]dockerHostPort {
	return renderPortBindings(meta, false)
}

// requestedPortBindings is the HostConfig.PortBindings form: what was asked
// for at create, so `-p 80` stays HostPort "" however often the container
// starts.
func requestedPortBindings(meta *containerMeta) map[string][]dockerHostPort {
	return renderPortBindings(meta, true)
}

func renderPortBindings(meta *containerMeta, requested bool) map[string][]dockerHostPort {
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
		hostPort := strconv.Itoa(m.HostPort)
		switch {
		case requested && m.Ephemeral && m.RangeLo > 0:
			hostPort = fmt.Sprintf("%d-%d", m.RangeLo, m.RangeHi)
		case requested && m.Ephemeral:
			hostPort = ""
		case m.HostPort <= 0:
			if _, ok := bindings[key]; !ok {
				bindings[key] = nil
			}
			continue
		}
		bindings[key] = append(bindings[key], dockerHostPort{
			HostIp:   hostIP,
			HostPort: hostPort,
		})
	}
	return bindings
}

// unsupportedHostConfigWarnings lists HostConfig features the native runtime
// knowingly ignores, so `docker create` surfaces them in Warnings instead of
// failing silently. Fields that can never be honored are rejected with a 400
// by validateHostConfig instead — this is only for accepted-but-degraded.
func unsupportedHostConfigWarnings(hc dockerHostConfig) []string {
	var w []string
	if m := hc.UsernsMode; m != "" && m != "host" {
		w = append(w, fmt.Sprintf("HostConfig.UsernsMode %q is not supported", m))
	}
	return w
}

// inspectHostConfig layers the dynamic, monitor-owned HostConfig fields over
// the create-time snapshot persisted in the container metadata, so inspect
// reports what was requested (memory, cpus, caps, ...) alongside live state.
func inspectHostConfig(meta *containerMeta, live dockerHostConfig) dockerHostConfig {
	if meta == nil || meta.HostConfig == nil {
		return live
	}
	snap := *meta.HostConfig
	// Fields owned elsewhere at runtime override the snapshot.
	snap.AutoRemove = live.AutoRemove
	snap.NetworkMode = live.NetworkMode
	snap.PortBindings = live.PortBindings
	snap.RestartPolicy = live.RestartPolicy
	return snap
}

// inspectMountPoints is .Mounts (an empty list, never null, like Docker).
func inspectMountPoints(meta *containerMeta) []dockerMountPoint {
	if meta == nil || meta.MountPoints == nil {
		return []dockerMountPoint{}
	}
	return meta.MountPoints
}

// dockerTime formats a timestamp as Docker does; the zero time is
// "0001-01-01T00:00:00Z" (never started / not finished).
func dockerTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// enrichEndpoints fills inspect's per-network fields that clients read:
// NetworkID, Gateway, IPPrefixLen (testcontainers and compose compute
// addresses from them), Aliases and DNSNames.
func enrichEndpoints(endpoints map[string]dockerEndpointStats, meta *containerMeta, name, did string) {
	conflists, err := loadCNIConflists()
	if err != nil {
		return
	}
	for netName, ep := range endpoints {
		for _, cl := range conflists {
			if cl.Name != netName {
				continue
			}
			dn := conflistToDockerNetwork(cl)
			ep.NetworkID = dn.Id
			if len(dn.IPAM.Config) > 0 {
				ep.Gateway = dn.IPAM.Config[0].Gateway
			}
			if ep.IPAddress != "" {
				ep.IPPrefixLen = networkPrefixLen(dn.IPAM)
			}
			break
		}
		if meta != nil {
			ep.Aliases = meta.aliasesOn(netName)
		}
		if netName != "bridge" && netName != noneNetwork && !usesHostNetworkName(netName) {
			ep.DNSNames = append([]string{name}, ep.Aliases...)
			ep.DNSNames = append(ep.DNSNames, truncateID(did))
		}
		endpoints[netName] = ep
	}
}

// networkIDsByName maps network names to their IDs, read from the conflists
// (the ID stored there is what network ls/inspect report).
func networkIDsByName() map[string]string {
	out := map[string]string{}
	conflists, err := loadCNIConflists()
	if err != nil {
		return out
	}
	for _, cl := range conflists {
		dn := conflistToDockerNetwork(cl)
		out[dn.Name] = dn.Id
	}
	return out
}
