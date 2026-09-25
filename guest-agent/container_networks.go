package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// Containers on several networks. meta.Networks[0] is the primary network:
// its endpoint is eth0, carries the published ports and is what the rest of
// the agent treats as "the" container address (net.json's top-level
// fields). Every further network is a secondary endpoint (eth1, eth2...,
// net.json's Extra) attached at start, or live by docker network connect.
// Compose sends all of a service's networks in the create request's
// EndpointsConfig; before this, everything but the first was dropped.

// noneNetwork is --network none: an isolated netns with only lo up.
const noneNetwork = "none"

// validateNetworkMode refuses what Docker refuses for --network none.
func validateNetworkMode(req dockerCreateRequest) error {
	if req.HostConfig.NetworkMode != noneNetwork {
		return nil
	}
	if len(req.HostConfig.PortBindings) > 0 || req.HostConfig.PublishAllPorts {
		return fmt.Errorf("conflicting options: port publishing and the container type network mode")
	}
	return nil
}

// netEndpoint is one secondary network attachment of a running container.
type netEndpoint struct {
	Network string `json:"Network"`
	IfName  string `json:"IfName"`
	IP      string `json:"IP"`
	Mac     string `json:"Mac,omitempty"`
}

// ipOn returns the container's address on network ("" when not attached).
func (ni containerNetInfo) ipOn(network string) string {
	if ni.Network == network {
		return ni.IP
	}
	for _, e := range ni.Extra {
		if e.Network == network {
			return e.IP
		}
	}
	return ""
}

// endpointOn returns the endpoint stats on network.
func (ni containerNetInfo) endpointOn(network string) (dockerEndpointStats, bool) {
	if ni.Network == network {
		return dockerEndpointStats{IPAddress: ni.IP, MacAddress: ni.Mac}, true
	}
	for _, e := range ni.Extra {
		if e.Network == network {
			return dockerEndpointStats{IPAddress: e.IP, MacAddress: e.Mac}, true
		}
	}
	return dockerEndpointStats{}, false
}

// nextIfName is the lowest ethN (N >= 1) not used by a secondary endpoint.
func nextIfName(extra []netEndpoint) string {
	used := map[string]bool{}
	for _, e := range extra {
		used[e.IfName] = true
	}
	for n := 1; ; n++ {
		name := "eth" + strconv.Itoa(n)
		if !used[name] {
			return name
		}
	}
}

