package main

import (
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Container domains: the host serves http://<name>.anvil.localhost (opt-in,
// ANVIL_DOMAINS=1) by proxying to the container named here, through the
// port proxy, without any -p. Names are the container name and, for compose
// services, <service>.<project>. The port is the label dev.anvil.http.port,
// else 80 when exposed, else the lowest exposed TCP port, else 80.

// DomainEntry is one container in the pushed domain table.
type DomainEntry struct {
	Names []string `json:"names"`
	IP    string   `json:"ip"`
	Port  int      `json:"port"`
}

const domainPortLabel = "dev.anvil.http.port"

// dnsLabel lowercases s and maps characters outside [a-z0-9-] to '-'.
func dnsLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func containerDomains(name string, labels map[string]string) []string {
	var out []string
	if n := dnsLabel(strings.TrimPrefix(name, "/")); n != "" {
		out = append(out, n)
	}
	svc, proj := dnsLabel(labels["com.docker.compose.service"]), dnsLabel(labels["com.docker.compose.project"])
	if svc != "" && proj != "" {
		out = append(out, svc+"."+proj)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func containerHTTPPort(exposed []string, labels map[string]string) int {
	if p, err := strconv.Atoi(labels[domainPortLabel]); err == nil && p > 0 && p < 65536 {
		return p
	}
	lowest := 0
	for _, e := range exposed {
		portStr, proto, _ := strings.Cut(e, "/")
		if proto != "" && proto != "tcp" {
			continue
		}
		p, err := strconv.Atoi(portStr)
		if err != nil || p <= 0 {
			continue
		}
		if p == 80 {
			return 80
		}
		if lowest == 0 || p < lowest {
			lowest = p
		}
	}
	if lowest > 0 {
		return lowest
	}
	return 80
}

func sortDomains(d []DomainEntry) {
	sort.Slice(d, func(i, j int) bool { return strings.Join(d[i].Names, ",") < strings.Join(d[j].Names, ",") })
}

func domainsEqual(a, b []DomainEntry) bool {
	return slices.EqualFunc(a, b, func(x, y DomainEntry) bool {
		return x.IP == y.IP && x.Port == y.Port && slices.Equal(x.Names, y.Names)
	})
}
