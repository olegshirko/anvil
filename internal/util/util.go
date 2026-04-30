package util

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/shlex"
	"github.com/sirupsen/logrus"
)

// UserHome returns the current user's home directory.
func UserHome() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot retrieve home directory: %w", err)
	}
	return h, nil
}

// FreeTCPPort binds to :0 and returns the assigned port.
func FreeTCPPort() (int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, fmt.Errorf("cannot acquire free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, fmt.Errorf("cannot release temporary listener: %w", err)
	}
	return port, nil
}

// LocalIPv4Addrs returns all non-loopback IPv4 addresses on the host.
func LocalIPv4Addrs() []net.IP {
	var addrs []net.IP
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, iface := range interfaces {
		parts := strings.Split(iface.String(), "/")
		ip := net.ParseIP(parts[0]).To4()
		if ip != nil && ip.String() != "127.0.0.1" {
			addrs = append(addrs, ip)
		}
	}
	return addrs
}

// SplitShell parses a shell command string into arguments.
func SplitShell(cmd string) []string {
	args, err := shlex.Split(cmd)
	if err != nil {
		logrus.Warnf("shlex split failed: %v", err)
		logrus.Warnln("falling back to whitespace split")
		args = strings.Fields(cmd)
	}
	return args
}

// ResolveMountPath expands environment variables and ~, then validates the path is absolute.
func ResolveMountPath(location string) (string, error) {
	if location == "" {
		return "", nil
	}

	s := os.ExpandEnv(location)
	if strings.HasPrefix(s, "~") {
		home, err := UserHome()
		if err != nil {
			return "", err
		}
		s = strings.Replace(s, "~", home, 1)
	}

	s = filepath.Clean(s)
	if !filepath.IsAbs(s) {
		return "", fmt.Errorf("relative mount path not allowed: %s", location)
	}
	return strings.TrimSuffix(s, "/") + "/", nil
}
