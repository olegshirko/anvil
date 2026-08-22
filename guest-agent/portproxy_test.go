package main

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"testing"
)

// readPortProxyHeader must consume a 4-byte big-endian length + JSON body and
// reject garbage without blocking past its deadline.
func TestReadPortProxyHeader(t *testing.T) {
	body, _ := json.Marshal(portProxyHeader{ContainerIP: "10.10.1.2", ContainerPort: 80})
	frame := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	frame = append(frame, body...)

	client, srv := net.Pipe()
	defer client.Close()
	defer srv.Close()
	go func() { _, _ = client.Write(frame) }()

	h, err := readPortProxyHeader(srv)
	if err != nil {
		t.Fatalf("readPortProxyHeader: %v", err)
	}
	if h.ContainerIP != "10.10.1.2" || h.ContainerPort != 80 {
		t.Fatalf("header = %+v", h)
	}
}

func TestReadPortProxyHeaderRejectsBadInput(t *testing.T) {
	cases := [][]byte{
		{0, 0, 0, 0},             // zero length
		{0xFF, 0xFF, 0xFF, 0xFF}, // oversize length
		{0, 0, 0, 5, 'x'},        // truncated body
	}
	for _, frame := range cases {
		client, srv := net.Pipe()
		go func(f []byte) { _, _ = client.Write(f); client.Close() }(frame)
		if _, err := readPortProxyHeader(srv); err == nil {
			t.Errorf("frame %v accepted, want error", frame)
		}
		srv.Close()
		client.Close()
	}
}

func TestPortProxyHeaderRejectsIncompleteTarget(t *testing.T) {
	client, srv := net.Pipe()
	defer client.Close()
	defer srv.Close()
	body := []byte(`{"container_ip":"","container_port":0}`)
	go func() {
		var head [4]byte
		binary.BigEndian.PutUint32(head[:], uint32(len(body)))
		_, _ = client.Write(append(head[:], body...))
	}()
	if _, err := readPortProxyHeader(srv); err == nil {
		t.Fatal("incomplete target accepted, want error")
	}
}

// Container ports persist in the per-container metadata; portBindingsFromMeta
// must render them back into the Docker PortBindings shape unchanged.
func TestPortBindingsFromMetaRoundtrip(t *testing.T) {
	dir := t.TempDir()
	orig := anvilStoreRoot
	anvilStoreRoot = dir
	defer func() { anvilStoreRoot = orig }()

	meta := &containerMeta{
		ID:        "abc123",
		Name:      "web",
		Namespace: "testns",
		Ports: []cniPortMapping{
			{HostPort: 8080, ContainerPort: 80, Protocol: "tcp", HostIP: "0.0.0.0"},
		},
	}
	if err := saveContainerMeta(meta); err != nil {
		t.Fatalf("save meta: %v", err)
	}

	got, err := loadContainerMeta("testns", "abc123")
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	if len(got.Ports) != 1 || got.Ports[0].HostPort != 8080 {
		t.Fatalf("roundtrip = %+v", got.Ports)
	}
	bindings := portBindingsFromMeta(got)
	tcp, ok := bindings["80/tcp"]
	if !ok || len(tcp) != 1 || tcp[0].HostPort != "8080" || tcp[0].HostIp != "0.0.0.0" {
		t.Fatalf("tcp binding = %+v", bindings)
	}
}

func TestPortBindingsFromMetaMulti(t *testing.T) {
	dir := t.TempDir()
	orig := anvilStoreRoot
	anvilStoreRoot = dir
	defer func() { anvilStoreRoot = orig }()

	meta := &containerMeta{
		ID:        "id1",
		Name:      "dns",
		Namespace: "tbs",
		Ports: []cniPortMapping{
			{HostPort: 18330, ContainerPort: 80, Protocol: "tcp"},
			{HostPort: 15353, ContainerPort: 53, Protocol: "udp"},
		},
	}
	if err := saveContainerMeta(meta); err != nil {
		t.Fatalf("save meta: %v", err)
	}
	got, err := loadContainerMeta("tbs", "id1")
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	bindings := portBindingsFromMeta(got)
	if len(bindings) != 2 {
		t.Fatalf("bindings = %+v", bindings)
	}
	tcp := bindings["80/tcp"]
	if len(tcp) != 1 || tcp[0].HostPort != "18330" || tcp[0].HostIp != "0.0.0.0" {
		t.Fatalf("tcp binding = %+v", tcp)
	}
	if _, ok := bindings["53/udp"]; !ok {
		t.Fatalf("udp binding missing: %+v", bindings)
	}
}
