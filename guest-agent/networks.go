package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// anvilShareRoot is the virtiofs mount exposed by the host.
const anvilShareRoot = "/mnt/anvil"

// anvilRunDir is where runtime artifacts (logs, persisted network labels) go
// on the share. In dev mode the share is the project root, so writing at the
// share root litters the checkout — everything ephemeral lives under this
// dot-directory instead.
const anvilRunDir = anvilShareRoot + "/.anvil-run"

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

// cniConflist is the subset of our generated CNI conflist files that network
// listing/inspecting needs. The conflists in /etc/cni/net.d are the single
// source of truth for networks.
type cniConflist struct {
	Name    string            `json:"name"`
	AnvilID string            `json:"anvilID"`
	Labels  map[string]string `json:"anvilLabels"`
	Plugins []struct {
		Type   string `json:"type"`
		Bridge string `json:"bridge"`
		IPAM   struct {
			Ranges [][]struct {
				Subnet  string `json:"subnet"`
				Gateway string `json:"gateway"`
			} `json:"ranges"`
		} `json:"ipam"`
	} `json:"plugins"`
}

// cniConflistPath returns the canonical file name of a network's conflist.
func cniConflistPath(name string) string {
	base := sanitizeCNIName(name)
	return filepath.Join(cniConfDir, "anvil-"+base+".conflist")
}

// loadCNIConflists parses every anvil conflist in the CNI config directory,
// keyed by logical network name.
func loadCNIConflists() (map[string]cniConflist, error) {
	entries, err := os.ReadDir(cniConfDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]cniConflist{}, nil
		}
		return nil, err
	}
	out := map[string]cniConflist{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".conflist") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(cniConfDir, e.Name()))
		if err != nil {
			continue
		}
		var cl cniConflist
		if json.Unmarshal(data, &cl) != nil || cl.Name == "" {
			continue
		}
		out[cl.Name] = cl
	}
	return out, nil
}

// conflistToDockerNetwork renders a conflist into the Docker API shape.
func conflistToDockerNetwork(cl cniConflist) dockerNetwork {
	ipam := dockerIPAM{Driver: "default"}
	for _, p := range cl.Plugins {
		for _, ranges := range p.IPAM.Ranges {
			for _, r := range ranges {
				ipam.Config = append(ipam.Config, dockerIPAMConfig{Subnet: r.Subnet})
			}
		}
	}
	labels := cl.Labels
	if labels == nil {
		labels = mergeNetworkLabels(map[string]string{}, loadNetworkLabels(cl.Name))
	} else {
		labels = mergeNetworkLabels(labels, loadNetworkLabels(cl.Name))
	}
	return dockerNetwork{
		Id:      cl.AnvilID,
		Name:    cl.Name,
		Driver:  "bridge",
		Scope:   "local",
		Created: time.Now().UTC().Format(time.RFC3339),
		IPAM:    ipam,
		Options: map[string]string{},
		Labels:  labels,
	}
}

// listDockerNetworks returns all networks known to the guest (one conflist per
// network; the default namespace bridge included).
func listDockerNetworks(ctx context.Context, filters map[string]map[string]bool) ([]dockerNetwork, error) {
	conflists, err := loadCNIConflists()
	if err != nil {
		return nil, fmt.Errorf("list cni conflists: %w", err)
	}
	result := make([]dockerNetwork, 0, len(conflists))
	seen := map[string]struct{}{}
	names := make([]string, 0, len(conflists))
	for name := range conflists {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dn := conflistToDockerNetwork(conflists[name])
		if _, ok := seen[dn.Id]; ok && dn.Id != "" {
			continue
		}
		seen[dn.Id] = struct{}{}
		if matchesLabelFilters(dn.Labels, filters) {
			result = append(result, dn)
		}
	}
	return result, nil
}

// inspectDockerNetwork returns a network by name or ID prefix.
func inspectDockerNetwork(ctx context.Context, name string) (*dockerNetwork, error) {
	conflists, err := loadCNIConflists()
	if err != nil {
		return nil, fmt.Errorf("list cni conflists: %w", err)
	}
	for _, cl := range conflists {
		if cl.Name != name && !(cl.AnvilID != "" && strings.HasPrefix(cl.AnvilID, name)) {
			continue
		}
		dn := conflistToDockerNetwork(cl)
		return &dn, nil
	}
	return nil, fmt.Errorf("No such network: %s", name)
}