// secondaryNetworksFromCreate lists the create request's networks beyond
// the primary one, in a stable order.
func secondaryNetworksFromCreate(req dockerCreateRequest) []string {
	if req.NetworkingConfig == nil || usesHostNetwork(req) || req.HostConfig.NetworkMode == noneNetwork {
		return nil
	}
	primary := effectiveNetworkName(req.HostConfig.NetworkMode)
	var out []string
	for name := range req.NetworkingConfig.EndpointsConfig {
		n := effectiveNetworkName(name)
		if n == primary || usesHostNetworkName(n) || n == noneNetwork || slices.Contains(out, n) {
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// attachSecondaryNetworks attaches networks as eth1, eth2...; on failure it
// detaches what it attached and returns the error.
func attachSecondaryNetworks(ctx context.Context, id string, networks []string) ([]netEndpoint, error) {
	var eps []netEndpoint
	for _, n := range networks {
		ifName := nextIfName(eps)
		ip, mac, err := attachExtraNetwork(ctx, n, id, netnsPathFor(id), ifName)
		if err != nil {
			detachSecondaryNetworks(ctx, id, eps)
			return nil, err
		}
		eps = append(eps, netEndpoint{Network: n, IfName: ifName, IP: ip, Mac: mac})
	}
	return eps, nil
}

// detachSecondaryNetworks is best effort: teardown must not stop half-way.
func detachSecondaryNetworks(ctx context.Context, id string, eps []netEndpoint) {
	for _, e := range eps {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := detachExtraNetwork(dctx, e.Network, id, netnsPathFor(id), e.IfName); err != nil {
			debugLog("[cni] detach %s %s from %s: %v", truncateID(id), e.IfName, e.Network, err)
		}
		cancel()
	}
}

// --- docker network connect / disconnect ------------------------------------

type networkConnectRequest struct {
	Container      string `json:"Container"`
	Force          bool   `json:"Force"`
	EndpointConfig *struct {
		Aliases []string `json:"Aliases"`
	} `json:"EndpointConfig"`
}

func handleNetworkConnect(w http.ResponseWriter, r *http.Request, p routeParams) {
	var req networkConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Container == "" {
		writeJSONError(w, http.StatusBadRequest, "missing Container")
		return
	}
	var aliases []string
	if req.EndpointConfig != nil {
		aliases = req.EndpointConfig.Aliases
	}
	if err := connectContainerNetwork(r.Context(), p["id"], req.Container, aliases); err != nil {
		writeJSONError(w, errorStatus(err, http.StatusInternalServerError), err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func handleNetworkDisconnect(w http.ResponseWriter, r *http.Request, p routeParams) {
	var req networkConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Container == "" {
		writeJSONError(w, http.StatusBadRequest, "missing Container")
		return
	}
	if err := disconnectContainerNetwork(r.Context(), p["id"], req.Container); err != nil {
		writeJSONError(w, errorStatus(err, http.StatusInternalServerError), err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// resolveNetworkName maps a network name or ID prefix to its logical name.
func resolveNetworkName(ctx context.Context, ref string) (string, error) {
	nw, err := inspectDockerNetwork(ctx, ref)
	if err != nil {
		return "", &apiError{status: http.StatusNotFound, msg: err.Error()}
	}
	return nw.Name, nil
}

// connectContainerNetwork adds network to the container's networks and, when
// it runs, attaches the endpoint live.
func connectContainerNetwork(ctx context.Context, networkRef, container string, aliases []string) error {
	network, err := resolveNetworkName(ctx, networkRef)
	if err != nil {
		return err
	}
	ns, id, _, err := resolveDockerID(ctx, container)
	if err != nil {
		return &apiError{status: http.StatusNotFound, msg: err.Error()}
	}
	meta, err := loadContainerMeta(ns, id)
	if err != nil {
		return fmt.Errorf("container metadata: %w", err)
	}
	if len(meta.Networks) > 0 && (usesHostNetworkName(meta.Networks[0]) || meta.Networks[0] == noneNetwork) {
		return &apiError{status: http.StatusBadRequest,
			msg: fmt.Sprintf("container sharing network namespace with another container or host cannot be connected to any other network (network mode %q)", meta.Networks[0])}
	}
	if slices.Contains(meta.Networks, network) {
		return &apiError{status: http.StatusForbidden,
			msg: fmt.Sprintf("endpoint with name %s already exists in network %s", strings.TrimPrefix(meta.Name, "/"), network)}
	}

	if running, _, _ := containerTaskState(ctx, ns, id); running {
		ni, _ := loadNetInfo(ns, id)
		ifName := nextIfName(ni.Extra)
		ip, mac, err := attachExtraNetwork(ctx, network, id, netnsPathFor(id), ifName)
		if err != nil {
			return err
		}
		ni.Extra = append(ni.Extra, netEndpoint{Network: network, IfName: ifName, IP: ip, Mac: mac})
		if err := saveNetInfo(ns, id, ni); err != nil {
			detachSecondaryNetworks(ctx, id, ni.Extra[len(ni.Extra)-1:])
			return err
		}
	}
	meta.Networks = append(meta.Networks, network)
	if meta.NetworkAliases == nil {
		// Freeze the legacy flat list onto the networks it applied to, so
		// the new network's aliases stay on the new network only.
		meta.NetworkAliases = map[string][]string{}
		for _, n := range meta.Networks[:len(meta.Networks)-1] {
			meta.NetworkAliases[n] = meta.Aliases
		}
	}
	meta.NetworkAliases[network] = dedupeStrings(aliases)
	if err := saveContainerMeta(meta); err != nil {
		return err
	}
	updateNetworksLabel(ctx, ns, id, meta.Networks)
	refreshHostsForContainer(ns, id)
	log.Printf("[docker-api] connected %s to %s", truncateID(id), network)
	return nil
}

// disconnectContainerNetwork removes network from the container. A running
// container's primary endpoint (eth0, which carries the published ports)
// cannot be moved live; secondary ones detach immediately.
func disconnectContainerNetwork(ctx context.Context, networkRef, container string) error {
	network, err := resolveNetworkName(ctx, networkRef)
	if err != nil {
		return err
	}
	ns, id, _, err := resolveDockerID(ctx, container)
	if err != nil {
		return &apiError{status: http.StatusNotFound, msg: err.Error()}
	}
	meta, err := loadContainerMeta(ns, id)
	if err != nil {
		return fmt.Errorf("container metadata: %w", err)
	}
	idx := slices.Index(meta.Networks, network)
	if idx < 0 {
		return &apiError{status: http.StatusBadRequest,
			msg: fmt.Sprintf("container %s is not connected to network %s", truncateID(dockerID(ns, id)), network)}
	}
	if len(meta.Networks) == 1 {
		return &apiError{status: http.StatusBadRequest,
			msg: fmt.Sprintf("cannot disconnect container from its last network %s", network)}
	}
	running, _, _ := containerTaskState(ctx, ns, id)
	if running {
		if idx == 0 {
			return &apiError{status: http.StatusBadRequest,
				msg: fmt.Sprintf("cannot disconnect a running container from its primary network %s; stop it first", network)}
		}
		ni, _ := loadNetInfo(ns, id)
		var gone []netEndpoint
		ni.Extra = slices.DeleteFunc(ni.Extra, func(e netEndpoint) bool {
			if e.Network == network {
				gone = append(gone, e)
				return true
			}
			return false
		})
		detachSecondaryNetworks(ctx, id, gone)
		if err := saveNetInfo(ns, id, ni); err != nil {
			return err
		}
	}
	meta.Networks = slices.Delete(meta.Networks, idx, idx+1)
	delete(meta.NetworkAliases, network)
	if err := saveContainerMeta(meta); err != nil {
		return err
	}
	updateNetworksLabel(ctx, ns, id, meta.Networks)
	refreshNetworkHosts(network)
	refreshHostsForContainer(ns, id)
	log.Printf("[docker-api] disconnected %s from %s", truncateID(id), network)
	return nil
}

// updateNetworksLabel keeps the containerd label in step with the metadata.
func updateNetworksLabel(ctx context.Context, ns, id string, networks []string) {
	cl, err := pc.get(ctx)
	if err != nil {
		return
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return
	}
	data, _ := json.Marshal(networks)
	if _, err := c.SetLabels(nsCtx, map[string]string{labelNetworks: string(data)}); err != nil {
		debugLog("[docker-api] networks label %s: %v", truncateID(id), err)
	}
}

// --- network inspect endpoints ------------------------------------------------

// networkEndpoints lists the running containers with an endpoint on network,
// keyed by Docker container ID.
func networkEndpoints(network string, prefixLen int) map[string]dockerNetworkContainer {
	metas, err := containerMetas()
	if err != nil {
		return map[string]dockerNetworkContainer{}
	}
	return endpointsOn(network, prefixLen, metas, func(ns, id string) (containerNetInfo, bool) { return loadNetInfo(ns, id) })
}

func endpointsOn(network string, prefixLen int, metas []*containerMeta, netInfo func(ns, id string) (containerNetInfo, bool)) map[string]dockerNetworkContainer {
	out := map[string]dockerNetworkContainer{}
	for _, m := range metas {
		if !slices.Contains(m.Networks, network) {
			continue
		}
		ni, ok := netInfo(m.Namespace, m.ID)
		if !ok {
			continue
		}
		ep, ok := ni.endpointOn(network)
		if !ok || ep.IPAddress == "" {
			continue
		}
		did := dockerID(m.Namespace, m.ID)
		addr := ep.IPAddress
		if prefixLen > 0 {
			addr += "/" + strconv.Itoa(prefixLen)
		}
		out[did] = dockerNetworkContainer{
			Name:        strings.TrimPrefix(m.Name, "/"),
			EndpointID:  dockerID(did, network),
			MacAddress:  ep.MacAddress,
			IPv4Address: addr,
		}
	}
	return out
}

// networkPrefixLen is the prefix length of the network's first subnet.
func networkPrefixLen(ipam dockerIPAM) int {
	for _, c := range ipam.Config {
		if _, n, err := net.ParseCIDR(c.Subnet); err == nil {
			ones, _ := n.Mask.Size()
			return ones
		}
	}
	return 0
}

func endpointNames(eps map[string]dockerNetworkContainer) string {
	var names []string
	for _, e := range eps {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
