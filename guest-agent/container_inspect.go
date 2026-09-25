package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// GET /containers/json and GET /containers/{id}/json: list, filters and the
// inspect payload.

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
		// One task list per namespace instead of Task+Status per container.
		taskStates := namespaceTaskStates(nsCtx, cl)

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

			state := "created"
			status := "created"
			if st, ok := taskStates[c.ID()]; ok {
				state = dockerState(st)
				status = dockerStatus(st)
			}

			var ports []dockerPort
			if portsJSON := labels[labelPorts]; portsJSON != "" {
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

			did := dockerID(ns, c.ID())
			summary := dockerContainerSummary{
				Id:      did,
				Names:   []string{"/" + name},
				Image:   imageName,
				ImageID: info.Image,
				Command: command,
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

// namespaceTaskStates maps containerd container ID -> task status
// ("running", "stopped", "created", "paused") for one namespace.
func namespaceTaskStates(nsCtx context.Context, cl *client.Client) map[string]string {
	out := map[string]string{}
	resp, err := cl.TaskService().List(nsCtx, &tasks.ListTasksRequest{})
	if err != nil {
		return out
	}
	for _, p := range resp.Tasks {
		out[p.ID] = strings.ToLower(p.Status.String())
	}
	return out
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
				cmdList = userProcessArgs(spec.Process.Args)
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
				HostConfig: inspectHostConfig(meta, dockerHostConfig{
					AutoRemove:   isAutoRemove(did),
					NetworkMode:  networkName,
					PortBindings: portBindings,
					// The restart policy is owned by our monitor; report it
					// from the registry.
					RestartPolicy: restarts.policySpecFor(did),
				}),
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
