package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
)

// anvilShareRoot is the virtiofs mount exposed by the host.
const anvilShareRoot = "/mnt/anvil"

// dockerNetwork matches the JSON returned by GET /networks.
type dockerNetwork struct {
	Id         string            `json:"Id"`
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Scope      string            `json:"Scope"`
	Created    string            `json:"Created"`
	Internal   bool              `json:"Internal"`
	Attachable bool              `json:"Attachable"`
	Ingress    bool              `json:"Ingress"`
	IPAM       dockerIPAM        `json:"IPAM"`
	Options    map[string]string `json:"Options"`
	Labels     map[string]string `json:"Labels"`
}

// dockerIPAM is the IPAM configuration inside dockerNetwork.
type dockerIPAM struct {
	Driver  string             `json:"Driver"`
	Config  []dockerIPAMConfig `json:"Config"`
	Options map[string]string  `json:"Options"`
}

// dockerIPAMConfig is a single IPAM pool config.
type dockerIPAMConfig struct {
	Subnet string `json:"Subnet,omitempty"`
}

// nerdctlNetworkLs is the shape of `nerdctl network ls --format json` output.
type nerdctlNetworkLs struct {
	ID     string `json:"ID"`
	Name   string `json:"Name"`
	Labels string `json:"Labels"`
}

// nerdctlNetworkInspect is the shape of `nerdctl network inspect --format json` output.
type nerdctlNetworkInspect struct {
	Name    string `json:"Name"`
	Id      string `json:"Id"`
	Driver  string `json:"Driver"`
	Scope   string `json:"Scope"`
	Created string `json:"Created"`
	IPAM    struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
	Labels  interface{}       `json:"Labels"`
	Options map[string]string `json:"Options"`
}

// dockerNetworkCreateRequest mirrors Docker's POST /networks/create body.
type dockerNetworkCreateRequest struct {
	Name      string            `json:"Name"`
	Driver    string            `json:"Driver"`
	Scope     string            `json:"Scope"`
	IPAM      dockerIPAM        `json:"IPAM"`
	Options   map[string]string `json:"Options"`
	Labels    map[string]string `json:"Labels"`
	Internal  bool              `json:"Internal"`
	Namespace string            `json:"-"`
}

