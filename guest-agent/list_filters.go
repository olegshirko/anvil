package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Network and volume list filters beyond labels. Ignoring them made
// `docker network ls -qf name=x` (the usual "create unless it exists"
// check) and `docker volume ls -qf dangling=true` return everything.

var networkFilterKeys = map[string]bool{
	"name": true, "id": true, "driver": true, "type": true, "scope": true,
	"dangling": true, "label": true, "label!": true,
}

var volumeFilterKeys = map[string]bool{
	"name": true, "driver": true, "dangling": true, "label": true, "label!": true,
}

// filterNetworks applies the non-label network filters.
func filterNetworks(ctx context.Context, list []dockerNetwork, filters map[string]map[string]bool) []dockerNetwork {
	var used map[string]bool
	if len(filters["dangling"]) > 0 {
		used = networksInUse()
	}
	out := list[:0]
	for _, n := range list {
		builtin := n.Name == "bridge" || n.Name == "host" || n.Name == "none"
		ok := anyMatch(filters["name"], func(v string) bool { return nameFilterMatch(v, n.Name) }) &&
			anyMatch(filters["id"], func(v string) bool { return idFilterMatch(v, n.Id) }) &&
			anyMatch(filters["driver"], func(v string) bool { return v == n.Driver }) &&
			anyMatch(filters["scope"], func(v string) bool { return v == n.Scope || (n.Scope == "" && v == "local") }) &&
			anyMatch(filters["type"], func(v string) bool { return (v == "builtin") == builtin && (v == "builtin" || v == "custom") }) &&
			anyMatch(filters["dangling"], func(v string) bool { return queryBoolValue(v) == (!used[n.Name] && !builtin) })
		if ok {
			out = append(out, n)
		}
	}
	return out
}

// networksInUse returns the networks any container (running or not) is on.
func networksInUse() map[string]bool {
	used := map[string]bool{}
	metas, err := containerMetas()
	if err != nil {
		return used
	}
	for _, m := range metas {
		for _, n := range m.Networks {
			used[n] = true
		}
	}
	return used
}

// filterVolumes applies the non-label volume filters.
func filterVolumes(ctx context.Context, list []dockerVolume, filters map[string]map[string]bool) []dockerVolume {
	var mounted map[string]bool
	if len(filters["dangling"]) > 0 {
		mounted, _ = mountedBindSources(ctx)
	}
	out := list[:0]
	for _, v := range list {
		inUse := volumeInUse(mounted, v.Mountpoint)
		if dev, ok := bindDeviceOption(v.Options); ok && mounted[filepath.Clean(dev)] {
			inUse = true
		}
		ok := anyMatch(filters["name"], func(s string) bool { return nameFilterMatch(s, v.Name) }) &&
			anyMatch(filters["driver"], func(s string) bool { return s == v.Driver }) &&
			anyMatch(filters["dangling"], func(s string) bool { return queryBoolValue(s) == !inUse })
		if ok {
			out = append(out, v)
		}
	}
	return out
}

// queryBoolValue is Docker's truthiness for a filter value.
func queryBoolValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "no", "false", "none":
		return false
	}
	return true
}

// volumeCreatedAt is when the volume was created: its metadata file's
// mtime (written once at create), else its directory's.
func volumeCreatedAt(ns, name string) string {
	for _, p := range []string{volumeMetaPath(ns, name), volumeDataDir(ns, name)} {
		if fi, err := os.Stat(p); err == nil {
			return fi.ModTime().UTC().Format(time.RFC3339)
		}
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// nameFilterMatch is Docker's name filter: a regular expression (so
// `name=^x$` is exact), a plain substring when the pattern is not one.
func nameFilterMatch(pattern, name string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return strings.Contains(name, pattern)
	}
	return re.MatchString(name)
}

// idFilterMatch is Docker's id filter (filters.Args.Match): a regular
// expression, unanchored, so a plain ID prefix still matches and k3d's
// `id=^/?<id>$` does too. A pattern that is not a valid expression falls
// back to the prefix match it most likely meant.
func idFilterMatch(pattern, id string) bool {
	if pattern == "" {
		return false
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return strings.HasPrefix(id, pattern)
	}
	return re.MatchString(id)
}

// volumeInUse reports whether a mounted source is the volume's directory or
// lies inside it (a volume-subpath mount).
func volumeInUse(mounted map[string]bool, mountpoint string) bool {
	mp := filepath.Clean(mountpoint)
	if mounted[mp] {
		return true
	}
	for src := range mounted {
		if strings.HasPrefix(src, mp+"/") {
			return true
		}
	}
	return false
}
