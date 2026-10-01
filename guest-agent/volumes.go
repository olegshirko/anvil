package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// listDockerVolumes returns volumes from all namespaces.
func listDockerVolumes(ctx context.Context, filters map[string]map[string]bool) ([]dockerVolume, error) {
	dirs, err := volumeDirs()
	if err != nil {
		return nil, err
	}
	result := make([]dockerVolume, 0, len(dirs))
	for _, d := range dirs {
		labels := loadVolumeLabels(d.ns, d.name)
		dv := dockerVolume{
			Name:       d.name,
			Driver:     "local",
			Mountpoint: volumeDataDir(d.ns, d.name),
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
			Labels:     labels,
			Options:    nonNilMap(readVolumeOptions(d.ns, d.name)),
			Scope:      "local",
		}
		if matchesLabelFilters(labels, filters) {
			result = append(result, dv)
		}
	}
	return result, nil
}

// inspectDockerVolume returns a volume by name from any namespace.
func inspectDockerVolume(ctx context.Context, name string) (*dockerVolume, error) {
	dirs, err := volumeDirs()
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if d.name != name {
			continue
		}
		return &dockerVolume{
			Name:       d.name,
			Driver:     "local",
			Mountpoint: volumeDataDir(d.ns, d.name),
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
			Labels:     loadVolumeLabels(d.ns, d.name),
			Options:    nonNilMap(readVolumeOptions(d.ns, d.name)),
			Scope:      "local",
		}, nil
	}
	return nil, fmt.Errorf("No such volume: %s", name)
}

// createDockerVolume creates a volume in the default namespace. Of the
// local driver's options, the bind form (type=none, o=bind, device=<path>)
// is honored at mount time; others are recorded.
func createDockerVolume(ctx context.Context, req dockerVolumeCreateRequest) (*dockerVolume, error) {
	const ns = "default"
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

// removeDockerVolume removes a volume by name from any namespace.
func removeDockerVolume(ctx context.Context, name string) error {
	dirs, err := volumeDirs()
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if d.name != name {
			continue
		}
		if err := os.RemoveAll(volumeDataDir(d.ns, d.name)); err != nil {
			return err
		}
		os.Remove(volumeMetaPath(d.ns, d.name))
		os.Remove(volumeOptionsPath(d.ns, d.name))
		publishObjectEvent("volume", "destroy", d.name, map[string]string{"driver": "local"})
		return nil
	}
	return fmt.Errorf("No such volume: %s", name)
}
