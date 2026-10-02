package main

import (
	"encoding/hex"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
)

// A --network host container listens in the VM's own network namespace,
// which the Mac cannot reach, and has no port mappings: its ports were
// simply unreachable. The scanner finds the TCP sockets its processes
// listen on and publishes them on the Mac's loopback through the port
// proxy, as Docker Desktop's host networking does.

// containerScanInfo is what the scanner caches per running task (keyed by
// namespace/id, invalidated when the task pid changes).
type containerScanInfo struct {
	pid        uint32
	name       string
	hostNet    bool
	watchPaths []string
	domains    []string // host names (without the suffix)
	httpPort   int
}

func loadContainerScanInfo(ns, id string, pid uint32, labels map[string]string) containerScanInfo {
	info := containerScanInfo{pid: pid}
	meta, err := loadContainerMeta(ns, id)
	if err != nil {
		return info
	}
	info.name = meta.Name
	info.hostNet = meta.HostConfig != nil && meta.HostConfig.NetworkMode == "host"
	info.watchPaths = containerWatchPaths(ns, id)
	info.domains = containerDomains(meta.Name, labels)
	info.httpPort = containerHTTPPort(meta.ExposedPorts, labels)
	return info
}

// tcpListener is one LISTEN socket from /proc/net/tcp{,6}.
type tcpListener struct {
	ip   net.IP
	port int
}

// rootNetnsListeners maps socket inode -> listener for the VM's own network
// namespace (the agent's: PID 1 never leaves it).
func rootNetnsListeners() map[string]tcpListener {
	out := map[string]tcpListener{}
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" { // 0A = LISTEN
				continue
			}
			ip, port, ok := parseProcNetAddr(f[1])
			if ok {
				out[f[9]] = tcpListener{ip: ip, port: port}
			}
		}
	}
	return out
}

// parseProcNetAddr decodes "0100007F:1F90" (IPv4) or the 32-hex-digit IPv6
// form: the address is stored as host-order 32-bit words.
func parseProcNetAddr(s string) (net.IP, int, bool) {
	addr, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, false
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return nil, 0, false
	}
	raw, err := hex.DecodeString(addr)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil, 0, false
	}
	ip := make(net.IP, len(raw))
	for i := 0; i < len(raw); i += 4 {
		ip[i], ip[i+1], ip[i+2], ip[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	return ip, int(port), true
}

// processSocketInodes lists the socket inodes the given processes hold.
func processSocketInodes(pids []int) map[string]bool {
	out := map[string]bool{}
	for _, pid := range pids {
		dir := "/proc/" + strconv.Itoa(pid) + "/fd"
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			link, err := os.Readlink(dir + "/" + e.Name())
			if err != nil {
				continue
			}
			if inode, ok := strings.CutPrefix(link, "socket:["); ok {
				out[strings.TrimSuffix(inode, "]")] = true
			}
		}
	}
	return out
}

// hostNetCacheEntry remembers a container's ports for one combination of
// the VM's listening sockets and the container's processes.
type hostNetCacheEntry struct {
	sig   string
	ports map[int]string
}

// listenersSignature identifies the set of listening sockets.
func listenersSignature(listeners map[string]tcpListener) string {
	inodes := make([]string, 0, len(listeners))
	for inode := range listeners {
		inodes = append(inodes, inode)
	}
	slices.Sort(inodes)
	return strings.Join(inodes, ",")
}

// hostNetworkPorts returns the ports a host-network container listens on,
// with the address the port proxy must dial for each. Reading every fd of
// every process is the costly part (k3s runs hundreds): it is redone only
// when the listening sockets or the container's processes changed.
func hostNetworkPorts(taskPid uint32, listeners map[string]tcpListener, listenSig string, cache *hostNetCacheEntry) map[int]string {
	pids := cgroupPids(cgroupDir(int(taskPid)))
	if len(pids) == 0 {
		pids = []int{int(taskPid)}
	}
	var sig strings.Builder
	sig.WriteString(listenSig)
	sig.WriteByte('|')
	for _, p := range pids {
		sig.WriteString(strconv.Itoa(p))
		sig.WriteByte(',')
	}
	if cache != nil && cache.ports != nil && cache.sig == sig.String() {
		return cache.ports
	}
	out := map[int]string{}
	defer func() {
		if cache != nil {
			cache.sig, cache.ports = sig.String(), out
		}
	}()
	for inode := range processSocketInodes(pids) {
		l, ok := listeners[inode]
		if !ok {
			continue
		}
		target := "127.0.0.1"
		if !l.ip.IsUnspecified() && !l.ip.IsLoopback() {
			target = l.ip.String() // bound to one address only
		} else if l.ip.To4() == nil && l.ip.IsLoopback() {
			target = "::1"
		}
		if _, seen := out[l.port]; !seen || target == "127.0.0.1" {
			out[l.port] = target
		}
	}
	return out
}
