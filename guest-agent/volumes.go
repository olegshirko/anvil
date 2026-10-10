package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Named volumes are plain directories under the anvil state root:
//
//	/var/lib/anvil/volumes/<ns>/<name>/        volume data
//	/var/lib/anvil/volumes/<ns>/<name>.json    metadata (labels)
//
// The data directories are what containers get bind-mounted; the sidecar
// JSON files keep Docker-visible labels without polluting the data.

// dockerVolume matches the JSON returned by GET /volumes and /volumes/{name}.
type dockerVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
	Scope      string            `json:"Scope"`
}

// dockerVolumeList is the response for GET /volumes.
type dockerVolumeList struct {
	Volumes  []dockerVolume `json:"Volumes"`
	Warnings []string       `json:"Warnings"`
}

// dockerVolumeCreateRequest mirrors Docker's POST /volumes/create body.
type dockerVolumeCreateRequest struct {
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	Options map[string]string `json:"DriverOpts"`
	Labels  map[string]string `json:"Labels"`
}

// volumeMetaPath returns the labels file path of a volume.
func volumeMetaPath(ns, name string) string {
	return filepath.Join(anvilStoreRoot, "volumes", ns, name+".json")
}

// loadVolumeLabels reads the labels of one volume (empty when absent).
func loadVolumeLabels(ns, name string) map[string]string {
	labels := map[string]string{}
	if data, err := os.ReadFile(volumeMetaPath(ns, name)); err == nil {
		json.Unmarshal(data, &labels) //nolint:errcheck — defaults to empty
	}
	return labels
}

// saveVolumeLabels writes the labels of one volume.
func saveVolumeLabels(ns, name string, labels map[string]string) error {
	data, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	return os.WriteFile(volumeMetaPath(ns, name), data, 0o644)
}

