package main

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
)

// Cross-container name resolution. Docker serves 127.0.0.11 (an embedded
// DNS resolver) that answers container names and network aliases; we emulate
// the resolver's observable behaviour with a managed section of each
// container's /etc/hosts bind mount:
//
//	# BEGIN ANVIL NETWORK ENTRIES
//	10.10.5.3	kafka-1 kafka
//	10.10.5.4	kafka-2 kafka
//	# END ANVIL NETWORK ENTRIES
//
// Whenever a container joins (start) or leaves (stop/delete) a network, the
// whole section is REGENERATED for every member of that network from the
// persisted container metadata (name + compose aliases) and live CNI
// addresses. Regeneration — not appending — is what makes the mesh complete:
// a container created after its peers still resolves them, and a stopped
// container's entries disappear everywhere at once.

const (
	netHostsBegin = "# BEGIN ANVIL NETWORK ENTRIES"
	netHostsEnd   = "# END ANVIL NETWORK ENTRIES"
)

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

// refreshHostsForContainer regenerates the managed hosts sections of every
// network the container belongs to. Called after start (the CNI address is
// only known then) and after stop/delete/rename.
func refreshHostsForContainer(ns, containerdID string) {
	meta, err := loadContainerMeta(ns, containerdID)
	if err != nil {
		return
	}
	for _, net := range meta.Networks {
		refreshNetworkHosts(net)
	}
}

// refreshNetworkHosts regenerates the managed section of the hosts file of
// every container on the network. Members come from persisted metadata
// (create-time network membership); entries only for members that currently
// have a CNI address (running tasks — net.json is removed on stop).
func refreshNetworkHosts(network string) {
	metas, err := containerMetas()
	if err != nil {
		return
	}
	var members []*containerMeta
	var entries []string
	for _, m := range metas {
		if !stringIn(m.Networks, network) {
			continue
		}
		members = append(members, m)
		names := append([]string{m.Name}, m.Aliases...)
		if ip := containerAddress(m.Namespace, m.ID); ip != "" && containerOnNetwork(m.Namespace, m.ID, network) {
			entries = append(entries, ip+"\t"+strings.Join(dedupeStrings(names), " "))
		}
	}
	block := ""
	if len(entries) > 0 {
		block = netHostsBegin + "\n" + strings.Join(entries, "\n") + "\n" + netHostsEnd + "\n"
	}
	for _, m := range members {
		rewriteHostsManagedSection(containerHostsPath(m.Namespace, m.ID), block)
	}
	log.Printf("[net-alias] network %s refreshed: %d entries across %d members", network, len(entries), len(members))
}

// containerOnNetwork reports whether the container's live CNI endpoint
// (net.json) is currently attached to the named network.
func containerOnNetwork(ns, id, network string) bool {
	if ni, ok := loadNetInfo(ns, id); ok {
		return ni.Network == network
	}
	return false
}

// rewriteHostsManagedSection replaces the anvil-managed block of a hosts
// file (everything between the markers) with block. Files without markers
// (freshly created) simply get the block appended.
func rewriteHostsManagedSection(path, block string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // not created yet; create will include nothing stale
	}
	content := string(data)
	begin := strings.Index(content, netHostsBegin)
	end := strings.LastIndex(content, netHostsEnd)
	if begin >= 0 && end > begin {
		rest := content[end+len(netHostsEnd):]
		content = strings.TrimRight(content[:begin], "\n") + "\n" + block + strings.TrimLeft(rest, "\n")
	} else {
		content = strings.TrimRight(content, "\n") + "\n" + block
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		log.Printf("[net-alias] write %s: %v", path, err)
	}
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
func applyLinkAliases(ctx context.Context, ns, containerdID string, links []string) {
	entries := linkAliases(ctx, ns, links)
	if len(entries) == 0 {
		return
	}
	matches := []string{containerHostsPath(ns, containerdID)}
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
func linkAliases(ctx context.Context, ns string, links []string) []string {
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
		tns, tid, _, err := resolveDockerID(ctx, target)
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
// persisted net.json (written by the start-time CNI attach), falling back to
// the derived network info.
func containerAddress(ns, containerdID string) string {
	if ni, ok := loadNetInfo(ns, containerdID); ok && ni.IP != "" {
		return ni.IP
	}
	_, ip := containerNetworkInfo(ns, containerdID, "")
	return ip
}

func stringIn(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
