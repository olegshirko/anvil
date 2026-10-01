package main

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// dockerEngineVersion is the Docker release whose API anvil advertises
// (dockerAPIVersion 1.51 shipped with Docker 28.3). Tools gate features on
// the server version; the old placeholder "24.0.0" understated it.
const dockerEngineVersion = "28.3.0"

// anvilVersion is the release; set at build time with
// -ldflags "-X main.anvilVersion=x.y.z".
var anvilVersion = "dev"

// handleDockerInfo answers GET /info with the fields the docker CLI,
// compose, buildx and testcontainers read: real container and image counts,
// memory, kernel, cgroup v2, runtimes and plugins. No containerd-snapshotter
// DriverStatus: buildx would switch its docker driver to image-store
// features the classic /build path does not provide.
func handleDockerInfo(w http.ResponseWriter, r *http.Request) {
	running, paused, stopped := 0, 0, 0
	if cs, err := listDockerContainers(r.Context(), nil); err == nil {
		for _, c := range cs {
			switch c.State {
			case "running":
				running++
			case "paused":
				paused++
			default:
				stopped++
			}
		}
	}
	images := 0
	if is, err := listDockerImages(r.Context()); err == nil {
		images = len(is)
	}
	kernel := kernelRelease()
	info := map[string]interface{}{
		"ID":                 "anvil-vz-runner",
		"Name":               vmHostname(),
		"Containers":         running + paused + stopped,
		"ContainersRunning":  running,
		"ContainersPaused":   paused,
		"ContainersStopped":  stopped,
		"Images":             images,
		"Driver":             "overlayfs",
		"DockerRootDir":      "/var/lib/containerd",
		"SecurityOptions":    []string{"name=seccomp,profile=builtin", "name=cgroupns"},
		"Architecture":       unameMachine(),
		"OSType":             "linux",
		"OperatingSystem":    "anvil (Apple Virtualization.framework)",
		"OSVersion":          anvilVersion,
		"KernelVersion":      kernel,
		"NCPU":               runtime.NumCPU(),
		"MemTotal":           memTotal(),
		"ServerVersion":      dockerEngineVersion,
		"IndexServerAddress": "https://index.docker.io/v1/",
		"CgroupDriver":       "cgroupfs",
		"CgroupVersion":      "2",
		"DefaultRuntime":     "runc",
		"Runtimes":           map[string]interface{}{"runc": map[string]string{"path": "runc"}},
		"InitBinary":         "docker-init",
		"LoggingDriver":      "json-file",
		"Swarm":              map[string]interface{}{"LocalNodeState": "inactive", "ControlAvailable": false},
		"Plugins": map[string][]string{
			"Volume":  {"local"},
			"Network": {"bridge", "host", "null"},
			"Log":     {"json-file", "none"},
		},
		"Labels":             []string{},
		"ExperimentalBuild":  false,
		"LiveRestoreEnabled": false,
		"MemoryLimit":        true,
		"SwapLimit":          true,
		"CpuCfsPeriod":       true,
		"CpuCfsQuota":        true,
		"PidsLimit":          true,
		"IPv4Forwarding":     true,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// kernelRelease is `uname -r` of the VM ("" when unreadable).
func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// unameMachine is Docker's Architecture ("aarch64"), not Go's GOARCH.
func unameMachine() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return runtime.GOARCH
}

// memTotal is the VM's RAM in bytes, from /proc/meminfo.
func memTotal() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				if kb, err := strconv.ParseInt(f[0], 10, 64); err == nil {
					return kb * 1024
				}
			}
		}
	}
	return 0
}

// vmHostname is the VM's hostname: Docker's info Name, and what
// `--uts host` containers see.
func vmHostname() string {
	b, err := os.ReadFile("/proc/sys/kernel/hostname")
	if err != nil {
		return "anvil"
	}
	return strings.TrimSpace(string(b))
}

// ensureVMHostname names the VM "anvil" when the kernel left it unset
// ("(none)"): `--uts host` containers and docker info showed that.
func ensureVMHostname() {
	if h := vmHostname(); h == "" || h == "(none)" || h == "localhost" {
		os.WriteFile("/proc/sys/kernel/hostname", []byte("anvil"), 0o644) //nolint:errcheck
	}
}