// networkLabelsPath returns the host-share path where we persist Compose labels
// for a network. The conflist is the source of truth; it reliably returns the
// labels passed at create time, so we keep our own copy.
func networkLabelsPath(name string) string {
	base := sanitizeCNIName(name)
	return filepath.Join(anvilRunDir, "networks", base+".json")
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

// mergeNetworkLabels combines conflist labels with persisted Compose labels.
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
// cold boot. containerd keeps its state on the persistent disk, but
// the CNI conflist lives on tmpfs and is lost on reboot. Without the conflist
// Docker Compose sees the network as "not created by compose" and refuses to
// use it.
func restoreNetworkConfigs() {
	restored := map[string]bool{}
	// Current location plus the pre-.anvil-run share root (labels persisted
	// by older builds must still restore after upgrade).
	for _, dir := range []string{
		filepath.Join(anvilRunDir, "networks"),
		filepath.Join(anvilShareRoot, "networks"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".json")
			if restored[name] {
				continue
			}
			var labels map[string]string
			if data, rerr := os.ReadFile(filepath.Join(dir, e.Name())); rerr == nil {
				json.Unmarshal(data, &labels) //nolint:errcheck — empty labels on parse failure
			}
			if labels == nil {
				labels = map[string]string{}
			}
			if err := generateCNIConfigWithLabels(name, labels); err != nil {
				log.Printf("[docker-api] restore network config for %s: %v", name, err)
			} else {
				restored[name] = true
				log.Printf("[docker-api] restored network config for %s", name)
			}
		}
	}
}

// cniLabelsForNetwork reads the labels from the CNI conflist generated for the
// given network name, if it exists.
func cniLabelsForNetwork(name string) map[string]string {
	data, err := os.ReadFile(cniConflistPath(name))
	if err != nil {
		return map[string]string{}
	}
	var conf struct {
		Labels map[string]string `json:"anvilLabels"`
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
// The runtime consumes the conflist directly — there is no separate
// "network create" step beyond writing the file.
func createDockerNetwork(ctx context.Context, req dockerNetworkCreateRequest) (*dockerNetwork, error) {
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
	// otherwise inspectDockerNetwork would report the conflist as an existing
	// network and we would skip label persistence.
	if existing, err := inspectDockerNetwork(ctx, req.Name); err == nil && existing != nil {
		return existing, nil
	}

	// Pre-create the CNI config so the new network uses our deterministic subnet
	// and bridge name instead of an auto-generated one. Include any labels sent
	// by Compose so the network is recognised as Compose-managed. A conflist
	// in /etc/cni/net.d IS the network, so no further registration is needed.
	if err := generateCNIConfigWithLabels(req.Name, req.Labels); err != nil {
		log.Printf("[docker-api] ensure cni config for network %s: %v", req.Name, err)
	}

	// Build the response manually: Compose relies on the labels being
	// present in the create response.
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
func removeDockerNetwork(ctx context.Context, name string) error {
	// Resolve the network first so we can delete the persisted labels by name
	// even when the client sent the network ID.
	var netName string
	if nw, err := inspectDockerNetwork(ctx, name); err == nil && nw != nil {
		netName = nw.Name
	}

	cl, err := pc.get(ctx)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}

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

	// The conflist file is keyed by network NAME; when the client sent a
	// network ID, inspectDockerNetwork already resolved it above.
	fileName := name
	if netName != "" {
		fileName = netName
	}
	path := cniConflistPath(fileName)
	if _, statErr := os.Stat(path); statErr != nil {
		return fmt.Errorf("No such network: %s", name)
	}
	removeStaleBridge(path)

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove conflist: %w", err)
	}
	cnim.invalidate()
	if netName != "" {
		deleteNetworkLabels(netName)
	}
	return nil
}

// removeStaleBridge deletes the Linux bridge of a removed network when no
// interfaces remain attached to it (best effort).
func removeStaleBridge(conflistPath string) {
	data, err := os.ReadFile(conflistPath)
	if err != nil {
		return
	}
	var cl cniConflist
	if json.Unmarshal(data, &cl) != nil || len(cl.Plugins) == 0 || cl.Plugins[0].Bridge == "" {
		return
	}
	out, err := exec.Command("sh", "-c",
		fmt.Sprintf("if [ -d /sys/class/net/%s/brif ] && [ -z \"$(ls /sys/class/net/%s/brif)\" ]; then ip link delete %s; fi",
			cl.Plugins[0].Bridge, cl.Plugins[0].Bridge, cl.Plugins[0].Bridge)).CombinedOutput()
	if err != nil {
		debugLog("[docker-api] bridge cleanup %s: %v: %s", cl.Plugins[0].Bridge, err, out)
	}
}
