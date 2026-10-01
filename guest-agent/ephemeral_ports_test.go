package main

import "testing"

func TestPortMappingsFromCreateEphemeral(t *testing.T) {
	req := dockerCreateRequest{}
	req.HostConfig.PortBindings = map[string][]dockerHostPort{
		"80/tcp":   {{HostPort: ""}},
		"53/udp":   {{HostIp: "127.0.0.1", HostPort: "0"}},
		"9000/tcp": {{HostPort: "8000-8010"}},
		"5432/tcp": {{HostPort: "15432"}},
	}
	got := map[int]cniPortMapping{}
	for _, m := range portMappingsFromCreate(req) {
		got[m.ContainerPort] = m
	}
	if m := got[80]; !m.Ephemeral || m.HostPort != 0 || m.Protocol != "tcp" {
		t.Errorf("-p 80 = %+v, want ephemeral tcp", m)
	}
	if m := got[53]; !m.Ephemeral || m.Protocol != "udp" || m.HostIP != "127.0.0.1" {
		t.Errorf("-p 127.0.0.1::53/udp = %+v", m)
	}
	if m := got[9000]; !m.Ephemeral || m.RangeLo != 8000 || m.RangeHi != 8010 {
		t.Errorf("-p 8000-8010:9000 = %+v", m)
	}
	if m := got[5432]; m.Ephemeral || m.HostPort != 15432 {
		t.Errorf("-p 15432:5432 = %+v", m)
	}
}

func TestPublishAllMappings(t *testing.T) {
	req := dockerCreateRequest{ExposedPorts: map[string]struct{}{"8080/tcp": {}}}
	req.HostConfig.PortBindings = map[string][]dockerHostPort{"443/tcp": {{HostPort: "8443"}}}
	img := map[string]struct{}{"80/tcp": {}, "443/tcp": {}, "53/udp": {}}
	got := publishAllMappings(req, img)
	if len(got) != 3 {
		t.Fatalf("got %d mappings, want 3 (443 is bound explicitly): %+v", len(got), got)
	}
	for _, m := range got {
		if !m.Ephemeral || m.HostPort != 0 {
			t.Errorf("mapping %+v is not ephemeral", m)
		}
		if m.ContainerPort == 443 {
			t.Errorf("explicitly bound 443 published twice")
		}
	}
}

func TestPickHostPortSkipsUsed(t *testing.T) {
	// UDP skips the host query, so this runs without a VM.
	m := cniPortMapping{ContainerPort: 53, Protocol: "udp", Ephemeral: true, RangeLo: 7000, RangeHi: 7002}
	p, err := pickHostPort(m, map[int]bool{7000: true, 7001: true})
	if err != nil || p != 7002 {
		t.Fatalf("pick = %d, %v; want 7002", p, err)
	}
	if _, err := pickHostPort(m, map[int]bool{7000: true, 7001: true, 7002: true}); err == nil {
		t.Fatal("exhausted range picked a port")
	}
	m = cniPortMapping{ContainerPort: 53, Protocol: "udp", Ephemeral: true}
	p, err = pickHostPort(m, map[int]bool{})
	if err != nil || p < ephemeralPortLo || p > ephemeralPortHi {
		t.Fatalf("ephemeral pick = %d, %v", p, err)
	}
}

func TestRequestedVsAssignedPortBindings(t *testing.T) {
	meta := &containerMeta{Ports: []cniPortMapping{
		{ContainerPort: 80, Protocol: "tcp", Ephemeral: true},
		{ContainerPort: 9000, Protocol: "tcp", Ephemeral: true, RangeLo: 8000, RangeHi: 8010, HostPort: 8003},
	}}
	req := requestedPortBindings(meta)
	if b := req["80/tcp"]; len(b) != 1 || b[0].HostPort != "" {
		t.Errorf("requested 80/tcp = %+v, want HostPort \"\"", b)
	}
	if b := req["9000/tcp"]; len(b) != 1 || b[0].HostPort != "8000-8010" {
		t.Errorf("requested 9000/tcp = %+v, want the range", b)
	}
	got := portBindingsFromMeta(meta)
	if b, ok := got["80/tcp"]; !ok || b != nil {
		t.Errorf("unassigned 80/tcp = %+v (present=%v), want exposed with no binding", b, ok)
	}
	if b := got["9000/tcp"]; len(b) != 1 || b[0].HostPort != "8003" {
		t.Errorf("assigned 9000/tcp = %+v, want 8003", b)
	}
}

func TestDockerSocketBindSource(t *testing.T) {
	for _, src := range []string{"/var/run/docker.sock", "/run/docker.sock", "/var/run/docker.sock.raw",
		"/Users/me/.anvil-vz/docker.sock", "/var/run//docker.sock"} {
		if got, ok := dockerSocketBindSource(src); !ok || got != guestDockerSocket {
			t.Errorf("%s -> %s, %v; want %s", src, got, ok, guestDockerSocket)
		}
	}
	for _, src := range []string{"/Users/me/project", "/var/run/other.sock", "/Users/me/docker.sock"} {
		if got, ok := dockerSocketBindSource(src); ok || got != src {
			t.Errorf("%s rewritten to %s", src, got)
		}
	}
}
