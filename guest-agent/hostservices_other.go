//go:build !linux

package main

import (
	"errors"
	"net"
)

func originalDst(net.Conn) (net.IP, int, error) {
	return nil, 0, errors.New("SO_ORIGINAL_DST is linux-only")
}
