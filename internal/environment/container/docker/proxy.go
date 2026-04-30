package docker

import "strings"

// proxySetting holds a single proxy environment variable.
type proxySetting struct {
	Name  string
	Value string
}

// proxySettings aggregates proxy-related env vars for the Docker daemon.
type proxySettings []proxySetting

// IsEmpty reports whether no proxy variables are set.
func (p proxySettings) IsEmpty() bool { return len(p) == 0 }

// Lookup retrieves the value for the given proxy variable name (case-insensitive).
func (p proxySettings) Lookup(name string) string {
	for _, s := range p {
		if strings.EqualFold(s.Name, name) {
			return s.Value
		}
	}
	return ""
}

// EnvVars returns the settings as a list of "KEY=VALUE" strings.
func (p proxySettings) EnvVars() []string {
	out := make([]string, len(p))
	for i, s := range p {
		out[i] = s.Name + "=" + s.Value
	}
	return out
}

// knownProxyNames are the environment variable names we scan for.
var knownProxyNames = []string{
	"http_proxy",
	"https_proxy",
	"no_proxy",
	"HTTP_PROXY",
	"HTTPS_PROXY",
	"NO_PROXY",
}

// collectProxyEnv extracts proxy configuration from the given environment map.
func (d dockerEngine) collectProxyEnv(env map[string]string) proxySettings {
	var result proxySettings
	for _, name := range knownProxyNames {
		val, ok := env[name]
		if !ok || strings.TrimSpace(val) == "" {
			continue
		}
		result = append(result, proxySetting{Name: name, Value: val})
	}
	return result
}
