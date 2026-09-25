package main

import (
	"io"
	"net"
	"strings"
	"testing"
)

func TestHostLoopbackRules(t *testing.T) {
	rules := hostLoopbackRules("192.168.64.1")
	joined := make([]string, len(rules))
	for i, r := range rules {
		joined[i] = strings.Join(r, " ")
	}
	want := []string{
		"-t nat PREROUTING -d 192.168.65.254/32 -p tcp -j REDIRECT --to-ports 3129",
		"-t nat PREROUTING -d 192.168.65.254/32 -p udp -j DNAT --to-destination 192.168.64.1",
		"-t nat OUTPUT -d 192.168.65.254/32 -p tcp -j REDIRECT --to-ports 3129",
		"-t nat OUTPUT -d 192.168.65.254/32 -p udp -j DNAT --to-destination 192.168.64.1",
	}
	if strings.Join(joined, "\n") != strings.Join(want, "\n") {
		t.Errorf("rules:\n%s", strings.Join(joined, "\n"))
	}
}

// fakeHostService answers one host-loopback handshake, then echoes.
func fakeHostService(t *testing.T, replyErr string) {
	t.Helper()
	orig := dialHostService
	t.Cleanup(func() { dialHostService = orig })
	dialHostService = func(port uint32) (net.Conn, error) {
		if port != hostLoopbackPort {
			t.Errorf("dialed vsock port %d", port)
		}
		guest, host := net.Pipe()
		go func() {
			defer host.Close()
			var req hostLoopbackRequest
			if err := readFrame(host, &req); err != nil || req.Port != 5432 {
				t.Errorf("request %+v, %v", req, err)
				return
			}
			if err := writeFrame(host, hostEgressReply{Error: replyErr}); err != nil || replyErr != "" {
				return
			}
			io.Copy(host, host) //nolint:errcheck
		}()
		return guest, nil
	}
}

func TestDialHostLoopback(t *testing.T) {
	fakeHostService(t, "")
	conn, err := dialHostLoopback(5432)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go conn.Write([]byte("ping")) //nolint:errcheck
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Errorf("relay: %q %v", buf, err)
	}

	fakeHostService(t, "connect localhost:5432: Connection refused")
	if _, err := dialHostLoopback(5432); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("host error not surfaced: %v", err)
	}
}

func TestDesktopHostIPFallsBack(t *testing.T) {
	hostLoopbackReady.Store(false)
	if desktopHostIP() == hostLoopbackIP {
		t.Error("redirect address used before the rules are installed")
	}
	hostLoopbackReady.Store(true)
	t.Cleanup(func() { hostLoopbackReady.Store(false) })
	if desktopHostIP() != hostLoopbackIP {
		t.Error("redirect address not used once ready")
	}
}
