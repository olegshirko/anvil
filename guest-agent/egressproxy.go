package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/mdlayher/vsock"
)

// Egress through the host. The VM normally reaches the internet through the
// macOS NAT; when that path is dead (a full-tunnel VPN such as a Tailscale
// exit node swallows NATed traffic) the agent's own outbound connections —
// registry pulls, pushes, logins, the docker-mirror fallback — and
// buildkitd's registry traffic are carried over vsock to vz-runner, which
// dials from the Mac's network stack and therefore through the VPN.
//
// The decision is per connection: dial directly first; after a connect
// timeout or an unreachable network, mark the direct path broken for a
// while and go through the host. Container traffic (RUN steps, apps) is not
// covered — only registry-style traffic of the agent and buildkitd.

const (
	// hostEgressPort is vz-runner's vsock listener (EgressServer.swift).
	hostEgressPort = 1028
	// egressProxyAddr is the CONNECT proxy buildkitd is pointed at.
	egressProxyAddr = "127.0.0.1:3128"

	directDialTimeout = 4 * time.Second
	directBadFor      = 60 * time.Second
)

var directEgress = struct {
	sync.Mutex
	badUntil time.Time
	logged   bool
}{}

func directEgressKnownBad(now time.Time) bool {
	directEgress.Lock()
	defer directEgress.Unlock()
	return now.Before(directEgress.badUntil)
}

func markDirectEgressBad(now time.Time, cause error) {
	directEgress.Lock()
	defer directEgress.Unlock()
	directEgress.badUntil = now.Add(directBadFor)
	if !directEgress.logged {
		directEgress.logged = true
		log.Printf("[egress] direct internet access failed (%v); routing registry traffic through the host", cause)
	}
}

// Seams for tests.
var (
	dialDirect = (&net.Dialer{Timeout: directDialTimeout}).DialContext
	dialHost   = func() (net.Conn, error) { return vsock.Dial(vsock.Host, hostEgressPort, nil) }
)

// dialOut connects to a host:port on the internet, directly when that works
// and through the host otherwise.
func dialOut(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return dialDirect(ctx, network, addr)
	}
	if !directEgressKnownBad(time.Now()) {
		conn, err := dialDirect(ctx, network, addr)
		if err == nil || !isEgressFailure(err) {
			// Success, or a real answer (refused, no such host): the
			// network path itself works.
			return conn, err
		}
		if ctx.Err() != nil {
			return nil, err
		}
		markDirectEgressBad(time.Now(), err)
	}
	conn, err := dialViaHost(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s through the host: %w", addr, err)
	}
	return conn, nil
}

type hostEgressRequest struct {
	Target string `json:"target"`
}

type hostEgressReply struct {
	Error string `json:"error,omitempty"`
}

// dialViaHost asks vz-runner to connect to target; on success the returned
// connection carries the target's byte stream.
func dialViaHost(ctx context.Context, target string) (net.Conn, error) {
	conn, err := dialHost()
	if err != nil {
		return nil, fmt.Errorf("vsock: %w", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	if err := writeFrame(conn, hostEgressRequest{Target: target}); err != nil {
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

// writeFrame / readFrame: 4-byte big-endian length + JSON, the framing of
// every vsock side channel.
func writeFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(append(hdr[:], body...)); err != nil {
		return err
	}
	return nil
}

func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("read length: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > 4096 {
		return fmt.Errorf("bad frame length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	return json.Unmarshal(body, v)
}

// registryTransport is the transport of every registry client the agent
// uses: containerd's defaults, with outbound connections through dialOut.
func registryTransport() *http.Transport {
	t := docker.DefaultHTTPTransport(nil)
	t.Proxy = nil
	t.DialContext = dialOut
	return t
}

// routeDefaultTransportThroughEgress makes http.DefaultTransport (the
// mirror download, containerd paths that fall back to http.DefaultClient)
// use dialOut too.
func routeDefaultTransportThroughEgress() {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t.DialContext = dialOut
	}
}

// --- CONNECT proxy for buildkitd ------------------------------------------

// serveEgressProxy runs the HTTPS proxy buildkitd is configured with.
func serveEgressProxy() {
	ln, err := net.Listen("tcp", egressProxyAddr)
	if err != nil {
		log.Printf("[egress] listen %s: %v (buildkit registry traffic stays direct)", egressProxyAddr, err)
		return
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go handleEgressProxyClient(conn, dialOut)
	}
}

func handleEgressProxyClient(conn net.Conn, dial dialContextFunc) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		// Only HTTPS goes through the proxy (HTTPS_PROXY); plain HTTP
		// registries are not proxied.
		fmt.Fprint(conn, "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n")
		return
	}
	target := req.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The proxy is reachable from host-network containers and build steps,
	// not only buildkitd: only public destinations, dialed by the address
	// that was checked (no second lookup to rebind).
	checked, hostOnly, err := checkEgressProxyTarget(ctx, target)
	if err != nil {
		log.Printf("[egress] CONNECT %s refused: %v", target, err)
		fmt.Fprint(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		return
	}
	var upstream net.Conn
	if hostOnly {
		// No DNS in the VM (the full-tunnel VPN case this proxy exists
		// for): only the Mac can resolve it, and vz-runner refuses
		// loopback targets itself.
		upstream, err = dialViaHost(ctx, target)
	} else {
		upstream, err = dial(ctx, "tcp", checked)
	}
	if err != nil {
		log.Printf("[egress] CONNECT %s: %v", target, err)
		fmt.Fprint(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer upstream.Close()
	_ = conn.SetReadDeadline(time.Time{})
	if _, err := fmt.Fprint(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	// Bytes the client sent right behind the request (TLS ClientHello).
	if n := br.Buffered(); n > 0 {
		buf, _ := br.Peek(n)
		if _, err := upstream.Write(buf); err != nil {
			return
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(upstream, conn); closeWrite(upstream) }()
	go func() { defer wg.Done(); _, _ = io.Copy(conn, upstream); closeWrite(conn) }()
	wg.Wait()
}

// egressProxyLookup resolves CONNECT targets. Seam for tests.
var egressProxyLookup = net.DefaultResolver.LookupIPAddr

// checkEgressProxyTarget resolves host:port and returns ip:port when every
// address is a public one. buildkitd's NO_PROXY already keeps loopback and
// private destinations off the proxy; anything else asking for them is not
// registry traffic. Loopback, private (RFC 1918, fc00::/7 — the VM's NAT
// subnet and the guest's own services included), link-local, multicast and
// unspecified addresses are refused.
//
// When the VM cannot resolve the name at all, hostOnly is set: the target
// then goes to vz-runner by name, never dialed directly.
func checkEgressProxyTarget(ctx context.Context, target string) (addr string, hostOnly bool, err error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return "", false, err
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		addrs, err := egressProxyLookup(ctx, host)
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && !dnsErr.IsNotFound {
			return target, true, nil
		}
		if err != nil {
			return "", false, err
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	if len(ips) == 0 {
		return "", false, fmt.Errorf("no address for %s", host)
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() ||
			ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
			return "", false, fmt.Errorf("%s resolves to non-public address %s", host, ip)
		}
	}
	return net.JoinHostPort(ips[0].String(), port), false, nil
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// buildkitdProxyEnv points buildkitd's registry traffic at the proxy.
// Private and loopback destinations (local registries, containers) stay
// direct.
var buildkitdProxyEnv = []string{
	"HTTPS_PROXY=http://" + egressProxyAddr,
	"https_proxy=http://" + egressProxyAddr,
	"NO_PROXY=localhost,127.0.0.0/8,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16",
	"no_proxy=localhost,127.0.0.0/8,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16",
}
