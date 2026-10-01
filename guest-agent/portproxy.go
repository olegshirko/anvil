package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

// Anvil port publishing deliberately does not bind published ports inside
// the guest: create-time reservation would race
// availability at CREATE time (docker semantics are: check at start). With
// live containers, which `docker compose up --force-recreate` relies on:
// creates the replacement before stopping the old one — always failed with
// "port is already allocated". Instead, the host-side PortForwarder connects
// to this single guest-side proxy port and sends the real target
// (containerIP:containerPort) as a length-prefixed JSON header; the proxy
// dials it from inside the guest, where CNI addresses are reachable.
// Nothing ever binds the user's host ports inside the guest.
const portProxyAddr = "0.0.0.0:39131"

type portProxyHeader struct {
	ContainerIP   string `json:"container_ip"`
	ContainerPort int    `json:"container_port"`
}

// servePortProxy accepts host-forwarder connections on portProxyAddr and
// splices them to the requested container endpoint. One goroutine per
// connection; failures tear down that connection only.
func servePortProxy() {
	ln, err := net.Listen("tcp", portProxyAddr)
	if err != nil {
		log.Printf("[port-proxy] listen %s: %v (host port forwarding unavailable)", portProxyAddr, err)
		return
	}
	defer ln.Close() // a restart after a panic binds again
	log.Printf("[port-proxy] listening on %s", portProxyAddr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[port-proxy] accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		goSafe("port-proxy-conn", func() { handlePortProxyClient(conn) })
	}
}

func handlePortProxyClient(conn net.Conn) {
	defer conn.Close()
	// Only the Mac's forwarder, a peer on eth0's subnet. A container
	// (which may dial the eth0 address too, through its bridge) could
	// otherwise reach any address the VM can: unpublished ports of other
	// projects, the agent's loopback services, out of an --internal
	// network.
	if !peerOnInterfaceSubnet(conn, "eth0") {
		log.Printf("[port-proxy] refused %s (not from the host)", conn.RemoteAddr())
		return
	}
	target, err := readPortProxyHeader(conn)
	if err != nil {
		log.Printf("[port-proxy] header: %v", err)
		return
	}
	upstream, err := net.DialTimeout("tcp",
		net.JoinHostPort(target.ContainerIP, strconv.Itoa(target.ContainerPort)), 5*time.Second)
	if err != nil {
		log.Printf("[port-proxy] dial %s:%d: %v", target.ContainerIP, target.ContainerPort, err)
		return
	}
	defer upstream.Close()

	spliceHalfClose(conn, upstream)
}

// spliceHalfClose copies both directions and forwards a half-close: when one
// side finishes sending, the other gets FIN while the reverse direction keeps
// flowing. Without it the host relay's shutdown(SHUT_WR) never reached the
// container, so a client that closed its end left the server side (redis,
// DB pools, keep-alive HTTP) and both relays open forever.
func spliceHalfClose(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(b, a); closeWrite(b) }()
	go func() { defer wg.Done(); _, _ = io.Copy(a, b); closeWrite(a) }()
	wg.Wait()
}

func readPortProxyHeader(conn net.Conn) (*portProxyHeader, error) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("read length: %w", err)
	}
	length := binary.BigEndian.Uint32(lenBuf[:])
	if length == 0 || length > 1024 {
		return nil, fmt.Errorf("bad header length %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	var h portProxyHeader
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	if h.ContainerIP == "" || h.ContainerPort <= 0 {
		return nil, fmt.Errorf("incomplete target %+v", h)
	}
	return &h, nil
}

// peerOnInterfaceSubnet reports whether conn's peer is another host on the
// named interface's subnet (container addresses never are: they live on
// the CNI bridges, and local delivery is not masqueraded).
func peerOnInterfaceSubnet(conn net.Conn, name string) bool {
	peer, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return false
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return false
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.Contains(peer.IP) && !ipn.IP.Equal(peer.IP) {
			return true
		}
	}
	return false
}
