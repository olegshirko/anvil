//go:build linux

package main

import (
	"errors"
	"net"
	"syscall"
)

// soOriginalDst is SO_ORIGINAL_DST from linux/netfilter_ipv4.h.
const soOriginalDst = 80

// originalDst returns the pre-REDIRECT destination of a connection.
func originalDst(c net.Conn) (net.IP, int, error) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return nil, 0, errors.New("not a TCP connection")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return nil, 0, err
	}
	var ip net.IP
	var port int
	var serr error
	err = raw.Control(func(fd uintptr) {
		// The kernel fills a sockaddr_in; IPv6Mreq is the usual 16-byte
		// carrier for it in Go.
		mreq, gerr := syscall.GetsockoptIPv6Mreq(int(fd), syscall.IPPROTO_IP, soOriginalDst)
		if gerr != nil {
			serr = gerr
			return
		}
		a := mreq.Multiaddr
		port = int(a[2])<<8 | int(a[3])
		ip = net.IPv4(a[4], a[5], a[6], a[7])
	})
	if err != nil {
		return nil, 0, err
	}
	return ip, port, serr
}
