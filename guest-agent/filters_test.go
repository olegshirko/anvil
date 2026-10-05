package main

import (
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestMatchesContainerFiltersExtended(t *testing.T) {
	s := dockerContainerSummary{
		Id: "abcdef0123", Names: []string{"/web"}, Image: "docker.io/library/nginx:latest",
		State: "exited", Labels: map[string]string{"app": "x"},
		Ports:    []dockerPort{{PrivatePort: 80, PublicPort: 8080, Type: "tcp"}},
		Mounts:   []dockerMountPoint{{Name: "data", Destination: "/data"}},
		exitCode: 3, networks: []string{"proj_default"}, health: "healthy", exposed: []string{"80/tcp"},
	}
	f := func(k, v string) map[string]map[string]bool { return map[string]map[string]bool{k: {v: true}} }
	yes := []map[string]map[string]bool{
		f("ancestor", "nginx"), f("ancestor", "nginx:latest"), f("network", "proj_default"),
		f("health", "healthy"), f("exited", "3"), f("volume", "data"), f("volume", "/data"),
		f("publish", "8080"), f("publish", "8000-8100/tcp"), f("expose", "80"), f("expose", "80/tcp"),
		f("status", "exited"), f("label", "app=x"),
	}
	no := []map[string]map[string]bool{
		f("ancestor", "alpine"), f("network", "other"), f("health", "unhealthy"), f("exited", "0"),
		f("volume", "nope"), f("publish", "80"), f("expose", "443"), f("status", "running"),
	}
	for _, flt := range yes {
		if !matchesContainerFilters(s, flt) {
			t.Errorf("%v should match", flt)
		}
	}
	for _, flt := range no {
		if matchesContainerFilters(s, flt) {
			t.Errorf("%v should not match", flt)
		}
	}
	if err := validateFilterKeys(f("bogus", "1"), containerFilterKeys); err == nil {
		t.Error("unknown filter key accepted")
	}
}

func TestPruneFilter(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	until, err := parseUntil("24h", now)
	if err != nil || !until.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("24h -> %v %v", until, err)
	}
	if _, err := parseUntil("2026-09-01", now); err != nil {
		t.Fatal(err)
	}
	if _, err := newPruneFilter(map[string]map[string]bool{"bogus": {"x": true}}); err == nil {
		t.Fatal("unknown prune filter accepted")
	}
	pf := pruneFilter{until: now, filters: map[string]map[string]bool{"label": {"keep=no": true}, "label!": {"pin": true}}}
	if !pf.keep(map[string]string{"keep": "no"}, now.Add(-time.Hour)) {
		t.Error("old, labelled object should be pruned")
	}
	if pf.keep(map[string]string{"keep": "no"}, now.Add(time.Hour)) {
		t.Error("object newer than until pruned")
	}
	if pf.keep(map[string]string{"keep": "no", "pin": "1"}, now.Add(-time.Hour)) {
		t.Error("label! object pruned")
	}
	if pf.keep(map[string]string{}, now.Add(-time.Hour)) {
		t.Error("object without the label pruned")
	}
}

func TestResolvConfContent(t *testing.T) {
	base := "# generated\nnameserver 192.168.64.1\nsearch lan\noptions edns0\n"
	if got := resolvConfContent(base, nil, nil, nil); got != "# generated\nnameserver 192.168.64.1\nsearch lan\noptions edns0\n" {
		t.Errorf("unchanged base: %q", got)
	}
	got := resolvConfContent(base, []string{"1.1.1.1"}, []string{"a.test", "b.test"}, []string{"ndots:2"})
	want := "# generated\nnameserver 1.1.1.1\nsearch a.test b.test\noptions ndots:2\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := resolvConfContent(base, nil, []string{"."}, nil); got != "# generated\nnameserver 192.168.64.1\noptions edns0\n" {
		t.Errorf("--dns-search . should clear search: %q", got)
	}
}

// k3d looks networks and containers up with id=^/?<id>$ (Docker matches
// filter values as regular expressions); a plain prefix must still work.
func TestIDFilterMatch(t *testing.T) {
	id := "3f1c9a0b7d2e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a"
	for _, tc := range []struct {
		pattern string
		want    bool
	}{
		{id, true},
		{id[:12], true},
		{"^/?" + id + "$", true},
		{"^" + id[:12] + "$", false},
		{"^/?deadbeef$", false},
		{"[", false}, // invalid expression: prefix fallback
		{"", false},
	} {
		if got := idFilterMatch(tc.pattern, id); got != tc.want {
			t.Errorf("idFilterMatch(%q) = %v, want %v", tc.pattern, got, tc.want)
		}
	}
	s := dockerContainerSummary{Id: id, Names: []string{"/c"}}
	if !matchesContainerFilters(s, map[string]map[string]bool{"id": {"^/?" + id + "$": true}}) {
		t.Error("container id filter does not take a regular expression")
	}
}

// Docker joins the NetworkMode network only when EndpointsConfig is empty
// (k3d: NetworkMode "bridge" + its cluster network in EndpointsConfig).
func TestPrimaryNetworkMode(t *testing.T) {
	req := func(mode string, eps ...string) dockerCreateRequest {
		var r dockerCreateRequest
		r.HostConfig.NetworkMode = mode
		if len(eps) > 0 {
			r.NetworkingConfig = &dockerNetworkingConf{EndpointsConfig: map[string]dockerEndpoint{}}
			for _, e := range eps {
				r.NetworkingConfig.EndpointsConfig[e] = dockerEndpoint{}
			}
		}
		return r
	}
	for _, tc := range []struct {
		r    dockerCreateRequest
		want string
	}{
		{req("bridge", "k3d-x"), "k3d-x"},
		{req("", "k3d-x"), "k3d-x"},
		{req("default", "b", "a"), "a"},
		{req("bridge"), "bridge"},
		{req("bridge", "bridge", "k3d-x"), "bridge"},
		{req("app_default", "app_default", "other"), "app_default"},
		{req("host", "k3d-x"), "host"},
	} {
		if got := primaryNetworkMode(tc.r); got != tc.want {
			t.Errorf("primaryNetworkMode(%q, %v) = %q, want %q", tc.r.HostConfig.NetworkMode, tc.r.NetworkingConfig, got, tc.want)
		}
	}
}

// Docker mounts parents first: a tmpfs on /run must not cover a bind on
// /var/run/docker.sock requested before it.
func TestSortMountsByDepth(t *testing.T) {
	in := []specs.Mount{
		{Destination: "/var/run/docker.sock"},
		{Destination: "/run"},
		{Destination: "/var/run"},
		{Destination: "/etc/hosts"},
		{Destination: "/data/"},
	}
	var got []string
	for _, m := range sortMountsByDepth(in) {
		got = append(got, m.Destination)
	}
	want := []string{"/run", "/data/", "/var/run", "/etc/hosts", "/var/run/docker.sock"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("order = %v, want %v", got, want)
	}
}
