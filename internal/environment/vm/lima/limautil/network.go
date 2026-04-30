package limautil

import (
	"bytes"
	"fmt"
	"net"
	"strings"
)

const (
	// DefaultInterfaceName is the Lima network interface for shared mode.
	DefaultInterfaceName = "col0"
	// DefaultRouteMetric is the route metric for the shared interface.
	DefaultRouteMetric uint32 = 300
	// PreferredRouteMetric is used when the VM IP is the preferred route.
	PreferredRouteMetric uint32 = 100
)

// NetworkInspector queries network state from a Lima instance.
type NetworkInspector struct {
	profileID string
}

// NewNetworkInspector creates an inspector for the given profile.
func NewNetworkInspector(profileID string) *NetworkInspector {
	return &NetworkInspector{profileID: profileID}
}

// FindIP returns the VM's reachable IP address, falling back to 127.0.0.1.
func (n *NetworkInspector) FindIP() string {
	const fallback = "127.0.0.1"
	inst, err := Instance(n.profileID)
	if err != nil || len(inst.Network) == 0 {
		return fallback
	}
	for _, nw := range inst.Network {
		if nw.Interface == DefaultInterfaceName {
			if ip := n.extractIP(nw.Interface); ip != "" {
				return ip
			}
		}
	}
	return fallback
}

// InternalIP returns the IP address on the internal eth0 interface.
func (n *NetworkInspector) InternalIP() string {
	return n.extractIP("eth0")
}

func (n *NetworkInspector) extractIP(iface string) string {
	script := fmt.Sprintf("ip -4 addr show %s 2>/dev/null || true", iface)
	cmd := Limactl("shell", n.profileID, "sh", "-c", script)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return ""
	}
	return parseIPv4(buf.String())
}

// parseIPv4 extracts the first IPv4 address from ip addr output.
func parseIPv4(output string) string {
	for _, line := range strings.Split(output, "\n") {
		idx := strings.Index(line, "inet ")
		if idx == -1 {
			continue
		}
		fields := strings.Fields(line[idx+5:])
		if len(fields) == 0 {
			continue
		}
		ip := strings.Split(fields[0], "/")[0]
		if net.ParseIP(ip) != nil {
			return ip
		}
	}
	return ""
}

// LimaNetworkConfig holds static network configuration from limactl.
type LimaNetworkConfig struct {
	Mode    string `yaml:"mode"`
	Gateway net.IP `yaml:"gateway"`
	Netmask string `yaml:"netmask"`
}

// LimaNetwork wraps the network section of limactl inspect output.
type LimaNetwork struct {
	Networks struct {
		Shared LimaNetworkConfig `yaml:"user-v2"`
	} `yaml:"networks"`
}
