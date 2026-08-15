package main

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
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

// writeContainerPortMappings persists the published ports into nerdctl's
// network store; readNetworkStorePorts (the scanner's fallback) must read
// them back unchanged.
func TestWriteContainerPortMappingsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	orig := nerdctlStoreRoot
	nerdctlStoreRoot = dir
	defer func() { nerdctlStoreRoot = orig }()

	// Seed the datastore layout glob expects: <root>/<addrHash>/containers.
	if err := os.MkdirAll(dir+"/1935db59/containers", 0o755); err != nil {
		t.Fatalf("seed datastore: %v", err)
	}

	mappings := []cniPortMapping{
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp", HostIP: "0.0.0.0"},
	}
	if err := writeContainerPortMappings("testns", "abc123", mappings); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := readNetworkStorePorts("testns", "abc123")
	if got == "" {
		t.Fatal("readNetworkStorePorts returned empty")
	}
	var parsed []cniPortMapping
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed) != 1 || parsed[0].HostPort != 8080 || parsed[0].ContainerPort != 80 {
		t.Fatalf("roundtrip = %+v", parsed)
	}

	// Rewriting must preserve foreign fields (read-modify-write).
	// The datastore glob sorts, so 1935db59 wins over ds; seed in place.
	raw, _ := json.Marshal(map[string]interface{}{
		"portMappings": parsed,
		"networks":     []string{"some"},
	})
	path := dir + "/1935db59/containers/testns/abc123/network-config.json"
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeContainerPortMappings("testns", "abc123", mappings); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var stored map[string]interface{}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("stored parse: %v", err)
	}
	if _, ok := stored["networks"]; !ok {
		t.Fatal("rewrite dropped the foreign 'networks' field")
	}
}

// portBindingsFromStore renders the persisted mappings back into the Docker
// HostConfig.PortBindings shape the CLI expects.
func TestPortBindingsFromStore(t *testing.T) {
	dir := t.TempDir()
	orig := nerdctlStoreRoot
	nerdctlStoreRoot = dir
	defer func() { nerdctlStoreRoot = orig }()
	if err := os.MkdirAll(dir+"/1935db59/containers", 0o755); err != nil {
		t.Fatalf("seed datastore: %v", err)
	}

	mappings := []cniPortMapping{
		{HostPort: 18330, ContainerPort: 80, Protocol: "tcp"},
		{HostPort: 15353, ContainerPort: 53, Protocol: "udp"},
	}
	if err := writeContainerPortMappings("tbs", "id1", mappings); err != nil {
		t.Fatalf("write: %v", err)
	}

	bindings := portBindingsFromStore("tbs", "id1")
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
