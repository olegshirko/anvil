package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"time"

	"github.com/mdlayher/vsock"
)

type portCheckRequest struct {
	Ports []int `json:"ports"`
}

type portCheckResponse struct {
	Busy []int `json:"busy"`
}

// busyForeignHostPorts asks vz-runner (host side, vsock port 1027) which of
// the given TCP ports are already bound on the host by a foreign process
// (Docker Desktop, Lima, a local postgres...). Without this check the
// container starts but the port forwarder can only log the bind failure,
// leaving the container silently unreachable on localhost.
// Ports held by anvil's own port forwarder are not reported by the host;
// container-vs-container conflicts are checked separately. Any failure
// (older vz-runner without the listener, timeout) is treated as "nothing
// busy" so creation never breaks against older hosts.
func busyForeignHostPorts(ports []int) []int {
	if len(ports) == 0 {
		return nil
	}

	conn, err := vsock.Dial(vsock.Host, hostPortCheckPort, nil)
	if err != nil {
		log.Printf("[portcheck] host query unavailable: %v", err)
		return nil
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	body, err := json.Marshal(portCheckRequest{Ports: ports})
	if err != nil {
		return nil
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := conn.Write(append(hdr[:], body...)); err != nil {
		log.Printf("[portcheck] write: %v", err)
		return nil
	}
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		log.Printf("[portcheck] read header: %v", err)
		return nil
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > 1<<20 {
		return nil
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(conn, payload); err != nil {
		log.Printf("[portcheck] read body: %v", err)
		return nil
	}

	var resp portCheckResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil
	}
	return resp.Busy
}