// volumeDirs lists all volume data directories across namespaces.
func volumeDirs() ([]struct{ ns, name string }, error) {
	type volRef = struct{ ns, name string }
	base := filepath.Join(anvilStoreRoot, "volumes")
	nsEntries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []volRef
	for _, ne := range nsEntries {
		if !ne.IsDir() {
			continue // skip stray files
		}
		volEntries, err := os.ReadDir(filepath.Join(base, ne.Name()))
		if err != nil {
			continue
		}
		for _, ve := range volEntries {
			if ve.IsDir() {
				out = append(out, volRef{ns: ne.Name(), name: ve.Name()})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ns != out[j].ns {
			return out[i].ns < out[j].ns
		}
		return out[i].name < out[j].name
	})
	return out, nil
}

// A named volume is one volume across the whole daemon, as in Docker. Older
// anvils created API volumes in "default" but mounted a container's named
// volume from its own namespace, so a compose project got an empty second
// copy next to the one compose created: `compose down -v` removed the empty
// one and the data survived. The copies of one name are still on disk; the
// helpers below treat them as one volume.

// volumeCopies returns the namespaces holding a directory for volume name,
// the one containers mount first: a copy in a project namespace holds the
// data an older anvil wrote, the "default" one is an empty twin then.
func volumeCopies(name string) []string {
	dirs, _ := volumeDirs()
	var nss []string
	for _, d := range dirs {
		if d.name == name {
			nss = append(nss, d.ns)
		}
	}
	sort.SliceStable(nss, func(i, j int) bool { return nss[i] != "default" && nss[j] == "default" })
	return nss
}

// volumeNamespace returns where a container in ns finds named volume name:
// its own namespace when an older anvil put a copy there, else wherever the
// volume exists, else "default" (where it is then created, so that
// `docker volume rm` and `compose down -v` find it).
func volumeNamespace(ns, name string) string {
	if fi, err := os.Stat(volumeDataDir(ns, name)); err == nil && fi.IsDir() {
		return ns
	}
	if nss := volumeCopies(name); len(nss) > 0 {
		return nss[0]
	}
	return "default"
}

// dockerVolumeOf describes volume name held in namespaces nss (non-empty,
// mounted copy first). Labels and options of every copy are merged, the
// mounted copy's winning.
func dockerVolumeOf(name string, nss []string) dockerVolume {
	labels, opts := map[string]string{}, map[string]string{}
	for i := len(nss) - 1; i >= 0; i-- {
		maps.Copy(labels, loadVolumeLabels(nss[i], name))
		maps.Copy(opts, readVolumeOptions(nss[i], name))
	}
	return dockerVolume{
		Name:       name,
		Driver:     "local",
		Mountpoint: volumeDataDir(nss[0], name),
		CreatedAt:  volumeCreatedAt(nss[0], name),
		Labels:     labels,
		Options:    opts,
		Scope:      "local",
	}
}

// listDockerVolumes returns every volume once.
func listDockerVolumes(ctx context.Context, filters map[string]map[string]bool) ([]dockerVolume, error) {
	dirs, err := volumeDirs()
	if err != nil {
		return nil, err
	}
	var names []string
	seen := map[string]bool{}
	for _, d := range dirs {
		if !seen[d.name] {
			seen[d.name] = true
			names = append(names, d.name)
		}
	}
	sort.Strings(names)
	result := make([]dockerVolume, 0, len(names))
	for _, name := range names {
		dv := dockerVolumeOf(name, volumeCopies(name))
		if matchesLabelFilters(dv.Labels, filters) {
			result = append(result, dv)
		}
	}
	return result, nil
}

// inspectDockerVolume returns a volume by name.
func inspectDockerVolume(ctx context.Context, name string) (*dockerVolume, error) {
	nss := volumeCopies(name)
	if len(nss) == 0 {
		return nil, fmt.Errorf("No such volume: %s", name)
	}
	dv := dockerVolumeOf(name, nss)
	return &dv, nil
}

// createDockerVolume creates a volume (in the default namespace). Of the
// local driver's options, the bind form (type=none, o=bind, device=<path>)
// is honored at mount time; others are recorded.
func createDockerVolume(ctx context.Context, req dockerVolumeCreateRequest) (*dockerVolume, error) {
	const ns = "default"
	// An existing volume is returned as it is, as Docker does. A copy an
	// older anvil made when a container mounted the name before compose
	// created it has no labels: it takes the request's, so compose
	// recognizes its volume.
	if nss := volumeCopies(req.Name); len(nss) > 0 {
		if dv := dockerVolumeOf(req.Name, nss); len(dv.Labels) == 0 && len(req.Labels) > 0 {
			saveVolumeLabels(nss[0], req.Name, req.Labels) //nolint:errcheck — cosmetic
		}
		dv := dockerVolumeOf(req.Name, nss)
		return &dv, nil
	}
	if dev, ok := bindDeviceOption(req.Options); ok {
		if _, err := os.Stat(dev); err != nil {
			return nil, &apiError{status: http.StatusBadRequest,
				msg: fmt.Sprintf("failed to mount local volume: mount %s: no such file or directory", dev)}
		}
	}
	dir := volumeDataDir(ns, req.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create volume %s: %w", req.Name, err)
	}
	labels := req.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	saveVolumeLabels(ns, req.Name, labels) //nolint:errcheck — cosmetic
	if len(req.Options) > 0 {
		saveVolumeOptions(ns, req.Name, req.Options) //nolint:errcheck
	}
	publishObjectEvent("volume", "create", req.Name, map[string]string{"driver": "local"})
	return &dockerVolume{
		Name:       req.Name,
		Driver:     "local",
		Mountpoint: dir,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		Labels:     labels,
		Options:    nonNilMap(req.Options),
		Scope:      "local",
	}, nil
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// volumeOptionsPath stores a volume's driver options.
func volumeOptionsPath(ns, name string) string {
	return filepath.Join(anvilStoreRoot, "volumes", ns, name+".opts.json")
}

func saveVolumeOptions(ns, name string, opts map[string]string) error {
	data, err := json.Marshal(opts)
	if err != nil {
		return err
	}
	return os.WriteFile(volumeOptionsPath(ns, name), data, 0o644)
}

// readVolumeOptions reads one volume's driver options (nil when none).
func readVolumeOptions(ns, name string) map[string]string {
	var opts map[string]string
	if data, err := os.ReadFile(volumeOptionsPath(ns, name)); err == nil {
		json.Unmarshal(data, &opts) //nolint:errcheck
	}
	return opts
}

// loadVolumeOptions finds a volume's driver options in any namespace
// (volumes are created in "default", mounted from project namespaces).
func loadVolumeOptions(name string) map[string]string {
	dirs, _ := volumeDirs()
	for _, d := range dirs {
		if d.name != name {
			continue
		}
		var opts map[string]string
		if data, err := os.ReadFile(volumeOptionsPath(d.ns, d.name)); err == nil && json.Unmarshal(data, &opts) == nil {
			return opts
		}
	}
	return nil
}

// bindDeviceOption recognizes the local driver's bind form — compose's
// `driver_opts: {type: none, o: bind, device: /path}` — and returns the
// device to bind-mount in place of the volume's own directory.
func bindDeviceOption(opts map[string]string) (string, bool) {
	if opts["device"] == "" || (opts["type"] != "none" && opts["type"] != "") {
		return "", false
	}
	for _, o := range strings.Split(opts["o"], ",") {
		if o == "bind" || o == "rbind" {
			return opts["device"], true
		}
	}
	return "", false
}

// removeDockerVolume removes a volume — every copy of its name. A volume a
// container mounts (running or not) is refused, as by Docker.
func removeDockerVolume(ctx context.Context, name string) error {
	nss := volumeCopies(name)
	if len(nss) == 0 {
		return fmt.Errorf("No such volume: %s", name)
	}
	mounted, err := mountedBindSources(ctx)
	if err != nil {
		return fmt.Errorf("remove %s: list container mounts: %w", name, err)
	}
	if volumeCopiesInUse(mounted, name, nss) {
		return &apiError{status: http.StatusConflict, msg: fmt.Sprintf("remove %s: volume is in use", name)}
	}
	for _, ns := range nss {
		if err := os.RemoveAll(volumeDataDir(ns, name)); err != nil {
			return err
		}
		os.Remove(volumeMetaPath(ns, name))
		os.Remove(volumeOptionsPath(ns, name))
	}
	publishObjectEvent("volume", "destroy", name, map[string]string{"driver": "local"})
	return nil
}

// volumeCopiesInUse reports whether a container mounts any copy of volume
// name.
func volumeCopiesInUse(mounted map[string]bool, name string, nss []string) bool {
	for _, ns := range nss {
		if volumeInUse(mounted, volumeDataDir(ns, name)) {
			return true
		}
	}
	return false
}
