package main

import (
	"path/filepath"
	"slices"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// docker inspect .Mounts. Compose reads it when it recreates a service: the
// old container's anonymous volumes are handed to the new one by name, so
// without it a database's VOLUME came back empty after every
// `compose up` that changed the service.

type dockerMountPoint struct {
	Type        string `json:"Type"`
	Name        string `json:"Name,omitempty"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Driver      string `json:"Driver,omitempty"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
	Propagation string `json:"Propagation"`
}

// labelAnonymousVolume marks a volume anvil created without a name, as
// Docker does; rm -v removes such volumes wherever they are mounted from.
const labelAnonymousVolume = "com.docker.volume.anonymous"

// markAnonymousVolume records that a volume is anonymous.
func markAnonymousVolume(ns, name string) {
	labels := loadVolumeLabels(ns, name)
	labels[labelAnonymousVolume] = ""
	saveVolumeLabels(ns, name, labels) //nolint:errcheck — best effort bookkeeping
}

func isAnonymousVolume(ns, name string) bool {
	_, ok := loadVolumeLabels(ns, name)[labelAnonymousVolume]
	return ok
}

// volumeRefFromDir maps a volume data directory back to (namespace, name).
func volumeRefFromDir(dir string) (string, string, bool) {
	rel, err := filepath.Rel(filepath.Join(anvilStoreRoot, "volumes"), filepath.Clean(dir))
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// mountPointsFor renders the user-visible mounts of a container spec: volumes
// (named or anonymous), host binds and tmpfs — not the /etc files, docker-init
// or the Rosetta socket anvil adds itself.
func mountPointsFor(mounts []specs.Mount) []dockerMountPoint {
	out := []dockerMountPoint{}
	for _, m := range mounts {
		switch m.Destination {
		case "/etc/hosts", "/etc/resolv.conf", "/etc/hostname", containerInitPath, rosettaCacheSocket:
			continue
		}
		rw := !slices.Contains(m.Options, "ro")
		switch m.Type {
		case "tmpfs":
			out = append(out, dockerMountPoint{Type: "tmpfs", Destination: m.Destination, RW: rw})
		case "bind":
			if _, name, ok := volumeRefFromDir(m.Source); ok {
				out = append(out, dockerMountPoint{Type: "volume", Name: name, Source: m.Source,
					Destination: m.Destination, Driver: "local", RW: rw})
				continue
			}
			out = append(out, dockerMountPoint{Type: "bind", Source: m.Source, Destination: m.Destination,
				RW: rw, Propagation: "rprivate"})
		}
	}
	return out
}
