package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Anvil's own on-disk state root. Everything the Docker API emulation needs
// beyond what containerd itself persists lives here:
//
//	/var/lib/anvil/containers/<ns>/<id>/config.json   create-time metadata
//	/var/lib/anvil/containers/<ns>/<id>/hosts         etchosts file (bind-mounted)
//	/var/lib/anvil/containers/<ns>/<id>/resolv.conf   DNS config (bind-mounted)
//	/var/lib/anvil/containers/<ns>/<id>/<id>.json     json-file task log
//	/var/lib/anvil/volumes/<ns>/<name>/               named volume data
//
// The mount is declared persistent in the initramfs stage2 so the state
// survives resume/reboot like /var/lib/containerd does.
var (
	anvilStoreRoot = "/var/lib/anvil"
)

const (
	// Container label keys. These are set on containerd containers at create
	// time and double as the query interface for name/network lookups.
	labelName             = "anvil/name"
	labelNetworks         = "anvil/networks"
	labelPorts            = "anvil/ports"
	labelMounts           = "anvil/mounts"
	labelAnonymousVolumes = "anvil/anonymous-volumes"
	labelDefaultNetwork   = "anvil/default-network"
)

// containerMetaDir returns /var/lib/anvil/containers/<ns>/<id>.
func containerMetaDir(ns, id string) string {
	return filepath.Join(anvilStoreRoot, "containers", ns, id)
}

// containerHostsPath returns the container's etchosts file path.
func containerHostsPath(ns, id string) string {
	return filepath.Join(containerMetaDir(ns, id), "hosts")
}

// containerResolvPath returns the container's resolv.conf path.
func containerResolvPath(ns, id string) string {
	return filepath.Join(containerMetaDir(ns, id), "resolv.conf")
}

// containerLogPath returns the container's json-file task log path.
func containerLogPath(ns, id string) string {
	return filepath.Join(containerMetaDir(ns, id), id+".json")
}

// volumeDataDir returns the data directory of a named volume.
func volumeDataDir(ns, name string) string {
	return filepath.Join(anvilStoreRoot, "volumes", ns, name)
}

// containerMeta is the persisted create-time state of one container. It is
// written once at create and removed at delete; runtime state (exit codes,
// attach counts, restart policies) stays in memory exactly as before. The
// containerd id is encoded in the directory name and mirrored into ID.
type containerMeta struct {
	ID               string             `json:"ID"`
	Name             string             `json:"Name"`
	Namespace        string             `json:"Namespace"`
	ImageRef         string             `json:"ImageRef,omitempty"`
	Ports            []cniPortMapping   `json:"Ports,omitempty"`
	Networks         []string           `json:"Networks,omitempty"`
	Aliases          []string           `json:"Aliases,omitempty"`
	Links            []string           `json:"Links,omitempty"`
	TTY              bool               `json:"TTY,omitempty"`
	AutoRemove       bool               `json:"AutoRemove,omitempty"`
	StopSignal       string             `json:"StopSignal,omitempty"`
	WorkingDir       string             `json:"WorkingDir,omitempty"`
	Entrypoint       []string           `json:"Entrypoint,omitempty"`
	Mounts           []dockerMount      `json:"Mounts,omitempty"`
	AnonymousVolumes []string           `json:"AnonymousVolumes,omitempty"`
	Healthcheck      *dockerHealthcheck `json:"Healthcheck,omitempty"`
	// HostConfig snapshot of the create request, echoed back by inspect for
	// fields owned by the spec (memory, cpus, caps, ...) that have no other
	// persisted representation.
	HostConfig *dockerHostConfig `json:"HostConfig,omitempty"`
}

var metaMu sync.Mutex

// saveContainerMeta atomically writes the container's metadata file,
// creating the per-container directory as needed.
func saveContainerMeta(m *containerMeta) error {
	if m.Namespace == "" || m.ID == "" {
		return fmt.Errorf("containerMeta: namespace and id required")
	}
	metaMu.Lock()
	defer metaMu.Unlock()
	dir := containerMetaDir(m.Namespace, m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "config.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "config.json"))
}

// loadContainerMeta reads the metadata file for a container.
func loadContainerMeta(ns, id string) (*containerMeta, error) {
	metaMu.Lock()
	defer metaMu.Unlock()
	data, err := os.ReadFile(filepath.Join(containerMetaDir(ns, id), "config.json"))
	if err != nil {
		return nil, err
	}
	var m containerMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	m.ID = id
	if m.Namespace == "" {
		m.Namespace = ns
	}
	return &m, nil
}

// deleteContainerMeta removes the whole per-container metadata directory.
func deleteContainerMeta(ns, id string) {
	metaMu.Lock()
	defer metaMu.Unlock()
	os.RemoveAll(containerMetaDir(ns, id))
}

// containerMetas lists all persisted metas across namespaces (used by boot
// cleanup and reconciliation paths).
func containerMetas() ([]*containerMeta, error) {
	metaMu.Lock()
	defer metaMu.Unlock()
	nsDirs, err := os.ReadDir(filepath.Join(anvilStoreRoot, "containers"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*containerMeta
	for _, nd := range nsDirs {
		if !nd.IsDir() {
			continue
		}
		idDirs, err := os.ReadDir(filepath.Join(anvilStoreRoot, "containers", nd.Name()))
		if err != nil {
			continue
		}
		for _, idd := range idDirs {
			if !idd.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(anvilStoreRoot, "containers", nd.Name(), idd.Name(), "config.json"))
			if err != nil {
				continue
			}
			var m containerMeta
			if err := json.Unmarshal(data, &m); err != nil {
				continue
			}
			m.ID = idd.Name()
			if m.Namespace == "" {
				m.Namespace = nd.Name()
			}
			out = append(out, &m)
		}
	}
	return out, nil
}
