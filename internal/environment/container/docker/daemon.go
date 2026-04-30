package docker

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
)

const (
	dockerDaemonPath = "/etc/docker/daemon.json"
	gatewayIPSetting = "host-gateway-ip"
	systemdOverride  = "/etc/systemd/system/docker.service.d/docker.conf"
)

// resolveGatewayIP returns the host-gateway IP from the guest's /etc/hosts,
// falling back to any user-specified value in the daemon config map.
func resolveGatewayIP(d dockerEngine, cfg map[string]any) (string, error) {
	ip, err := d.guest.RunOutput("sh", "-c", "grep 'host.lima.internal' /etc/hosts | awk '{print $1}'")
	if err != nil {
		return "", fmt.Errorf("cannot retrieve host gateway IP: %w", err)
	}
	if v, ok := cfg[gatewayIPSetting].(string); ok && v != "" {
		ip = v
	}
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("invalid host gateway IP: %q", ip)
	}
	return ip, nil
}

// rewriteProxyForGateway replaces loopback addresses in a proxy URL with the
// VM host-gateway IP so the proxy is reachable from inside the VM.
func rewriteProxyForGateway(proxyURL, gateway string) string {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return proxyURL
	}
	ips, err := net.LookupIP(u.Hostname())
	if err != nil {
		return proxyURL
	}
	for _, ip := range ips {
		if ip.IsLoopback() {
			host := gateway
			if u.Port() != "" {
				host = net.JoinHostPort(host, u.Port())
			}
			u.Host = host
			return u.String()
		}
	}
	return proxyURL
}

// writeDaemonConfig writes /etc/docker/daemon.json from the supplied map,
// injecting defaults for buildkit, cgroupdriver, and proxy settings.
// Returns true if the file was actually modified.
func (d dockerEngine) writeDaemonConfig(cfg map[string]any, env map[string]string) (bool, error) {
	if cfg == nil {
		cfg = map[string]any{}
	}

	if _, ok := cfg["features"]; !ok {
		cfg["features"] = map[string]any{
			"buildkit":               true,
			"containerd-snapshotter": true,
		}
	}

	if _, ok := cfg["exec-opts"]; !ok {
		cfg["exec-opts"] = []string{"native.cgroupdriver=cgroupfs"}
	} else if opts, ok := cfg["exec-opts"].([]string); ok {
		cfg["exec-opts"] = append(opts, "native.cgroupdriver=cgroupfs")
	}

	// remove user-supplied host-gateway-ip to avoid conflicting with systemd
	delete(cfg, gatewayIPSetting)

	if vars := d.collectProxyEnv(env); !vars.IsEmpty() {
		proxyCfg := map[string]any{}
		gw, err := resolveGatewayIP(d, cfg)
		if err != nil {
			return false, err
		}
		if http := vars.Lookup("http_proxy"); http != "" {
			proxyCfg["http-proxy"] = rewriteProxyForGateway(http, gw)
		}
		if https := vars.Lookup("https_proxy"); https != "" {
			proxyCfg["https-proxy"] = rewriteProxyForGateway(https, gw)
		}
		if no := vars.Lookup("no_proxy"); no != "" {
			proxyCfg["no-proxy"] = no
		}
		cfg["proxies"] = proxyCfg
	}

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, fmt.Errorf("cannot marshal daemon.json: %w", err)
	}

	existing, err := d.guest.Read(dockerDaemonPath)
	if err == nil && existing == string(b) {
		return false, nil
	}
	return true, d.guest.Write(dockerDaemonPath, b)
}

// writeSystemdOverride writes a systemd drop-in that sets --host-gateway-ip.
// Returns true if the file was actually modified.
func (d dockerEngine) writeSystemdOverride(cfg map[string]any) (bool, error) {
	ip, err := resolveGatewayIP(d, cfg)
	if err != nil {
		return false, err
	}
	content := fmt.Sprintf(systemdUnitTemplate, ip)

	existing, err := d.guest.Read(systemdOverride)
	if err == nil && existing == content {
		return false, nil
	}
	if err := d.guest.Write(systemdOverride, []byte(content)); err != nil {
		return false, fmt.Errorf("cannot write systemd override: %w", err)
	}
	return true, nil
}

// restartDockerService reloads systemd and restarts the Docker daemon.
func (d dockerEngine) restartDockerService() error {
	if err := d.guest.Run("sudo", "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("cannot reload systemd: %w", err)
	}
	if err := d.guest.Run("sudo", "systemctl", "restart", "docker"); err != nil {
		return fmt.Errorf("cannot restart docker: %w", err)
	}
	return nil
}

const systemdUnitTemplate = `
[Service]
LimitNOFILE=infinity
ExecStart=
ExecStart=/usr/bin/dockerd -H fd:// --containerd=/run/containerd/containerd.sock --host-gateway-ip=%s
`
