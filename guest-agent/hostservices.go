package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdlayher/vsock"
)

// Docker Desktop host services, served by vz-runner over vsock
// (HostServicesServer.swift):
//
//   - SSH agent forwarding: /run/host-services/ssh-auth.sock in the guest
//     relays to the Mac's ssh-agent. Containers bind-mount it, exactly as
//     with Docker Desktop:
//     -v /run/host-services/ssh-auth.sock:/run/host-services/ssh-auth.sock
//     -e SSH_AUTH_SOCK=/run/host-services/ssh-auth.sock
//
//   - The Mac's localhost: host.docker.internal resolves to a reserved
//     address (hostLoopbackIP) that nothing owns. iptables redirects TCP to
//     it into a local transparent proxy, which recovers the original port
//     (SO_ORIGINAL_DST) and asks the host to connect to 127.0.0.1:<port> —
//     so services bound only to the Mac's loopback are reachable, as in
//     Docker Desktop. UDP to it is DNAT'ed to the Mac's NAT address.

const (
	hostSSHAgentPort    = 1029
	hostLoopbackPort    = 1030
	sshAgentSocketPath  = "/run/host-services/ssh-auth.sock"
	hostLoopbackIP      = "192.168.65.254" // Docker Desktop's host.docker.internal
	hostLoopbackProxyPt = "3129"
)

// hostLoopbackReady is set once the redirect rules are installed; until
// then (or if they fail) host.docker.internal falls back to the Mac's NAT
// address, which only reaches services listening on all interfaces.
var hostLoopbackReady atomic.Bool

// Seam for tests.
var dialHostService = func(port uint32) (net.Conn, error) { return vsock.Dial(vsock.Host, port, nil) }

// desktopHostIP is what host.docker.internal and host-gateway resolve to.
func desktopHostIP() string {
	if hostLoopbackReady.Load() {
		return hostLoopbackIP
	}
	return hostGatewayIP()
}

// --- SSH agent ------------------------------------------------------------------

func serveSSHAgentForward() {
	if err := os.MkdirAll(filepath.Dir(sshAgentSocketPath), 0o755); err != nil {
		log.Printf("[host-services] ssh-agent: %v", err)
		return
	}
	os.Remove(sshAgentSocketPath)
	ln, err := net.Listen("unix", sshAgentSocketPath)
	if err != nil {
		log.Printf("[host-services] ssh-agent: listen %s: %v", sshAgentSocketPath, err)
		return
	}
	// Any container user may talk to the agent, as with Docker Desktop.
	os.Chmod(sshAgentSocketPath, 0o777) //nolint:errcheck
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[host-services] ssh-agent: accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func() {
			defer conn.Close()
			upstream, err := dialHostService(hostSSHAgentPort)
			if err != nil {
				log.Printf("[host-services] ssh-agent: vsock: %v", err)
				return
			}
			defer upstream.Close()
			relayConns(conn, upstream)
		}()
	}
}

// --- the Mac's localhost -----------------------------------------------------------

type hostLoopbackRequest struct {
	Port int `json:"port"`
}

// setupHostLoopback installs the redirect rules and starts the proxy.
// Called once the default route is known (the UDP rule needs the gateway).
func setupHostLoopback() {
	ln, err := net.Listen("tcp", ":"+hostLoopbackProxyPt)
	if err != nil {
		log.Printf("[host-services] host loopback: listen: %v", err)
		return
	}
	for _, rule := range hostLoopbackRules(hostGatewayIP()) {
		if err := ensureIptablesRule(rule); err != nil {
			log.Printf("[host-services] host loopback: %v (host.docker.internal falls back to the NAT address)", err)
			ln.Close()
			return
		}
	}
	hostLoopbackReady.Store(true)
	go serveHostLoopback(ln)
}

// hostLoopbackRules are the nat-table rules (without -A/-C) steering
// traffic for hostLoopbackIP: PREROUTING for bridged containers, OUTPUT for
// host-network containers and the guest itself.
func hostLoopbackRules(gateway string) [][]string {
	var rules [][]string
	for _, chain := range []string{"PREROUTING", "OUTPUT"} {
		rules = append(rules,
			[]string{"-t", "nat", chain, "-d", hostLoopbackIP + "/32", "-p", "tcp",
				"-j", "REDIRECT", "--to-ports", hostLoopbackProxyPt},
			[]string{"-t", "nat", chain, "-d", hostLoopbackIP + "/32", "-p", "udp",
				"-j", "DNAT", "--to-destination", gateway})
	}
	return rules
}

// ensureIptablesRule appends rule unless an identical one exists (a
// snapshot-resumed guest keeps its rules).
func ensureIptablesRule(rule []string) error {
	withOp := func(op string) []string {
		// rule = -t nat CHAIN ...: the operation goes before the chain.
		return append([]string{rule[0], rule[1], op}, rule[2:]...)
	}
	if exec.Command("iptables", withOp("-C")...).Run() == nil {
		return nil
	}
	if out, err := exec.Command("iptables", withOp("-A")...).CombinedOutput(); err != nil {
		return fmt.Errorf("iptables %s: %v: %s", strings.Join(withOp("-A"), " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func serveHostLoopback(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[host-services] host loopback: accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go handleHostLoopbackClient(conn)
	}
}

func handleHostLoopbackClient(conn net.Conn) {
	defer conn.Close()
	ip, port, err := originalDst(conn)
	if err != nil {
		log.Printf("[host-services] host loopback: original destination: %v", err)
		return
	}
	// Only redirected traffic: a direct connection to the proxy port has
	// no host-side meaning.
	if ip.String() != hostLoopbackIP {
		return
	}
	upstream, err := dialHostLoopback(port)
	if err != nil {
		debugLog("[host-services] host loopback :%d: %v", port, err)
		return
	}
	defer upstream.Close()
	relayConns(conn, upstream)
}

// dialHostLoopback asks vz-runner to connect to the Mac's localhost:port.
func dialHostLoopback(port int) (net.Conn, error) {
	conn, err := dialHostService(hostLoopbackPort)
	if err != nil {
		return nil, fmt.Errorf("vsock: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := writeFrame(conn, hostLoopbackRequest{Port: port}); err != nil {
		conn.Close()
		return nil, err
	}
	var reply hostEgressReply
	if err := readFrame(conn, &reply); err != nil {
		conn.Close()
		return nil, err
	}
	if reply.Error != "" {
		conn.Close()
		return nil, errors.New(reply.Error)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// relayConns copies both ways, half-closing each side when its source ends.
func relayConns(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(b, a); closeWrite(b) }()
	go func() { defer wg.Done(); _, _ = io.Copy(a, b); closeWrite(a) }()
	wg.Wait()
}
