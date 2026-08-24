package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Native container creation on top of the containerd client: image unpack,
// named netns, rootfs snapshot, OCI spec generation, hosts/resolv.conf
// preparation and label bookkeeping. Start/stop/delete live in
// container_ops.go. Together they replace everything nerdctl used to own.

// --- network helpers ------------------------------------------------------

// effectiveNetworkName maps the request's NetworkMode to the logical CNI
// network name (the conflist `name`). Default networking is the "bridge"
// network of the namespace.
func effectiveNetworkName(networkMode string) string {
	if networkMode == "" || networkMode == "default" || networkMode == "bridge" {
		return "bridge"
	}
	return networkMode
}

func usesHostNetwork(req dockerCreateRequest) bool {
	return req.HostConfig.NetworkMode == "host"
}

// --- signals ----------------------------------------------------------------

var signalNumbers = map[string]syscall.Signal{
	"HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6,
	"BUS": 7, "FPE": 8, "KILL": 9, "USR1": 10, "SEGV": 11, "USR2": 12,
	"PIPE": 13, "ALRM": 14, "TERM": 15, "CHLD": 17, "CONT": 18,
	"STOP": 19, "TSTP": 20, "TTIN": 21, "TTOU": 22, "URG": 23,
	"XCPU": 24, "XFSZ": 25, "VTALRM": 26, "PROF": 27, "WINCH": 28,
}

// signalValue resolves "SIGTERM"/"TERM"/"9"-style specs to a signal number.
func signalValue(name string) (syscall.Signal, bool) {
	if n, err := strconv.Atoi(strings.TrimSpace(name)); err == nil && n > 0 && n < 64 {
		return syscall.Signal(n), true
	}
	up := strings.ToUpper(strings.TrimSpace(name))
	up = strings.TrimPrefix(up, "SIG")
	sig, ok := signalNumbers[up]
	return sig, ok
}

// --- per-container root preparation ---------------------------------------

// prepareContainerRoot creates the metadata directory and the files that get
// bind-mounted into the container: hosts, resolv.conf, hostname.
func prepareContainerRoot(ns, id, hostname string, dns []string, extraHosts []string) error {
	dir := containerMetaDir(ns, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	hosts := "127.0.0.1\tlocalhost\n" +
		"::1\tlocalhost ip6-localhost ip6-loopback\n" +
		"fe00::0\tip6-localnet\n" +
		"ff00::0\tip6-mcastprefix\n" +
		"ff02::1\tip6-allnodes\n" +
		"ff02::2\tip6-allrouters\n" +
		"127.0.0.1\t" + hostname + "\n"
	for _, eh := range extraHosts {
		parts := strings.SplitN(eh, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		ip := parts[1]
		if ip == "host-gateway" {
			ip = detectGuestIP()
			if ip == "" {
				ip = "10.10.0.1"
			}
		}
		hosts += ip + "\t" + parts[0] + "\n"
	}
	if err := os.WriteFile(containerHostsPath(ns, id), []byte(hosts), 0o644); err != nil {
		return err
	}

	var resolv string
	if len(dns) > 0 {
		for _, s := range dns {
			resolv += "nameserver " + s + "\n"
		}
	} else if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		resolv = string(data)
	}
	if err := os.WriteFile(containerResolvPath(ns, id), []byte(resolv), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "hostname"), []byte(hostname+"\n"), 0o644)
}

// --- mounts ---------------------------------------------------------------