// listDockerNetworks returns networks from all namespaces.
func listDockerNetworks(filters map[string]map[string]bool) ([]dockerNetwork, error) {
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
	// Always include the default namespace even if it has no containers/images,
	// because networks and volumes may live there.
	hasDefault := false
	for _, ns := range nss {
		if ns == "default" {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		nss = append([]string{"default"}, nss...)
	}

	seen := map[string]struct{}{}
	result := make([]dockerNetwork, 0)
	for _, ns := range nss {
		stdout, stderr, code, err := runNerdctl(ns, "network", "ls", "--format", "json")
		if err != nil || code != 0 {
			log.Printf("[docker-api] network ls in %s: %d %s%s", ns, code, stdout, stderr)
			continue
		}
		for _, line := range strings.Split(stdout, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var nw nerdctlNetworkLs
			if err := json.Unmarshal([]byte(line), &nw); err != nil {
				continue
			}
			if nw.ID == "" {
				continue
			}
			if _, ok := seen[nw.ID]; ok {
				continue
			}
			seen[nw.ID] = struct{}{}

			labels := mergeNetworkLabels(parseNerdctlLabels(nw.Labels), loadNetworkLabels(nw.Name))
			dn := dockerNetwork{
				Id:      nw.ID,
				Name:    nw.Name,
				Driver:  "bridge",
				Scope:   "local",
				Created: time.Now().UTC().Format(time.RFC3339),
				IPAM:    dockerIPAM{Driver: "default"},
				Options: map[string]string{},
				Labels:  labels,
			}
			if matchesLabelFilters(labels, filters) {
				result = append(result, dn)
			}
		}
	}
	return result, nil
}

// inspectDockerNetwork returns a network by name or ID.
func inspectDockerNetwork(name string) (*dockerNetwork, error) {
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
	hasDefault := false
	for _, ns := range nss {
		if ns == "default" {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		nss = append([]string{"default"}, nss...)
	}

	for _, ns := range nss {
		stdout, stderr, code, err := runNerdctl(ns, "network", "inspect", "--format", "json", name)
		if err != nil || code != 0 {
			log.Printf("[docker-api] network inspect in %s: %d %s%s", ns, code, stdout, stderr)
			continue
		}
		for _, line := range strings.Split(stdout, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var nw nerdctlNetworkInspect
			if err := json.Unmarshal([]byte(line), &nw); err != nil {
				continue
			}
			dn := nerdctlNetworkInspectToDocker(nw)
			if dn.Name == name || strings.HasPrefix(dn.Id, name) {
				// nerdctl inspect may not return Compose labels for bridge networks;
				// fall back to the CNI conflist we generated.
				dn.Labels = mergeNetworkLabels(dn.Labels, loadNetworkLabels(name))
				if len(dn.Labels) == 0 {
					dn.Labels = cniLabelsForNetwork(name)
				}
				return &dn, nil
			}
		}
	}
	return nil, fmt.Errorf("No such network: %s", name)
}

// networkLabelsPath returns the host-share path where we persist Compose labels
// for a network. nerdctl bridge network inspect does not reliably return the
// labels passed at create time, so we keep our own copy.
func networkLabelsPath(name string) string {
	base := sanitizeCNIName(name)
	return filepath.Join(anvilShareRoot, "networks", base+".json")
}

// saveNetworkLabels persists the labels for a network.
func saveNetworkLabels(name string, labels map[string]string) error {
	path := networkLabelsPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// loadNetworkLabels reads previously persisted labels for a network.
func loadNetworkLabels(name string) map[string]string {
	path := networkLabelsPath(name)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var labels map[string]string
	if err := json.Unmarshal(data, &labels); err != nil {
		return nil
	}
	return labels
}

// deleteNetworkLabels removes persisted labels for a network.
func deleteNetworkLabels(name string) {
	_ = os.Remove(networkLabelsPath(name))
}

// mergeNetworkLabels combines nerdctl/inspect labels with persisted Compose labels.
func mergeNetworkLabels(base, persisted map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range persisted {
		out[k] = v
	}
	return out
}

// restoreNetworkConfigs recreates CNI conflists from persisted labels after a
// cold boot. nerdctl keeps network state on the persistent containerd disk, but
// the CNI conflist lives on tmpfs and is lost on reboot. Without the conflist
// Docker Compose sees the network as "not created by compose" and refuses to
// use it.
func restoreNetworkConfigs() {
	dir := filepath.Join(anvilShareRoot, "networks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		labels := loadNetworkLabels(name)
		if labels == nil {
			labels = map[string]string{}
		}
		if err := generateCNIConfigWithLabels(name, labels); err != nil {
			log.Printf("[docker-api] restore network config for %s: %v", name, err)
		} else {
			log.Printf("[docker-api] restored network config for %s", name)
		}
	}
}

// cniLabelsForNetwork reads the labels from the CNI conflist generated for the
// given network name, if it exists.
func cniLabelsForNetwork(name string) map[string]string {
	base := sanitizeCNIName(name)
	path := filepath.Join(cniConfDir, "nerdctl-"+base+".conflist")
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	var conf struct {
		Labels map[string]string `json:"nerdctlLabels"`
	}
	if err := json.Unmarshal(data, &conf); err != nil {
		return map[string]string{}
	}
	if conf.Labels == nil {
		return map[string]string{}
	}
	return conf.Labels
}

// createDockerNetwork creates a network in the requested namespace.
// It first writes a deterministic CNI conflist for the requested name, because
// `nerdctl network create` cannot reuse a conflist it did not create itself.
func createDockerNetwork(req dockerNetworkCreateRequest) (*dockerNetwork, error) {
	ns := req.Namespace
	if ns == "" {
		// Compose custom networks are named <project>_<network>. Use the project
		// label as the containerd namespace so the network and its containers
		// live in the same namespace.
		if project := req.Labels["com.docker.compose.project"]; project != "" {
			ns = project
		} else {
			ns = namespaceFromNetwork(req.Name)
		}
	}
	if ns == "" {
		ns = "default"
	}
	log.Printf("[docker-api] create network %q in namespace %q labels=%v", req.Name, ns, req.Labels)

	// If the network already exists (e.g. pre-created by the scanner or a
	// previous compose run), return it. Do this before writing the CNI conflist,
	// otherwise nerdctl network inspect will report the conflist as an existing
	// network and we would skip the real creation and label persistence.
	if existing, err := inspectDockerNetwork(req.Name); err == nil && existing != nil {
		return existing, nil
	}

	// Pre-create the CNI config so the new network uses our deterministic subnet
	// and bridge name instead of an auto-generated one. Include any labels sent
	// by Compose so the network is recognised as Compose-managed. nerdctl treats
	// a conflist in /etc/cni/net.d as an existing network, so we do not need to
	// (and must not) call `nerdctl network create` afterwards.
	if err := generateCNIConfigWithLabels(req.Name, req.Labels); err != nil {
		log.Printf("[docker-api] ensure cni config for network %s: %v", req.Name, err)
	}

	// Build the response manually. nerdctl's bridge network inspect does not
	// always surface the labels we passed, but Compose relies on them being
	// present in inspect output.
	subnet := ""
	if len(req.IPAM.Config) > 0 && req.IPAM.Config[0].Subnet != "" {
		subnet = req.IPAM.Config[0].Subnet
	} else {
		octet := projectSubnetOctet(req.Name)
		subnet = fmt.Sprintf("10.10.%d.0/24", octet)
	}
	labels := req.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	if err := saveNetworkLabels(req.Name, labels); err != nil {
		log.Printf("[docker-api] save network labels for %s: %v", req.Name, err)
	}
	return &dockerNetwork{
		Id:      networkID(req.Name),
		Name:    req.Name,
		Driver:  defaultString(req.Driver, "bridge"),
		Scope:   "local",
		Created: time.Now().UTC().Format(time.RFC3339),
		IPAM:    dockerIPAM{Driver: defaultString(req.IPAM.Driver, "default"), Config: []dockerIPAMConfig{{Subnet: subnet}}},
		Labels:  labels,
		Options: req.Options,
	}, nil
}

// removeDockerNetwork removes a network by name or ID.
func removeDockerNetwork(name string) error {
	// Resolve the network first so we can delete the persisted labels by name
	// even when the client sent the network ID.
	var netName string
	if nw, err := inspectDockerNetwork(name); err == nil && nw != nil {
		netName = nw.Name
	}

	cl, err := client.New(containerdSocket)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	hasDefault := false
	for _, ns := range nss {
		if ns == "default" {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		nss = append([]string{"default"}, nss...)
	}

	for _, ns := range nss {
		stdout, stderr, code, err := runNerdctl(ns, "network", "rm", name)
		if err == nil && code == 0 {
			if netName != "" {
				deleteNetworkLabels(netName)
			}
			return nil
		}
		_ = stdout
		_ = stderr
	}
	return fmt.Errorf("No such network: %s", name)
}

// nerdctlNetworkInspectToDocker converts nerdctl network inspect JSON to Docker API shape.
func nerdctlNetworkInspectToDocker(nw nerdctlNetworkInspect) dockerNetwork {
	ipam := dockerIPAM{Driver: "default"}
	for _, cfg := range nw.IPAM.Config {
		ipam.Config = append(ipam.Config, dockerIPAMConfig{Subnet: cfg.Subnet})
	}
	created := nw.Created
	if created == "" {
		created = time.Now().UTC().Format(time.RFC3339)
	}
	labels := map[string]string{}
	switch m := nw.Labels.(type) {
	case map[string]interface{}:
		for k, v := range m {
			if s, ok := v.(string); ok {
				labels[k] = s
			}
		}
	case map[string]string:
		for k, v := range m {
			labels[k] = v
		}
	}
	options := nw.Options
	if options == nil {
		options = map[string]string{}
	}
	if ipam.Options == nil {
		ipam.Options = map[string]string{}
	}
	return dockerNetwork{
		Id:         nw.Id,
		Name:       nw.Name,
		Driver:     defaultString(nw.Driver, "bridge"),
		Scope:      defaultString(nw.Scope, "local"),
		Created:    created,
		Internal:   false,
		Attachable: false,
		Ingress:    false,
		IPAM:       ipam,
		Options:    options,
		Labels:     labels,
	}
}

// parseNerdctlLabels parses the comma-separated key=value label string returned
// by `nerdctl network ls --format json` and `nerdctl volume ls --format json`.
func parseNerdctlLabels(s string) map[string]string {
	labels := map[string]string{}
	s = strings.TrimSpace(s)
	if s == "" || s == "<no value>" {
		return labels
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			labels[kv[0]] = kv[1]
		}
	}
	return labels
}
