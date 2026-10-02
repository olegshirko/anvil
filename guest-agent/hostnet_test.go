package main

import "testing"

func TestParseProcNetAddr(t *testing.T) {
	cases := []struct {
		in   string
		ip   string
		port int
	}{
		{"0100007F:1F90", "127.0.0.1", 8080},
		{"00000000:0050", "0.0.0.0", 80},
		{"00000000000000000000000000000000:1F90", "::", 8080},
		{"00000000000000000000000001000000:0016", "::1", 22},
	}
	for _, c := range cases {
		ip, port, ok := parseProcNetAddr(c.in)
		if !ok || ip.String() != c.ip || port != c.port {
			t.Errorf("%s: %v %d %v, want %s %d", c.in, ip, port, ok, c.ip, c.port)
		}
	}
}
