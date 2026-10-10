package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
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

// sortMountsByDepth orders mounts parents first, as Docker does
// (daemon sortMounts): a tmpfs on /run listed after a bind on
// /run/docker.sock otherwise covered it — k3d's tools container has exactly
// that (--tmpfs /run --tmpfs /var/run -v /var/run/docker.sock:...).
// Stable, so mounts of equal depth keep the requested order.
func sortMountsByDepth(mounts []specs.Mount) []specs.Mount {
	out := slices.Clone(mounts)
	depth := func(m specs.Mount) int {
		return strings.Count(filepath.Clean("/"+m.Destination), "/")
	}
	sort.SliceStable(out, func(i, j int) bool { return depth(out[i]) < depth(out[j]) })
	return out
}

// primaryNetworkMode is the network a create request really asks for.
// Docker (updateContainerNetworkSettings) joins the NetworkMode network only
// when EndpointsConfig is empty: k3d sends NetworkMode "bridge" with its
// cluster network in EndpointsConfig and gets that network alone. Joining
// both gave the nodes two default routes.
func primaryNetworkMode(req dockerCreateRequest) string {
	mode := req.HostConfig.NetworkMode
	if effectiveNetworkName(mode) != "bridge" || req.NetworkingConfig == nil || len(req.NetworkingConfig.EndpointsConfig) == 0 {
		return mode
	}
	var names []string
	for name := range req.NetworkingConfig.EndpointsConfig {
		if effectiveNetworkName(name) == "bridge" {
			return mode // the default network is among the requested ones
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names[0]
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
func prepareContainerRoot(ns, id, hostname string, hc dockerHostConfig) error {
	dns, extraHosts := hc.Dns, hc.ExtraHosts
	dir := containerMetaDir(ns, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	hosts := containerHostsContent(hostname, extraHosts, desktopHostIP(), hostGatewayIP())
	if err := os.WriteFile(containerHostsPath(ns, id), []byte(hosts), 0o644); err != nil {
		return err
	}

	base, _ := os.ReadFile("/etc/resolv.conf")
	resolv := resolvConfContent(string(base), dns, hc.DnsSearch, hc.DnsOptions)
	if err := os.WriteFile(containerResolvPath(ns, id), []byte(resolv), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "hostname"), []byte(hostname+"\n"), 0o644)
}

// containerHostsContent renders a container's /etc/hosts. hostIP is what
// "host-gateway" and host.docker.internal resolve to — the Mac, not the VM:
// on macOS that is what users mean by "the host" (see desktopHostIP).
// gateway.docker.internal is the VM's gateway, the Mac's NAT address. An
// explicit --add-host for one of those names wins over the built-in entry.
func containerHostsContent(hostname string, extraHosts []string, hostIP, gatewayIP string) string {
	hosts := "127.0.0.1\tlocalhost\n" +
		"::1\tlocalhost ip6-localhost ip6-loopback\n" +
		"fe00::0\tip6-localnet\n" +
		"ff00::0\tip6-mcastprefix\n" +
		"ff02::1\tip6-allnodes\n" +
		"ff02::2\tip6-allrouters\n" +
		"127.0.0.1\t" + hostname + "\n"
	overridden := map[string]bool{}
	for _, eh := range extraHosts {
		// The API carries "name:ip"; newer CLIs also accept "name=ip".
		// Split at the first separator only: IPv6 addresses contain ':'.
		sep := strings.IndexAny(eh, ":=")
		if sep <= 0 || sep == len(eh)-1 {
			continue
		}
		name, ip := eh[:sep], eh[sep+1:]
		if ip == "host-gateway" {
			ip = hostIP
		}
		hosts += ip + "\t" + name + "\n"
		overridden[name] = true
	}
	for _, e := range [][2]string{{"host.docker.internal", hostIP}, {"gateway.docker.internal", gatewayIP}} {
		if !overridden[e[0]] {
			hosts += e[1] + "\t" + e[0] + "\n"
		}
	}
	return hosts
}

// hostGatewayIP is the Mac's address as seen from the VM: the default
// gateway of the Virtualization.framework NAT. Containers reach it through
// the CNI bridge's masquerade like any other off-subnet address. Falls back
// to the VM's own address (the pre-Desktop-compat behavior) when there is no
// default route yet.
func hostGatewayIP() string {
	if data, err := os.ReadFile("/proc/net/route"); err == nil {
		if gw := parseDefaultGateway(string(data)); gw != "" {
			return gw
		}
	}
	if ip := detectGuestIP(); ip != "" {
		return ip
	}
	return "10.10.0.1"
}

// parseDefaultGateway extracts the IPv4 default gateway from the contents of
// /proc/net/route (little-endian hex addresses).
func parseDefaultGateway(routeTable string) string {
	for _, line := range strings.Split(routeTable, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || v == 0 {
			continue
		}
		return fmt.Sprintf("%d.%d.%d.%d", byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	return ""
}

// --- mounts ---------------------------------------------------------------

// computeContainerMounts translates Binds/Mounts/TmpFs into OCI mounts plus
// the standard /etc file bind-mounts. Named volumes are created on demand;
// anonymous volumes are tracked for removal on delete.
// isMountMode reports whether s is a -v mode list ("ro", "rw,nocopy", "z").
func isMountMode(s string) bool {
	if s == "" {
		return false
	}
	for _, m := range strings.Split(s, ",") {
		switch m {
		case "ro", "rw", "nocopy", "z", "Z", "shared", "rshared", "slave", "rslave",
			"private", "rprivate", "consistent", "cached", "delegated":
		default:
			return false
		}
	}
	return true
}

// volumeMount is a volume (named or anonymous, never a host bind) mounted
// into the container: a candidate for copy-up from the image.
type volumeMount struct {
	dir, dst string
	nocopy   bool
}

func computeContainerMounts(ns, id string, req dockerCreateRequest) (_ []specs.Mount, _ []string, _ []volumeMount, subpaths []subpathMount, _ error) {
	var mounts []specs.Mount
	var anonVols []string
	var volMounts []volumeMount
	nocopy := false // set per spec below

	addBind := func(src, dst string, ro bool) {
		opts := []string{"rbind"}
		if ro {
			opts = append(opts, "ro")
		}
		mounts = append(mounts, specs.Mount{Type: "bind", Source: src, Destination: dst, Options: opts})
	}
	addNamedVolume := func(volName, dst string, ro, anonymous bool) error {
		if dev, ok := bindDeviceOption(loadVolumeOptions(volName)); ok {
			addBind(dev, dst, ro) // a bind-backed local volume
			return nil
		}
		// A named volume is the daemon's, wherever it was created; an
		// anonymous one belongs to the container's namespace.
		volNS := ns
		if !anonymous {
			volNS = volumeNamespace(ns, volName)
		}
		dir := volumeDataDir(volNS, volName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		addBind(dir, dst, ro)
		volMounts = append(volMounts, volumeMount{dir: dir, dst: dst, nocopy: nocopy})
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
			if err := addNamedVolume(name, dst, ro, true); err != nil {
				return err
			}
			markAnonymousVolume(ns, name)
			anonVols = append(anonVols, name)
		case strings.HasPrefix(src, "/"):
			if sock, ok := dockerSocketBindSource(src); ok {
				addBind(sock, dst, ro)
				break
			}
			src = macPathInVM(src)
			if err := ensureBindSource(src); err != nil {
				return err
			}
			addBind(src, dst, ro)
		default:
			if err := addNamedVolume(src, dst, ro, false); err != nil {
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
			// "dst:mode" (-v /data:ro) is an anonymous volume with a mode,
			// as Docker reads it; otherwise it is "src:dst".
			if isMountMode(parts[1]) {
				modes := strings.Split(parts[1], ",")
				nocopy = slices.Contains(modes, "nocopy")
				defer func() { nocopy = false }()
				return addHostOrVolume("", parts[0], slices.Contains(modes, "ro"))
			}
			return addHostOrVolume(parts[0], parts[1], false)
		case 3:
			modes := strings.Split(parts[2], ",")
			nocopy = slices.Contains(modes, "nocopy")
			defer func() { nocopy = false }()
			return addHostOrVolume(parts[0], parts[1], slices.Contains(modes, "ro"))
		}
		return fmt.Errorf("invalid mount spec")
	}

	for _, b := range req.HostConfig.Binds {
		if err := parseBindSpec(b); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("bind %q: %w", b, err)
		}
	}
	for _, m := range req.HostConfig.Mounts {
		if m.Type == "tmpfs" && m.Target != "" {
			// --mount type=tmpfs (compose `tmpfs:` with options) — it was
			// silently dropped.
			mounts = append(mounts, tmpfsMountSpec(m))
			continue
		}
		// A volume mount without a source is anonymous (compose `- /data`).
		if m.Type == "tmpfs" || m.Target == "" || (m.Source == "" && m.Type != "volume") {
			continue
		}
		nocopy = m.VolumeOptions != nil && m.VolumeOptions.NoCopy
		if m.Type == "volume" && m.Source != "" && m.VolumeOptions != nil && m.VolumeOptions.Subpath != "" {
			volDir := volumeDataDir(volumeNamespace(ns, m.Source), m.Source)
			dir, serr := volumeSubpath(volDir, m.VolumeOptions.Subpath)
			if serr != nil {
				return nil, nil, nil, nil, fmt.Errorf("mount %q: %w", m.Target, serr)
			}
			// runc mounts a staging mountpoint that every start re-arms
			// from a beneath-the-volume resolution (armSubpathMounts), not
			// the path itself, which a container could swap meanwhile.
			staging := subpathStagingPath(ns, id, len(subpaths))
			addBind(staging, m.Target, m.ReadOnly)
			subpaths = append(subpaths, subpathMount{VolumeDir: volDir, Subpath: m.VolumeOptions.Subpath, Source: dir, Staging: staging})
			continue
		}
		if m.Type != "" && m.Type != "bind" && m.Type != "volume" {
			// type=image (API 1.48), npipe, cluster: refused, not taken
			// for a volume named after the image.
			return nil, nil, nil, nil, errInvalid("mount type %q is not supported by anvil", m.Type)
		}
		if m.Type == "bind" {
			// Unlike -v, --mount type=bind never creates its source.
			if _, serr := os.Stat(macPathInVM(m.Source)); serr != nil {
				if _, ok := dockerSocketBindSource(m.Source); !ok {
					return nil, nil, nil, nil, errInvalid("invalid mount config for type \"bind\": bind source path does not exist: %s", m.Source)
				}
			}
		}
		err := addHostOrVolume(m.Source, m.Target, m.ReadOnly)
		nocopy = false
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("mount %q: %w", m.Target, err)
		}
	}
	// The CLI sends a bare `-v /path` as Config.Volumes, not as a bind.
	// Paths something else already mounts need no anonymous volume.
	taken := map[string]bool{}
	for _, m := range mounts {
		taken[filepath.Clean(m.Destination)] = true
	}
	for _, dst := range slices.Sorted(maps.Keys(req.Volumes)) {
		if taken[filepath.Clean(dst)] {
			continue
		}
		if err := addHostOrVolume("", dst, false); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("volume %q: %w", dst, err)
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

	// Standard /etc files as bind mounts, writable unless --read-only, as
	// Docker's NetworkMounts: k3d rewrites a node's /etc/hosts, and images
	// append to it at startup. The peer block is rewritten in place (same
	// inode, other lines kept), so it coexists with such edits.
	etcOpts := []string{"rbind"}
	if req.HostConfig.ReadonlyRootfs {
		etcOpts = []string{"rbind", "ro"}
	}
	mounts = append(mounts,
		specs.Mount{Type: "bind", Source: containerHostsPath(ns, id), Destination: "/etc/hosts",
			Options: etcOpts},
		specs.Mount{Type: "bind", Source: containerResolvPath(ns, id), Destination: "/etc/resolv.conf",
			Options: etcOpts},
		specs.Mount{Type: "bind", Source: filepath.Join(containerMetaDir(ns, id), "hostname"),
			Destination: "/etc/hostname", Options: etcOpts},
	)
	return mounts, anonVols, volMounts, subpaths, nil
}

// volumesFromMounts resolves --volumes-from: every volume and bind mount of
// the referenced containers, read from their OCI specs (which already
// include what they inherited themselves). A ":ro"/":rw" suffix overrides
// the access mode, as in Docker.
//
// A volume-subpath mount is carried over as a subpath of the new container
// (ns/id, with staging indexes from firstIndex): the source's staging
// mountpoint is its own, gone when it is removed or the VM cold-boots.
func volumesFromMounts(ctx context.Context, refs []string, ns, id string, firstIndex int) ([]specs.Mount, []subpathMount, error) {
	if len(refs) == 0 {
		return nil, nil, nil
	}
	cl, err := pc.get(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("containerd client: %w", err)
	}
	var out []specs.Mount
	var subpaths []subpathMount
	for _, ref := range refs {
		name, mode := ref, ""
		if i := strings.LastIndexByte(ref, ':'); i > 0 {
			name, mode = ref[:i], ref[i+1:]
		}
		if mode != "" && mode != "ro" && mode != "rw" {
			return nil, nil, fmt.Errorf("volumes-from %q: invalid mode %q", ref, mode)
		}
		srcNS, srcID, _, rerr := resolveDockerID(ctx, name)
		if rerr != nil {
			return nil, nil, fmt.Errorf("volumes-from %q: %w", name, rerr)
		}
		nsCtx := namespaces.WithNamespace(ctx, srcNS)
		c, lerr := cl.LoadContainer(nsCtx, srcID)
		if lerr != nil {
			return nil, nil, fmt.Errorf("volumes-from %q: %w", name, lerr)
		}
		spec, serr := c.Spec(nsCtx)
		if serr != nil {
			return nil, nil, fmt.Errorf("volumes-from %q: spec: %w", name, serr)
		}
		staged := stagedSubpaths(srcNS, srcID)
		for _, m := range inheritableMounts(spec.Mounts, mode) {
			if sp, ok := staged[filepath.Clean(m.Source)]; ok {
				sp.Staging = subpathStagingPath(ns, id, firstIndex+len(subpaths))
				m.Source = sp.Staging
				subpaths = append(subpaths, sp)
			}
			out = append(out, m)
		}
	}
	return out, subpaths, nil
}

// stagedSubpaths maps a container's subpath staging mountpoints to their
// subpath mounts.
func stagedSubpaths(ns, id string) map[string]subpathMount {
	out := map[string]subpathMount{}
	if meta, err := loadContainerMeta(ns, id); err == nil {
		for _, sp := range meta.SubpathMounts {
			if sp.Staging != "" {
				out[filepath.Clean(sp.Staging)] = sp
			}
		}
	}
	return out
}

// inheritableMounts picks the user volumes out of a container's OCI mounts:
// bind mounts, minus the per-container /etc files and docker-init.
func inheritableMounts(all []specs.Mount, mode string) []specs.Mount {
	var out []specs.Mount
	for _, m := range all {
		if m.Type != "bind" {
			continue
		}
		switch m.Destination {
		case "/etc/hosts", "/etc/resolv.conf", "/etc/hostname", containerInitPath, rosettaCacheSocket:
			continue
		}
		opts := []string{}
		for _, o := range m.Options {
			if o != "ro" && o != "rw" {
				opts = append(opts, o)
			}
		}
		if mode == "ro" || (mode == "" && slices.Contains(m.Options, "ro")) {
			opts = append(opts, "ro")
		}
		m.Options = opts
		out = append(out, m)
	}
	return out
}

// mergeInheritedMounts adds inherited mounts whose destination the
// container does not mount itself: its own -v/--mount wins, as in Docker.
func mergeInheritedMounts(own, inherited []specs.Mount) []specs.Mount {
	taken := map[string]bool{}
	for _, m := range own {
		taken[filepath.Clean(m.Destination)] = true
	}
	for _, m := range inherited {
		dst := filepath.Clean(m.Destination)
		if taken[dst] {
			continue
		}
		taken[dst] = true
		own = append(own, m)
	}
	return own
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

// docker-init (a static tini) ships in the guest and is bind-mounted into
// containers that ask for --init, at the path Docker uses.
const (
	guestInitPath     = "/opt/containerd/bin/docker-init"
	containerInitPath = "/sbin/docker-init"
)

// dockerInitMount is the read-only bind of the guest's docker-init.
func dockerInitMount() (specs.Mount, error) {
	if _, err := os.Stat(guestInitPath); err != nil {
		return specs.Mount{}, fmt.Errorf("--init: %s is missing from the guest (rebuild the initramfs)", guestInitPath)
	}
	return specs.Mount{Type: "bind", Source: guestInitPath, Destination: containerInitPath,
		Options: []string{"rbind", "ro"}}, nil
}

// userProcessArgs strips the injected docker-init prefix, so ps/inspect
// show the command the user asked for, as Docker does.
func userProcessArgs(args []string) []string {
	if len(args) >= 2 && args[0] == containerInitPath && args[1] == "--" {
		return args[2:]
	}
	return args
}

// cpuPeriod is the CFS period Docker and runc use by default (100 ms).
const cpuPeriod = 100000

// buildSpecOpts composes the OCI spec options for a create request.
func buildSpecOpts(id, hostname string, imgCfg *ocispecImageConfig, req dockerCreateRequest, mounts []specs.Mount, hostNet bool, joinNetNS string) ([]oci.SpecOpts, error) {
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
	if req.HostConfig.Init != nil && *req.HostConfig.Init {
		initMount, ierr := dockerInitMount()
		if ierr != nil {
			return nil, ierr
		}
		mounts = append(mounts, initMount)
		argv = append([]string{containerInitPath, "--"}, argv...)
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
	}
	if req.HostConfig.UtsMode != "host" {
		// A shared UTS namespace makes the hostname read-only (runc refuses
		// to set one), and docker's --uts=host does not set it either.
		opts = append(opts, oci.WithHostname(hostname))
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
		// As Docker: every device of the VM and access to all of them —
		// kubelet in k3s/kind nodes needs /dev/kmsg, DinD loop devices.
		opts = append(opts, oci.WithPrivileged, oci.WithHostDevices, oci.WithAllDevicesAllowed)
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
		// NanoCpus is billionths of a CPU: 1.5 CPUs == 1_500_000_000. The
		// CFS quota is that fraction of one period. (oci.WithCPUs is the
		// cpuset — a CPU *list* — and must not be used here.)
		quota := req.HostConfig.NanoCpus * cpuPeriod / 1e9
		opts = append(opts, oci.WithCPUCFS(quota, cpuPeriod))
	}
	{
		hc := &req.HostConfig
		if hc.CpuShares > 0 {
			opts = append(opts, oci.WithCPUShares(uint64(hc.CpuShares)))
		}
		if hc.CpuPeriod > 0 && hc.CpuQuota != 0 {
			// CpuQuota -1 means "unlimited" and maps to the cgroup "max".
			opts = append(opts, oci.WithCPUCFS(hc.CpuQuota, uint64(hc.CpuPeriod)))
		}
		if hc.CpusetCpus != "" {
			opts = append(opts, oci.WithCPUs(hc.CpusetCpus))
		}
		if hc.CpusetMems != "" {
			opts = append(opts, oci.WithCPUsMems(hc.CpusetMems))
		}
		// Docker's MemorySwap is memory+swap combined; runc applies the
		// v1→v2 conversion itself (see memorySwapSpec).
		if swap, serr := memorySwapSpec(hc.Memory, hc.MemorySwap); serr != nil {
			return nil, serr
		} else if swap != nil {
			opts = append(opts, oci.WithMemorySwap(*swap))
		}
		if hc.MemoryReservation > 0 {
			reservation := hc.MemoryReservation
			opts = append(opts, func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
				if s.Linux == nil {
					s.Linux = &specs.Linux{}
				}
				if s.Linux.Resources == nil {
					s.Linux.Resources = &specs.LinuxResources{}
				}
				if s.Linux.Resources.Memory == nil {
					s.Linux.Resources.Memory = &specs.LinuxMemory{}
				}
				s.Linux.Resources.Memory.Reservation = &reservation
				return nil
			})
		}
		if hc.PidsLimit != nil && *hc.PidsLimit > 0 {
			// docker's -1 (unlimited) is the cgroup default here — skipped.
			opts = append(opts, oci.WithPidsLimit(*hc.PidsLimit))
		}
		if hc.ShmSize > 0 {
			opts = append(opts, oci.WithDevShmSize(hc.ShmSize/1024))
		}
		{
			// Always set: without it the process inherits the shim's
			// score, which follows containerd's -999 (supervise.go), and
			// the OOM killer would prefer the agent over a container.
			adj := hc.OomScoreAdj
			opts = append(opts, func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
				if s.Process == nil {
					return errors.New("oom_score_adj: missing process in spec")
				}
				s.Process.OOMScoreAdj = &adj
				return nil
			})
		}
		for _, opt := range hc.SecurityOpt {
			switch {
			case opt == "no-new-privileges" || opt == "no-new-privileges:true":
				opts = append(opts, oci.WithNoNewPrivileges)
			case strings.HasPrefix(opt, "seccomp=") || strings.HasPrefix(opt, "seccomp:"):
				// applied last by seccompSpecOpt (it needs the final caps)
			case strings.HasPrefix(opt, "apparmor=") || strings.HasPrefix(opt, "apparmor:") ||
				strings.HasPrefix(opt, "label=") || strings.HasPrefix(opt, "label:"):
				// The guest has neither AppArmor nor SELinux: like Docker on
				// such a host (Docker Desktop too), the options are no-ops.
				// kind, FUSE images and devcontainers pass them routinely.
			case opt == "systempaths=unconfined" || opt == "systempaths:unconfined":
				opts = append(opts, func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
					if s.Linux != nil {
						s.Linux.MaskedPaths, s.Linux.ReadonlyPaths = nil, nil
					}
					return nil
				})
			default:
				return nil, fmt.Errorf("security-opt %q is not supported by anvil", opt)
			}
		}
		for _, u := range hc.Ulimits {
			name := "RLIMIT_" + strings.ToUpper(u.Name)
			if !knownRlimits[name] {
				return nil, fmt.Errorf("invalid ulimit %q", u.Name)
			}
			opts = append(opts, oci.WithRlimit(&specs.POSIXRlimit{
				Type: name, Soft: uint64(u.Soft), Hard: uint64(u.Hard),
			}))
		}
		if len(hc.GroupAdd) > 0 {
			groups := hc.GroupAdd
			opts = append(opts, func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
				if s.Process == nil {
					return errors.New("group_add: missing process in spec")
				}
				for _, g := range groups {
					// Numeric-only by design: names would need /etc/group
					// from the image rootfs.
					gid, err := strconv.Atoi(g)
					if err != nil || gid < 0 {
						return fmt.Errorf("group-add %q: only numeric gids are supported", g)
					}
					s.Process.User.AdditionalGids = append(s.Process.User.AdditionalGids, uint32(gid))
				}
				return nil
			})
		}
		if hc.UtsMode == "host" {
			opts = append(opts, oci.WithHostNamespace(specs.UTSNamespace))
		}
		if hc.IpcMode == "host" {
			opts = append(opts, oci.WithHostNamespace(specs.IPCNamespace))
		}
		if hc.CgroupnsMode == "host" {
			opts = append(opts, oci.WithHostNamespace(specs.CgroupNamespace))
		}
		if len(hc.Annotations) > 0 {
			opts = append(opts, oci.WithAnnotations(hc.Annotations))
		}
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
		opts = append(opts, oci.WithMounts(sortMountsByDepth(mounts)))
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
		path := netnsPathFor(id)
		if joinNetNS != "" {
			path = joinNetNS // --network container:<x>
		}
		opts = append(opts, oci.WithLinuxNamespace(specs.LinuxNamespace{
			Type: specs.NetworkNamespace,
			Path: path,
		}))
	}
	// Last: the profile is resolved against the final capability set.
	seccompOpt, err := seccompSpecOpt(req.HostConfig.SecurityOpt, req.HostConfig.Privileged)
	if err != nil {
		return nil, err
	}
	if seccompOpt != nil {
		opts = append(opts, seccompOpt)
	}
	return opts, nil
}

// --- create ----------------------------------------------------------------

// createNativeContainer registers the container with containerd and prepares
// all start-time state. It returns the containerd ID and create warnings.
func createNativeContainer(ctx context.Context, ns, name, platform string, req dockerCreateRequest) (_ string, warnings []string, err error) {
	cl, err := pc.get(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("containerd client: %w", err)
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)

	id := newContainerID()
	imgRef := canonicalizeImageRef(req.Image)
	img, err := cl.GetImage(nsCtx, imgRef)
	if err != nil {
		return "", nil, fmt.Errorf("image %s not found in namespace %s: %w", imgRef, ns, err)
	}
	img = imageWithPlatform(nsCtx, cl, img, platform)
	// The platform actually resolved (an amd64-only image under Rosetta
	// with none requested included), for commit to pick the same subtree.
	actualPlatform := imagePlatform(nsCtx, img)
	if actualPlatform == "" {
		actualPlatform = platform
	}
	if platform == "" {
		if w := emulatedPlatformWarning(nsCtx, img); w != "" {
			warnings = append(warnings, w)
		}
	}

	// The rootfs snapshot requires unpacked layers.
	if uerr := img.Unpack(nsCtx, ""); uerr != nil {
		return "", nil, fmt.Errorf("unpack %s: %w", imgRef, uerr)
	}

	hostNet := usesHostNetwork(req)
	// joinNetNS is the target's named netns for --network container:<x>.
	joinNetNS := ""
	var joinHosts, joinResolv, joinHostname string
	if isContainerNetworkMode(req.HostConfig.NetworkMode) {
		tns, tid, terr := resolveNetworkContainer(ctx, req.HostConfig.NetworkMode)
		if terr != nil {
			return "", nil, terr
		}
		joinNetNS = netnsPathFor(tid)
		joinHosts, joinResolv = containerHostsPath(tns, tid), containerResolvPath(tns, tid)
		if b, rerr := os.ReadFile(filepath.Join(containerMetaDir(tns, tid), "hostname")); rerr == nil {
			joinHostname = strings.TrimSpace(string(b))
		}
		if req.Hostname == "" && joinHostname != "" {
			req.Hostname = joinHostname // the target's UTS name, as Docker does
		}
	}
	ownNetNS := !hostNet && joinNetNS == ""
	if ownNetNS {
		if _, nerr := createNamedNetNS(id); nerr != nil {
			return "", nil, fmt.Errorf("create netns: %w", nerr)
		}
	}
	// Undo everything below on any failure. err is the named result, so
	// every `return "", nil, …` counts — the per-step perr/merr/serr variables
	// used to bypass the old cleanup and leak the bind-mounted netns.
	var anonVols []string
	defer func() {
		if err == nil {
			return
		}
		for _, v := range anonVols {
			os.RemoveAll(volumeDataDir(ns, v))
		}
		deleteContainerMeta(ns, id) // also the prepared root (hosts, resolv.conf)
		if ownNetNS {
			releaseNamedNetNS(id)
		}
	}()

	hostname := req.Hostname
	if hostname == "" && !req.generatedName {
		hostname = name
	}
	if hostname == "" {
		hostname = truncateID(dockerID(ns, id)) // the ID docker ps shows, as Docker does
	}
	if perr := prepareContainerRoot(ns, id, hostname, req.HostConfig); perr != nil {
		return "", nil, perr
	}

	mounts, vols, volMounts, subpaths, merr := computeContainerMounts(ns, id, req)
	if merr != nil {
		return "", nil, merr
	}
	anonVols = vols
	inherited, inheritedSubpaths, verr := volumesFromMounts(ctx, req.HostConfig.VolumesFrom, ns, id, len(subpaths))
	if verr != nil {
		return "", nil, verr
	}
	subpaths = append(subpaths, inheritedSubpaths...)
	mounts = mergeInheritedMounts(mounts, inherited)
	if m, ok := rosettaCacheMount(actualPlatform); ok {
		mounts = append(mounts, m)
	}

	var imgCfg *ocispecImageConfig
	var imgExposed map[string]struct{}
	if spec, serr := img.Spec(nsCtx); serr == nil {
		// The image's VOLUME paths not mounted otherwise get an anonymous
		// volume each (after --volumes-from, which takes precedence).
		imgVols, imgMounts, ierr := imageVolumeMounts(ns, spec.Config.Volumes, mounts)
		if ierr != nil {
			return "", nil, ierr
		}
		anonVols = append(anonVols, imgVols...)
		for _, vm := range imgMounts {
			mounts = append(mounts, specs.Mount{Type: "bind", Source: vm.dir, Destination: vm.dst, Options: []string{"rbind"}})
		}
		volMounts = append(volMounts, imgMounts...)
		imgExposed = spec.Config.ExposedPorts
		imgCfg = &ocispecImageConfig{
			Entrypoint: spec.Config.Entrypoint,
			Cmd:        spec.Config.Cmd,
			Env:        spec.Config.Env,
			User:       spec.Config.User,
			WorkingDir: spec.Config.WorkingDir,
		}
	}

	if joinNetNS != "" {
		// The target's /etc/hosts and resolv.conf, like Docker.
		for i := range mounts {
			switch mounts[i].Destination {
			case "/etc/hosts":
				mounts[i].Source = joinHosts
			case "/etc/resolv.conf":
				mounts[i].Source = joinResolv
			}
		}
	}
	specOpts, serr := buildSpecOpts(id, hostname, imgCfg, req, mounts, hostNet, joinNetNS)
	if serr != nil {
		return "", nil, serr
	}

	portMappings := portMappingsFromCreate(req)
	if req.HostConfig.PublishAllPorts {
		portMappings = append(portMappings, publishAllMappings(req, imgExposed)...)
	}
	meta := &containerMeta{
		ID:               id,
		Name:             name,
		Namespace:        ns,
		ImageRef:         imgRef,
		Ports:            portMappings,
		Networks:         append([]string{effectiveNetworkName(req.HostConfig.NetworkMode)}, secondaryNetworksFromCreate(req)...),
		Aliases:          requestedNetworkAliases(req),
		NetworkAliases:   requestedNetworkAliasesByNetwork(req),
		NetworkIPs:       requestedNetworkIPs(req),
		SubpathMounts:    subpaths,
		TTY:              req.Tty,
		AutoRemove:       req.HostConfig.AutoRemove,
		OpenStdin:        req.OpenStdin,
		StopTimeout:      req.StopTimeout,
		StdinOnce:        req.StdinOnce,
		StopSignal:       req.StopSignal,
		User:             req.User,
		ConfigUser:       configUser(req.User, imgCfg),
		Domainname:       req.Domainname,
		ExposedPorts:     exposedPortList(req, imgExposed),
		WorkingDir:       req.WorkingDir,
		Entrypoint:       req.Entrypoint,
		Mounts:           req.HostConfig.Mounts,
		AnonymousVolumes: anonVols,
		Healthcheck:      req.Healthcheck,
		HostConfig:       &req.HostConfig,
		Platform:         actualPlatform,
		MountPoints:      mountPointsFor(mounts),
	}
	if serr := saveContainerMeta(meta); serr != nil {
		return "", nil, serr
	}

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
		return "", nil, fmt.Errorf("new container: %w", cerr)
	}
	// Seed empty volumes with the image's content at their mount point, as
	// Docker does on first mount. Best effort: the container exists now.
	if len(volMounts) > 0 {
		if cerr := copyUpVolumes(ns, id, volMounts); cerr != nil {
			log.Printf("[docker-api] volume copy-up for %s: %v", truncateID(id), cerr)
		}
	}
	return id, warnings, nil
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
			hostIP := hp.HostIp
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}
			hostSpec := strings.TrimSpace(hp.HostPort)
			if hostSpec == "" || hostSpec == "0" {
				// `-p 80`: Docker picks a free host port at start.
				for _, c := range cPorts {
					out = append(out, cniPortMapping{ContainerPort: c, Protocol: proto, HostIP: hostIP, Ephemeral: true})
				}
				continue
			}
			hPorts, herr := expandPortRange(hostSpec)
			if herr != nil {
				continue
			}
			if len(hPorts) > 1 && len(cPorts) == 1 {
				// `-p 8000-8010:80`: one free port from the range.
				out = append(out, cniPortMapping{ContainerPort: cPorts[0], Protocol: proto, HostIP: hostIP,
					Ephemeral: true, RangeLo: hPorts[0], RangeHi: hPorts[len(hPorts)-1]})
				continue
			}
			if len(hPorts) > 1 && len(hPorts) != len(cPorts) {
				continue
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

// configUser is inspect's Config.User: --user wins over the image's USER.
func configUser(requested string, img *ocispecImageConfig) string {
	if requested != "" || img == nil {
		return requested
	}
	return img.User
}

// exposedPortList is Config.ExposedPorts: the image's EXPOSE, the request's
// ExposedPorts and every published container port, sorted.
func exposedPortList(req dockerCreateRequest, imageExposed map[string]struct{}) []string {
	set := map[string]bool{}
	add := func(k string) {
		if k == "" {
			return
		}
		if !strings.Contains(k, "/") {
			k += "/tcp"
		}
		set[k] = true
	}
	for k := range imageExposed {
		add(k)
	}
	for k := range req.ExposedPorts {
		add(k)
	}
	for k := range req.HostConfig.PortBindings {
		add(k)
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tmpfsMountSpec turns --mount type=tmpfs into an OCI mount with Docker's
// defaults (noexec,nosuid,nodev) and the size/mode/options given.
func tmpfsMountSpec(m dockerMount) specs.Mount {
	opts := []string{"nosuid", "nodev"}
	exec := false
	if t := m.TmpfsOptions; t != nil {
		if t.SizeBytes > 0 {
			opts = append(opts, fmt.Sprintf("size=%d", t.SizeBytes))
		}
		if t.Mode != 0 {
			opts = append(opts, fmt.Sprintf("mode=%o", t.Mode))
		}
		for _, o := range t.Options {
			if len(o) == 1 && o[0] == "exec" {
				exec = true
			} else if len(o) > 0 {
				opts = append(opts, strings.Join(o, "="))
			}
		}
	}
	if !exec {
		opts = append(opts, "noexec")
	}
	if m.ReadOnly {
		opts = append(opts, "ro")
	}
	return specs.Mount{Type: "tmpfs", Source: "tmpfs", Destination: m.Target, Options: opts}
}

// resolvConfContent builds a container's resolv.conf the way Docker does:
// the VM's file, with its nameserver, search and options lines replaced by
// --dns, --dns-search and --dns-option when given (each independently).
func resolvConfContent(base string, dns, search, options []string) string {
	var ns, srch, opts, other []string
	for _, line := range strings.Split(base, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "nameserver":
			ns = append(ns, line)
		case "search", "domain":
			srch = append(srch, line)
		case "options":
			opts = append(opts, line)
		default:
			other = append(other, line)
		}
	}
	if len(dns) > 0 {
		ns = nil
		for _, s := range dns {
			ns = append(ns, "nameserver "+s)
		}
	}
	if len(search) > 0 {
		srch = nil
		if !(len(search) == 1 && search[0] == ".") { // "." clears the search list
			srch = []string{"search " + strings.Join(search, " ")}
		}
	}
	if len(options) > 0 {
		opts = []string{"options " + strings.Join(options, " ")}
	}
	out := append(append(append(append([]string{}, other...), ns...), srch...), opts...)
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// volumeSubpath resolves --mount type=volume,volume-subpath=<p> inside the
// volume's directory; the path must exist and stay inside the volume, as
// Docker requires.
func volumeSubpath(volDir, sub string) (string, error) {
	clean := filepath.Clean("/" + sub)
	dir := filepath.Join(volDir, clean)
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("volume subpath %q: %w", sub, err)
	}
	base, err := filepath.EvalSymlinks(volDir)
	if err != nil {
		return "", err
	}
	if real != base && !strings.HasPrefix(real, base+"/") {
		return "", fmt.Errorf("volume subpath %q escapes the volume", sub)
	}
	return real, nil
}

// requestedNetworkIPs collects the static addresses asked for per network
// (`--ip`, compose ipv4_address); they used to be dropped silently.
func requestedNetworkIPs(req dockerCreateRequest) map[string]string {
	if req.NetworkingConfig == nil {
		return nil
	}
	out := map[string]string{}
	for name, ep := range req.NetworkingConfig.EndpointsConfig {
		if ep.IPAMConfig != nil && ep.IPAMConfig.IPv4Address != "" {
			out[effectiveNetworkName(name)] = ep.IPAMConfig.IPv4Address
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// staticIPFor returns the requested static address of a container on a
// network ("" for none).
func staticIPFor(ns, id, network string) string {
	if meta, err := loadContainerMeta(ns, id); err == nil {
		return meta.NetworkIPs[network]
	}
	return ""
}

// macPathInVM maps a bind source given as the Mac sees it to the VM path
// of the share holding it: on macOS /tmp and /var are symlinks into
// /private, which the VM mounts at /private/tmp and /private/var/folders.
// A source outside those shares stays as given.
func macPathInVM(src string) string {
	for _, alias := range []struct{ mac, vm string }{
		{"/tmp", "/private/tmp"},
		{"/var/folders", "/private/var/folders"},
	} {
		if src != alias.mac && !strings.HasPrefix(src, alias.mac+"/") {
			continue
		}
		if isMountpoint(alias.vm) {
			return alias.vm + strings.TrimPrefix(src, alias.mac)
		}
	}
	return src
}

// ensureBindSource creates a missing -v source directory the way Docker
// does, but only on a Mac share: a path whose nearest existing ancestor is
// the guest's RAM rootfs is a Mac directory anvil does not share, and
// creating it would hand the container an empty directory that vanishes at
// the next boot.
func ensureBindSource(src string) error {
	if _, err := os.Stat(src); err == nil {
		return nil
	}
	var root syscall.Stat_t
	if err := syscall.Stat("/", &root); err != nil {
		return os.MkdirAll(src, 0o755)
	}
	for dir := filepath.Dir(src); ; dir = filepath.Dir(dir) {
		var st syscall.Stat_t
		if syscall.Stat(dir, &st) == nil {
			if st.Dev == root.Dev {
				return fmt.Errorf("mounts denied: the path %s is not shared from the Mac "+
					"(anvil shares /Users, and /Volumes, /tmp, /var/folders unless ANVIL_SHARE_EXTRA says otherwise)", src)
			}
			return os.MkdirAll(src, 0o755)
		}
		if dir == "/" {
			return os.MkdirAll(src, 0o755)
		}
	}
}

// isMountpoint reports whether path is the root of a mount (its device
// differs from its parent's).
func isMountpoint(path string) bool {
	var st, parent syscall.Stat_t
	if syscall.Stat(path, &st) != nil || syscall.Stat(filepath.Dir(path), &parent) != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// subpathMount is a volume-subpath mount as resolved at create; every start
// checks the path still resolves there (a container with write access to
// the volume could have swapped a component for a symlink meanwhile).
type subpathMount struct {
	VolumeDir string `json:"VolumeDir"`
	Subpath   string `json:"Subpath"`
	Source    string `json:"Source"`
	// Staging is the mountpoint the spec binds (containers created before
	// it existed bind Source directly and are only re-verified).
	Staging string `json:"Staging,omitempty"`
}

// subpathStagingRoot holds the per-container subpath mountpoints (tmpfs:
// re-armed at every start, so a cold boot needs nothing).
const subpathStagingRoot = "/run/anvil/subpath"

func subpathStagingPath(ns, id string, i int) string {
	return filepath.Join(subpathStagingRoot, ns, id, strconv.Itoa(i))
}

// verifySubpathMounts re-resolves the container's subpath mounts.
func verifySubpathMounts(m *containerMeta) error {
	for _, sp := range m.SubpathMounts {
		got, err := volumeSubpath(sp.VolumeDir, sp.Subpath)
		if err != nil {
			return err
		}
		if got != sp.Source {
			return fmt.Errorf("volume subpath %q changed since create (now %s); refusing to start", sp.Subpath, got)
		}
	}
	return nil
}
