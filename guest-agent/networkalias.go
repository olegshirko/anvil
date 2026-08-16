package main

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// nerdctl has no --network-alias flag, but compose resolves services by
// their service name ("db", "web"), which the CLI passes as Aliases in the
// create request's NetworkingConfig. We remember the aliases at create time
// and, once the container starts and has a CNI address, append
// "<ip> <aliases...>" to the /etc/hosts bind mounts of every running
// container on the same network (nerdctl generates those files itself, and
// each container's /etc/hosts is a bind mount of the file on the persistent
// disk — see /var/lib/nerdctl/<ds>/etchosts/<ns>/<id>/hosts).

var networkAliases = struct {
	mu sync.Mutex
	m  map[string][]string // dockerID -> aliases
}{m: make(map[string][]string)}

func setNetworkAliases(dockerID string, aliases []string) {
	networkAliases.mu.Lock()
	networkAliases.m[dockerID] = aliases
	networkAliases.mu.Unlock()
}

func pendingNetworkAliases(containerdID string) []string {
	networkAliases.mu.Lock()
	defer networkAliases.mu.Unlock()
	for _, aliases := range networkAliases.m {
		if len(aliases) > 0 {
			return aliases
		}
	}
	return nil
}

func clearNetworkAliases(dockerID string) {
	networkAliases.mu.Lock()
	delete(networkAliases.m, dockerID)
	networkAliases.mu.Unlock()
}

// requestedNetworkAliases extracts service aliases from the create request.
// Compose sends them per endpoint network; any non-empty list is used.
func requestedNetworkAliases(req dockerCreateRequest) []string {
	if req.NetworkingConfig == nil {
		return nil
	}
	var out []string
	for _, ep := range req.NetworkingConfig.EndpointsConfig {
		for _, a := range ep.Aliases {
			if a = strings.TrimSpace(a); a != "" {
				out = append(out, a)
			}
		}
	}
	return out
}

var hostsEntryRe = regexp.MustCompile(`(?m)^(\d+\.\d+\.\d+\.\d+)\s+.*$`)

// applyNetworkAliases resolves the container's CNI address and appends the
// alias entry to the hosts file of every container directory in the same
// namespace (per-project networks map 1:1 to namespaces here). Idempotent:
// an entry for the alias is replaced, not duplicated.
func applyNetworkAliases(ns, containerdID string) {
	aliases := func() []string {
		networkAliases.mu.Lock()
		defer networkAliases.mu.Unlock()
		// Keyed by dockerID; find by matching is overkill — pop the first
		// pending set (create→start is serialized per container).
		for did, a := range networkAliases.m {
			delete(networkAliases.m, did)
			return a
		}
		return nil
	}()
	if len(aliases) == 0 {
		return
	}

	ip := containerAddress(ns, containerdID)
	if ip == "" {
		log.Printf("[net-alias] no address for %s/%s, aliases %v dropped", ns, containerdID[:12], aliases)
		return
	}
	entry := ip + "\t" + strings.Join(aliases, " ")

	matches, _ := filepath.Glob(filepath.Join(nerdctlStoreRoot, "*", "etchosts", ns, "*", "hosts"))
	for _, hostsPath := range matches {
		data, err := os.ReadFile(hostsPath)
		if err != nil {
			continue
		}
		content := string(data)
		// Skip containers that already know every alias.
		if strings.Contains(content, "\t"+aliases[0]+" ") || strings.HasSuffix(strings.TrimSpace(strings.Split(content, "\n")[len(strings.Split(content, "\n"))-1]), aliases[0]) {
			if strings.Contains(content, aliases[0]) {
				continue
			}
		}
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += entry + "\n"
		if err := os.WriteFile(hostsPath, []byte(content), 0o644); err != nil {
			log.Printf("[net-alias] write %s: %v", hostsPath, err)
		}
	}
	log.Printf("[net-alias] %s/%s (%s) aliased as %v across %d hosts files",
		ns, containerdID[:12], ip, aliases, len(matches))
}

// Container --link entries, remembered at create and applied at start
// (the target may not have an IP before then).
var containerLinks = struct {
	mu sync.Mutex
	m  map[string][]string // dockerID -> raw link specs ("name:alias")
}{m: make(map[string][]string)}

func setContainerLinks(dockerID string, links []string) {
	containerLinks.mu.Lock()
	if len(links) > 0 {
		containerLinks.m[dockerID] = links
	} else {
		delete(containerLinks.m, dockerID)
	}
	containerLinks.mu.Unlock()
}

func pendingLinkEntries(dockerID string) []string {
	containerLinks.mu.Lock()
	defer containerLinks.mu.Unlock()
	return containerLinks.m[dockerID]
}

// applyLinkAliases appends alias -> target-IP lines to this container's own
// /etc/hosts bind mount (the legacy docker --link contract).
func applyLinkAliases(ns, containerdID string, links []string) {
	entries := linkAliases(ns, links)
	if len(entries) == 0 {
		return
	}
	hostsPath := filepath.Join(nerdctlStoreRoot, "*", "etchosts", ns, containerdID, "hosts")
	matches, _ := filepath.Glob(hostsPath)
	written := 0
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		content := string(data)
		for _, e := range entries {
			alias := strings.Fields(e)
			if len(alias) == 2 && strings.Contains(content, alias[1]) {
				continue
			}
			if !strings.HasSuffix(content, "\n") {
				content += "\n"
			}
			content += e + "\n"
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			log.Printf("[net-alias] link write %s: %v", p, err)
			continue
		}
		written++
	}
	if written == 0 {
		// Hosts file not there yet (e.g. applied before create finalized);
		// keep the entries pending for the post-start application.
		return
	}
	setContainerLinks(dockerID(ns, containerdID), nil)
	log.Printf("[net-alias] applied %d link aliases to %s/%s", len(entries), ns, containerdID[:12])
}

// linkAliases returns extra /etc/hosts entries (alias -> target IP) for the
// docker --link flag: "name:alias" pairs referencing containers in the same
// namespace. Returns entries to append to this container's hosts file.
func linkAliases(ns string, links []string) []string {
	var out []string
	for _, l := range links {
		// Docker sends "/target:/consumer/alias" (or "target:alias"); the
		// link alias is the last path segment of the second part.
		parts := strings.SplitN(l, ":", 2)
		target := strings.TrimPrefix(parts[0], "/")
		alias := target
		if len(parts) == 2 && parts[1] != "" {
			segs := strings.Split(strings.TrimPrefix(parts[1], "/"), "/")
			if last := segs[len(segs)-1]; last != "" {
				alias = last
			}
		}
		tns, tid, _, err := resolveDockerID(target)
		if err != nil {
			continue
		}
		ip := containerAddress(tns, tid)
		if ip != "" {
			out = append(out, ip+"\t"+alias)
		}
	}
	return out
}

// containerAddress returns the container's first CNI IPv4 address from the
// nerdctl etchosts metadata (meta.json records the network config nerdctl's
// CNI returned on start), falling back to nerdctl inspect.
func containerAddress(ns, containerdID string) string {
	metaPath := filepath.Join(nerdctlStoreRoot, "*", "etchosts", ns, containerdID, "meta.json")
	matches, _ := filepath.Glob(metaPath)
	for _, p := range matches {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// The JSON is small; a focused regex avoids a full schema.
		m := regexp.MustCompile(`"address"\s*:\s*"(\d+\.\d+\.\d+\.\d+)/`).FindSubmatch(data)
		if len(m) == 2 {
			return string(m[1])
		}
	}
	_, ip := containerNetworkInfo(ns, containerdID, "")
	return ip
}
