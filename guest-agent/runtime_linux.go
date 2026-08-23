//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	cniclient "github.com/containerd/go-cni"
	"github.com/containerd/log"
	unix "golang.org/x/sys/unix"
)

// Native runtime plumbing: CNI attachment and named network namespaces —
// the low-level pieces of the container lifecycle that nerdctl used to own.

// --- named network namespaces -------------------------------------------

// createNamedNetNS creates a persistent named network namespace at
// /var/run/netns/<name> and returns its path. The bind mount survives task
// death so stop/start cycles reuse one netns and CNI plugins can run before
// any process exists inside.
func createNamedNetNS(name string) (string, error) {
	if err := os.MkdirAll(netnsDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(netnsDir, name)
	os.Remove(path)
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	f.Close()

	// The unshare/mount/setns sequence must run on one locked OS thread:
	// unshare switches only the calling thread's netns; we bind-mount the
	// new ns onto the named file while inside it, then switch back.
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		tid := unix.Gettid()
		selfPath := fmt.Sprintf("/proc/self/task/%d/ns/net", tid)
		orig, err := os.Open(selfPath)
		if err != nil {
			errCh <- err
			return
		}
		defer orig.Close()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			errCh <- fmt.Errorf("unshare netns: %w", err)
			return
		}
		newNS, err := os.Open(selfPath)
		if err != nil {
			unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET) //nolint:errcheck
			errCh <- fmt.Errorf("open new netns: %w", err)
			return
		}
		defer newNS.Close()
		srcRef := fmt.Sprintf("/proc/self/fd/%d", newNS.Fd())
		mountErr := unix.Mount(srcRef, path, "none", unix.MS_BIND, "")
		if setnsErr := unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET); setnsErr != nil && mountErr == nil {
			errCh <- fmt.Errorf("restore netns: %w", setnsErr)
			return
		}
		errCh <- mountErr
	}()
	if err := <-errCh; err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// releaseNamedNetNS unmounts and deletes /var/run/netns/<name>.
func releaseNamedNetNS(name string) {
	path := filepath.Join(netnsDir, name)
	_ = unix.Unmount(path, unix.MNT_DETACH)
	os.Remove(path)
}

// --- CNI attachment -------------------------------------------------------

const cniBinDir = "/opt/cni/bin"

// cniManager caches one go-cni instance per conflist file. Each anvil network
// is exactly one conflist in /etc/cni/net.d; attaching a container means
// Setup against that single list with port-mapping capabilities.
type cniManager struct {
	mu       sync.Mutex
	byFile   map[string]cniclient.CNI
	confDir  string
}

var cnim = &cniManager{
	byFile:  make(map[string]cniclient.CNI),
	confDir: cniConfDir,
}

// invalidate drops cached instances (called after conflist writes/removals).
func (m *cniManager) invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byFile = make(map[string]cniclient.CNI)
}

func (m *cniManager) forConflist(path string) (cniclient.CNI, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.byFile[path]; ok {
		return c, nil
	}
	// WithConfListFile parses the conflist and registers the network right
	// away. Do NOT call Load afterwards: it resets the network list.
	c, err := cniclient.New(
		cniclient.WithPluginDir([]string{cniBinDir}),
		cniclient.WithInterfacePrefix("eth"),
		cniclient.WithConfListFile(path),
	)
	if err != nil {
		return nil, fmt.Errorf("cni init %s: %w", path, err)
	}
	m.byFile[path] = c
	return c, nil
}

// findConflistForNetwork locates the conflist file for a logical network
// name. Files are written by generateCNIConfigWithLabels with name == netName.
func findConflistForNetwork(netName string) (string, error) {
	entries, err := os.ReadDir(cniConfDir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if ext != ".conflist" && ext != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(cniConfDir, e.Name()))
		if err != nil {
			continue
		}
		var head struct {
			Name string `json:"name"`
		}
		if jsonUnmarshal(data, &head) == nil && head.Name == netName {
			return filepath.Join(cniConfDir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("no CNI config for network %q", netName)
}

// attachNetwork attaches a container's netns to the given logical network
// with port mappings and returns the assigned IPv4 address.
func attachNetwork(ctx context.Context, netName, ns, id, netnsPath string, ports []cniPortMapping) (string, string, error) {
	conflist, err := findConflistForNetwork(netName)
	if err != nil {
		return "", "", err
	}
	c, err := cnim.forConflist(conflist)
	if err != nil {
		return "", "", err
	}
	var opts []cniclient.NamespaceOpts
	if len(ports) > 0 {
		pms := make([]cniclient.PortMapping, 0, len(ports))
		for _, p := range ports {
			proto := strings.ToUpper(p.Protocol)
			if proto == "" {
				proto = "TCP"
			}
			pms = append(pms, cniclient.PortMapping{
				HostPort:      int32(p.HostPort),
				ContainerPort: int32(p.ContainerPort),
				Protocol:      proto,
				HostIP:        p.HostIP,
			})
		}
		opts = append(opts, cniclient.WithCapabilityPortMap(pms))
	}
	res, err := c.Setup(ctx, id, netnsPath, opts...)
	if err != nil {
		return "", "", fmt.Errorf("cni setup %s: %w", netName, err)
	}
	ip, mac := resultAddresses(res)
	log.G(ctx).WithField("network", netName).Debugf("[cni] %s attached ip=%s", id[:12], ip)
	return ip, mac, nil
}

// detachNetwork tears down a container endpoint on a logical network.
func detachNetwork(ctx context.Context, netName, ns, id, netnsPath string, ports []cniPortMapping) error {
	conflist, err := findConflistForNetwork(netName)
	if err != nil {
		return err
	}
	c, err := cnim.forConflist(conflist)
	if err != nil {
		return err
	}
	var opts []cniclient.NamespaceOpts
	if len(ports) > 0 {
		pms := make([]cniclient.PortMapping, 0, len(ports))
		for _, p := range ports {
			proto := strings.ToUpper(p.Protocol)
			if proto == "" {
				proto = "TCP"
			}
			pms = append(pms, cniclient.PortMapping{
				HostPort:      int32(p.HostPort),
				ContainerPort: int32(p.ContainerPort),
				Protocol:      proto,
				HostIP:        p.HostIP,
			})
		}
		opts = append(opts, cniclient.WithCapabilityPortMap(pms))
	}
	return c.Remove(ctx, id, netnsPath, opts...)
}

// resultAddresses extracts the primary IPv4 address and MAC from a CNI
// result. The address arrives as a plain net.IP.
func resultAddresses(res *cniclient.Result) (ip, mac string) {
	if res == nil {
		return "", ""
	}
	for _, iface := range res.Interfaces {
		if iface == nil {
			continue
		}
		if mac == "" {
			mac = iface.Mac
		}
		for _, cfg := range iface.IPConfigs {
			if cfg.IP != nil && cfg.IP.To4() != nil {
				return cfg.IP.String(), mac
			}
			if ip == "" && cfg.IP != nil {
				ip = cfg.IP.String()
			}
		}
	}
	return ip, mac
}

// jsonUnmarshal is a thin alias keeping runtime.go free of a second direct
// encoding/json import at call sites above.
func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
