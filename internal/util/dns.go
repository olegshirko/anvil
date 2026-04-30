package util

import (
	"net"
	"os/exec"
	"runtime"
	"strings"

	"github.com/sirupsen/logrus"
)

// HostDNSResolvers returns the DNS resolver IP addresses configured on the
// host machine. On macOS it parses scutil --dns; on Linux it reads
// /etc/resolv.conf. Loopback addresses are filtered out because they would
// resolve inside the VM itself, not on the host.
func HostDNSResolvers() []net.IP {
	var ips []net.IP
	switch runtime.GOOS {
	case "darwin":
		ips = macOSDNSResolvers()
	default:
		ips = linuxDNSResolvers()
	}

	var filtered []net.IP
	for _, ip := range ips {
		if ip.IsLoopback() {
			continue
		}
		filtered = append(filtered, ip)
	}
	return filtered
}

func macOSDNSResolvers() []net.IP {
	out, err := exec.Command("scutil", "--dns").Output()
	if err != nil {
		logrus.Warnf("failed to run scutil --dns: %v", err)
		return nil
	}

	seen := make(map[string]struct{})
	var res []net.IP
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "nameserver") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		ipStr := strings.TrimSpace(parts[1])
		if _, ok := seen[ipStr]; ok {
			continue
		}
		seen[ipStr] = struct{}{}
		if ip := net.ParseIP(ipStr); ip != nil {
			res = append(res, ip)
		}
	}
	return res
}

func linuxDNSResolvers() []net.IP {
	out, err := exec.Command("grep", "^nameserver", "/etc/resolv.conf").Output()
	if err != nil {
		logrus.Warnf("failed to read /etc/resolv.conf: %v", err)
		return nil
	}

	seen := make(map[string]struct{})
	var res []net.IP
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ipStr := fields[1]
		if _, ok := seen[ipStr]; ok {
			continue
		}
		seen[ipStr] = struct{}{}
		if ip := net.ParseIP(ipStr); ip != nil {
			res = append(res, ip)
		}
	}
	return res
}