// computeContainerMounts translates Binds/Mounts/TmpFs into OCI mounts plus
// the standard /etc file bind-mounts. Named volumes are created on demand;
// anonymous volumes are tracked for removal on delete.
func computeContainerMounts(ns, id string, req dockerCreateRequest) ([]specs.Mount, []string, error) {
	var mounts []specs.Mount
	var anonVols []string

	addBind := func(src, dst string, ro bool) {
		opts := []string{"rbind"}
		if ro {
			opts = append(opts, "ro")
		}
		mounts = append(mounts, specs.Mount{Type: "bind", Source: src, Destination: dst, Options: opts})
	}
	addNamedVolume := func(volName, dst string, ro bool) error {
		dir := volumeDataDir(ns, volName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		addBind(dir, dst, ro)
		return nil
	}
	newAnonVolName := func() string {
		b := make([]byte, 16)
		rand.Read(b) //nolint:errcheck — crypto/rand never fails in practice
		return hex.EncodeToString(b)
	}
	addHostOrVolume := func(src, dst string, ro bool) error {
		switch {
		case src == "":
			name := newAnonVolName()
			if err := addNamedVolume(name, dst, ro); err != nil {
				return err
			}
			anonVols = append(anonVols, name)
		case strings.HasPrefix(src, "/"):
			os.MkdirAll(src, 0o755) //nolint:errcheck — docker creates missing host dirs
			addBind(src, dst, ro)
		default:
			if err := addNamedVolume(src, dst, ro); err != nil {
				return err
			}
		}
		return nil
	}

	// Docker -v grammar: "dst", "src:dst", or "src:dst:mode".
	parseBindSpec := func(spec string) error {
		parts := strings.SplitN(spec, ":", 3)
		switch len(parts) {
		case 1:
			return addHostOrVolume("", parts[0], false)
		case 2:
			return addHostOrVolume(parts[0], parts[1], false)
		case 3:
			return addHostOrVolume(parts[0], parts[1], parts[2] == "ro")
		}
		return fmt.Errorf("invalid mount spec")
	}

	for _, b := range req.HostConfig.Binds {
		if err := parseBindSpec(b); err != nil {
			return nil, nil, fmt.Errorf("bind %q: %w", b, err)
		}
	}
	for _, m := range req.HostConfig.Mounts {
		if m.Type == "tmpfs" || m.Source == "" || m.Target == "" {
			continue
		}
		if err := addHostOrVolume(m.Source, m.Target, m.ReadOnly); err != nil {
			return nil, nil, fmt.Errorf("mount %q: %w", m.Target, err)
		}
	}
	for path, optsStr := range req.HostConfig.TmpFs {
		o := []string{"noexec", "nosuid", "nodev"}
		for _, kv := range strings.Split(optsStr, ",") {
			kv = strings.TrimSpace(kv)
			if kv == "" || kv == "rw" {
				continue
			}
			o = append(o, kv)
		}
		mounts = append(mounts, specs.Mount{
			Type:        "tmpfs",
			Source:      "tmpfs",
			Destination: path,
			Options:     o,
		})
	}

	// Standard /etc files as bind mounts (like nerdctl/docker).
	mounts = append(mounts,
		specs.Mount{Type: "bind", Source: containerHostsPath(ns, id), Destination: "/etc/hosts",
			Options: []string{"rbind", "ro"}},
		specs.Mount{Type: "bind", Source: containerResolvPath(ns, id), Destination: "/etc/resolv.conf",
			Options: []string{"rbind", "ro"}},
		specs.Mount{Type: "bind", Source: filepath.Join(containerMetaDir(ns, id), "hostname"),
			Destination: "/etc/hostname", Options: []string{"rbind"}},
	)
	return mounts, anonVols, nil
}

// --- OCI spec construction -------------------------------------------------

// ocispecImageConfig mirrors the image config fields the spec builder needs.
type ocispecImageConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	User       string
	WorkingDir string
}

func imageEnv(c *ocispecImageConfig) []string {
	if c == nil {
		return nil
	}
	return c.Env
}

// mergeEnv merges env lists; later values win at their first position.
func mergeEnv(lists ...[]string) []string {
	seen := map[string]int{}
	var out []string
	for _, l := range lists {
		for _, kv := range l {
			key := kv
			if i := strings.IndexByte(kv, '='); i >= 0 {
				key = kv[:i]
			}
			if idx, ok := seen[key]; ok {
				out[idx] = kv
			} else {
				seen[key] = len(out)
				out = append(out, kv)
			}
		}
	}
	return out
}

