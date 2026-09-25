package main

import (
	"reflect"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestContainerHostsContentDesktopNames(t *testing.T) {
	hosts := containerHostsContent("web", []string{
		"db.test:1.2.3.4",
		"eq.test=5.6.7.8",
		"v6.test:::1",
		"mac:host-gateway",
		"broken",
	}, "192.168.64.1")
	for _, want := range []string{
		"127.0.0.1\tweb\n",
		"1.2.3.4\tdb.test\n",
		"5.6.7.8\teq.test\n",
		"::1\tv6.test\n",
		"192.168.64.1\tmac\n",
		"192.168.64.1\thost.docker.internal\n",
		"192.168.64.1\tgateway.docker.internal\n",
	} {
		if !strings.Contains(hosts, want) {
			t.Errorf("hosts missing %q:\n%s", want, hosts)
		}
	}
	if strings.Contains(hosts, "broken") {
		t.Errorf("malformed entry leaked:\n%s", hosts)
	}

	// An explicit --add-host for a Desktop name replaces the built-in one.
	hosts = containerHostsContent("web", []string{"host.docker.internal:10.0.0.9"}, "192.168.64.1")
	if strings.Count(hosts, "host.docker.internal") != 1 || !strings.Contains(hosts, "10.0.0.9\thost.docker.internal\n") {
		t.Errorf("override not honored:\n%s", hosts)
	}
	if !strings.Contains(hosts, "192.168.64.1\tgateway.docker.internal\n") {
		t.Errorf("gateway.docker.internal lost:\n%s", hosts)
	}
}

func TestParseDefaultGateway(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t0040A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"eth0\t00000000\t0140A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
	if got := parseDefaultGateway(table); got != "192.168.64.1" {
		t.Errorf("gateway = %q, want 192.168.64.1", got)
	}
	if got := parseDefaultGateway(strings.SplitN(table, "\n", 3)[0] + "\n"); got != "" {
		t.Errorf("no default route: got %q", got)
	}
}

func TestInheritableMounts(t *testing.T) {
	all := []specs.Mount{
		{Type: "proc", Source: "proc", Destination: "/proc"},
		{Type: "bind", Source: "/var/lib/anvil/vol/data", Destination: "/data", Options: []string{"rbind"}},
		{Type: "bind", Source: "/Users/me/src", Destination: "/src", Options: []string{"rbind", "ro"}},
		{Type: "bind", Source: "/x/hosts", Destination: "/etc/hosts", Options: []string{"rbind", "ro"}},
		{Type: "bind", Source: "/x/resolv", Destination: "/etc/resolv.conf", Options: []string{"rbind", "ro"}},
		{Type: "bind", Source: "/x/hostname", Destination: "/etc/hostname", Options: []string{"rbind"}},
		{Type: "bind", Source: guestInitPath, Destination: containerInitPath, Options: []string{"rbind", "ro"}},
		{Type: "tmpfs", Source: "tmpfs", Destination: "/tmp"},
	}
	opts := func(ms []specs.Mount) map[string][]string {
		out := map[string][]string{}
		for _, m := range ms {
			out[m.Destination] = m.Options
		}
		return out
	}
	cases := []struct {
		mode string
		want map[string][]string
	}{
		{"", map[string][]string{"/data": {"rbind"}, "/src": {"rbind", "ro"}}},
		{"ro", map[string][]string{"/data": {"rbind", "ro"}, "/src": {"rbind", "ro"}}},
		{"rw", map[string][]string{"/data": {"rbind"}, "/src": {"rbind"}}},
	}
	for _, tc := range cases {
		if got := opts(inheritableMounts(all, tc.mode)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("mode %q: %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestMergeInheritedMountsOwnWins(t *testing.T) {
	own := []specs.Mount{{Type: "bind", Source: "/mine", Destination: "/data"}}
	inherited := []specs.Mount{
		{Type: "bind", Source: "/theirs", Destination: "/data/"},
		{Type: "bind", Source: "/logs", Destination: "/logs"},
		{Type: "bind", Source: "/logs2", Destination: "/logs"},
	}
	got := mergeInheritedMounts(own, inherited)
	if len(got) != 2 || got[0].Source != "/mine" || got[1].Source != "/logs" {
		t.Errorf("merged = %+v", got)
	}
}

func TestUserProcessArgs(t *testing.T) {
	if got := userProcessArgs([]string{containerInitPath, "--", "sleep", "1"}); !reflect.DeepEqual(got, []string{"sleep", "1"}) {
		t.Errorf("init prefix not stripped: %v", got)
	}
	if got := userProcessArgs([]string{"sh", "-c", "true"}); !reflect.DeepEqual(got, []string{"sh", "-c", "true"}) {
		t.Errorf("plain args changed: %v", got)
	}
}