// buildSpecOpts composes the OCI spec options for a create request.
func buildSpecOpts(id, hostname string, imgCfg *ocispecImageConfig, req dockerCreateRequest, mounts []specs.Mount, hostNet bool) ([]oci.SpecOpts, error) {
	entrypoint := req.Entrypoint
	if len(entrypoint) == 0 && imgCfg != nil {
		entrypoint = imgCfg.Entrypoint
	}
	cmdArgs := req.Cmd
	if len(cmdArgs) == 0 && imgCfg != nil {
		cmdArgs = imgCfg.Cmd
	}
	argv := append(append([]string{}, entrypoint...), cmdArgs...)
	if len(argv) == 0 {
		return nil, fmt.Errorf("no command specified and image has no CMD")
	}

	env := mergeEnv(imageEnv(imgCfg), req.Env)

	user := req.User
	if user == "" && imgCfg != nil {
		user = imgCfg.User
	}
	cwd := req.WorkingDir
	if cwd == "" && imgCfg != nil {
		cwd = imgCfg.WorkingDir
	}
	if cwd == "" {
		cwd = "/"
	}

	opts := []oci.SpecOpts{
		oci.WithProcessArgs(argv...),
		// containerd's default spec mounts an empty tmpfs over /run, which
		// hides image content placed there (/var/run -> /run), breaking
		// images like postgres that ship /var/run/postgresql. Docker does
		// not mask /run; user --tmpfs /run mounts are added later and are
		// not affected by this removal.
		oci.WithoutRunMount,
		// Private cgroup namespace (docker default on cgroup v2): runc then
		// mounts the container's own cgroup subtree at /sys/fs/cgroup, so
		// limits like memory.max are visible inside.
		func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
			s.Linux.Namespaces = append(s.Linux.Namespaces, specs.LinuxNamespace{Type: specs.CgroupNamespace})
			// runc swaps this for the container's own cgroup2 subtree when
			// the cgroup namespace is private.
			s.Mounts = append(s.Mounts, specs.Mount{
				Destination: "/sys/fs/cgroup",
				Type:        "cgroup",
				Source:      "cgroup",
				Options:     []string{"ro", "nosuid", "nodev", "noexec", "relatime"},
			})
			return nil
		},
		oci.WithEnv(env),
		oci.WithProcessCwd(cwd),
		oci.WithHostname(hostname),
	}
	if user != "" {
		opts = append(opts, oci.WithUser(user))
	}
	if req.Tty {
		opts = append(opts, oci.WithTTY)
	}
	if req.HostConfig.ReadonlyRootfs {
		opts = append(opts, oci.WithRootFSReadonly())
	}
	if req.HostConfig.Privileged {
		opts = append(opts, oci.WithPrivileged)
	} else {
		if len(req.HostConfig.CapAdd) > 0 {
			opts = append(opts, oci.WithAddedCapabilities(req.HostConfig.CapAdd))
		}
		if len(req.HostConfig.CapDrop) > 0 {
			opts = append(opts, oci.WithDroppedCapabilities(req.HostConfig.CapDrop))
		}
	}
	if req.HostConfig.Memory > 0 {
		opts = append(opts, oci.WithMemoryLimit(uint64(req.HostConfig.Memory)))
	}
	if req.HostConfig.NanoCpus > 0 {
		cpus := float64(req.HostConfig.NanoCpus) / 1e9
		opts = append(opts, oci.WithCPUs(strconv.FormatFloat(cpus, 'f', -1, 64)))
	}
	if len(req.HostConfig.Sysctls) > 0 {
		sysctls := req.HostConfig.Sysctls
		opts = append(opts, func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
			if s.Linux == nil {
				s.Linux = &specs.Linux{}
			}
			if s.Linux.Sysctl == nil {
				s.Linux.Sysctl = map[string]string{}
			}
			for k, v := range sysctls {
				s.Linux.Sysctl[k] = v
			}
			return nil
		})
	}
	if mode := req.HostConfig.PidMode; mode == "host" {
		opts = append(opts, oci.WithHostNamespace(specs.PIDNamespace))
	}
	for _, d := range req.HostConfig.Devices {
		if d.PathOnHost == "" {
			continue
		}
		opts = append(opts, oci.WithLinuxDeviceFollowSymlinks(d.PathOnHost, "rwm"))
	}
	if len(mounts) > 0 {
		opts = append(opts, oci.WithMounts(mounts))
	}
	if hostNet {
		// Drop the default (fresh, empty) network namespace so the task
		// shares the guest's own netns — that is what host networking is.
		opts = append(opts, func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
			kept := s.Linux.Namespaces[:0]
			for _, n := range s.Linux.Namespaces {
				if n.Type != specs.NetworkNamespace {
					kept = append(kept, n)
				}
			}
			s.Linux.Namespaces = kept
			return nil
		})
	} else {
		opts = append(opts, oci.WithLinuxNamespace(specs.LinuxNamespace{
			Type: specs.NetworkNamespace,
			Path: netnsPathFor(id),
		}))
	}
	return opts, nil
}

// --- create ----------------------------------------------------------------

// createNativeContainer registers the container with containerd and prepares
// all start-time state. It returns the containerd ID.
func createNativeContainer(ctx context.Context, ns, name string, req dockerCreateRequest) (string, error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return "", fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)

	id := newContainerID()
	imgRef := canonicalizeImageRef(req.Image)
	img, err := cl.GetImage(nsCtx, imgRef)
	if err != nil {
		return "", fmt.Errorf("image %s not found in namespace %s: %w", imgRef, ns, err)
	}

	// The rootfs snapshot requires unpacked layers.
	if uerr := img.Unpack(nsCtx, ""); uerr != nil {
		return "", fmt.Errorf("unpack %s: %w", imgRef, uerr)
	}

	hostNet := usesHostNetwork(req)
	if !hostNet {
		if _, nerr := createNamedNetNS(id); nerr != nil {
			return "", fmt.Errorf("create netns: %w", nerr)
		}
		defer func() {
			if err != nil {
				releaseNamedNetNS(id)
			}
		}()
	}

	hostname := req.Hostname
	if hostname == "" {
		hostname = name
	}
	if hostname == "" {
		hostname = id[:12]
	}
	if perr := prepareContainerRoot(ns, id, hostname, req.HostConfig.Dns, req.HostConfig.ExtraHosts); perr != nil {
		return "", perr
	}

	mounts, anonVols, merr := computeContainerMounts(ns, id, req)
	if merr != nil {
		return "", merr
	}

	var imgCfg *ocispecImageConfig
	if spec, serr := img.Spec(nsCtx); serr == nil {
		imgCfg = &ocispecImageConfig{
			Entrypoint: spec.Config.Entrypoint,
			Cmd:        spec.Config.Cmd,
			Env:        spec.Config.Env,
			User:       spec.Config.User,
			WorkingDir: spec.Config.WorkingDir,
		}
	}

	specOpts, serr := buildSpecOpts(id, hostname, imgCfg, req, mounts, hostNet)
	if serr != nil {
		return "", serr
	}

	portMappings := portMappingsFromCreate(req)
	meta := &containerMeta{
		ID:               id,
		Name:             name,
		Namespace:        ns,
		ImageRef:         imgRef,
		Ports:            portMappings,
		Networks:         []string{effectiveNetworkName(req.HostConfig.NetworkMode)},
		Aliases:          requestedNetworkAliases(req),
		TTY:              req.Tty,
		AutoRemove:       req.HostConfig.AutoRemove,
		StopSignal:       req.StopSignal,
		WorkingDir:       req.WorkingDir,
		Entrypoint:       req.Entrypoint,
		Mounts:           req.HostConfig.Mounts,
		AnonymousVolumes: anonVols,
		Healthcheck:      req.Healthcheck,
	}
	if serr := saveContainerMeta(meta); serr != nil {
		return "", serr
	}
	defer func() {
		if err != nil {
			deleteContainerMeta(ns, id)
		}
	}()

	labels := map[string]string{}
	for k, v := range req.Labels {
		labels[k] = v
	}
	displayName := name
	if displayName == "" {
		displayName = id
	}
	labels[labelName] = displayName
	networksJSON, _ := json.Marshal(meta.Networks)
	labels[labelNetworks] = string(networksJSON)
	if len(portMappings) > 0 {
		portsJSON, _ := json.Marshal(portMappings)
		labels[labelPorts] = string(portsJSON)
	}

	if _, cerr := cl.NewContainer(nsCtx, id,
		client.WithNewSnapshot(id, img),
		client.WithImage(img),
		client.WithContainerLabels(labels),
		client.WithNewSpec(specOpts...),
	); cerr != nil {
		err = cerr
		return "", fmt.Errorf("new container: %w", cerr)
	}
	return id, nil
}

// portMappingsFromCreate extracts published host ports from the create
// request in CNI port-mapping shape (same expansion rules as before).
func portMappingsFromCreate(req dockerCreateRequest) []cniPortMapping {
	var out []cniPortMapping
	for cportSpec, hostPorts := range req.HostConfig.PortBindings {
		proto := "tcp"
		parts := strings.SplitN(cportSpec, "/", 2)
		cport := parts[0]
		if len(parts) == 2 {
			proto = parts[1]
		}
		cPorts, err := expandPortRange(cport)
		if err != nil {
			continue
		}
		for _, hp := range hostPorts {
			hPorts, herr := expandPortRange(hp.HostPort)
			if herr != nil || (len(hPorts) > 1 && len(hPorts) != len(cPorts)) {
				continue
			}
			hostIP := hp.HostIp
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}
			for i, c := range cPorts {
				h := hPorts[0]
				if len(hPorts) == len(cPorts) {
					h = hPorts[i]
				}
				if h == 0 {
					continue
				}
				out = append(out, cniPortMapping{HostPort: h, ContainerPort: c, Protocol: proto, HostIP: hostIP})
			}
		}
	}
	return out
}
