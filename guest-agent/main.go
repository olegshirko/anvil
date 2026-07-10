// guest-agent runs inside the Linux VM and accepts control commands over
// virtio-vsock. It is intentionally small and static-linked.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/mdlayher/vsock"
	"github.com/opencontainers/runtime-spec/specs-go"
)

const (
	cniVersion         = "0.4.0"
	defaultNetworkName = "bridge"
)

const (
	listenPort       = 1024
	dockerAPIPort    = 1025
	containerdSocket = "/run/containerd/containerd.sock"
	cniConfDir       = "/etc/cni/net.d"
	debounceDelay    = 150 * time.Millisecond
	pollInterval     = 500 * time.Millisecond
)

// Request and Response use a simple length-prefixed JSON protocol.
type Request struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args,omitempty"`
}

type Response struct {
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
	Status   string `json:"status,omitempty"`
}

// PortMapping describes a single exposed container port.
type PortMapping struct {
	Namespace     string `json:"namespace"`
	ContainerID   string `json:"container_id"`
	Name          string `json:"name,omitempty"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol,omitempty"`
	GuestIP       string `json:"guest_ip"`
}

// PortMapState is the full snapshot pushed to vz-runner.
type PortMapState struct {
	Mappings []PortMapping `json:"mappings"`
}

// execSpec holds the configuration for a created exec instance.
type execSpec struct {
	ID                string
	Namespace         string
	ContainerName     string
	ContainerDockerID string
	Cmd               []string
	Env               []string
	User              string
	WorkingDir        string
	AttachStdin       bool
	AttachStdout      bool
	AttachStderr      bool
	Tty               bool
	Privileged        bool

	mu       sync.Mutex
	running  bool
	exitCode int
}

func (s *execSpec) setExit(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.exitCode = code
}

func (s *execSpec) state() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running, s.exitCode
}

// execStore keeps pending and finished exec instances keyed by Docker exec ID.
type execStore struct {
	mu   sync.RWMutex
	byID map[string]*execSpec
}

func newExecStore() *execStore {
	return &execStore{byID: make(map[string]*execSpec)}
}

func (s *execStore) add(spec *execSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[spec.ID] = spec
}

func (s *execStore) get(id string) *execSpec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byID[id]
}

func newExecID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback to a timestamp-based ID if randomness fails.
		return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano()))))[:32]
	}
	return fmt.Sprintf("%x", b)
}

var execs = newExecStore()

func main() {
	log.SetPrefix("[guest-agent] ")

	if len(os.Args) > 1 && os.Args[1] == "cni-gen" {
		ns := "default"
		if len(os.Args) > 2 {
			ns = os.Args[2]
		}
		if err := generateCNIConfig(ns); err != nil {
			log.Fatalf("cni-gen: %v", err)
		}
		return
	}

	log.Printf("listening on vsock port %d", listenPort)

	scanner := newPortScanner()
	go scanner.run()

	// Docker API server on a separate vsock port so the existing control
	// channel stays untouched.
	go runDockerAPIServer()

	l, err := vsock.Listen(listenPort, nil)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer l.Close()

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go handle(conn, scanner)
	}
}

func handle(conn net.Conn, scanner *portScanner) {
	defer conn.Close()

	for {
		req, err := readRequest(conn)
		if err != nil {
			if err != io.EOF {
				log.Printf("read request: %v", err)
			}
			return
		}

		if req.Cmd == "subscribe_ports" {
			handleSubscribe(conn, scanner)
			return
		}

		resp := dispatch(req)
		if err := writeResponse(conn, resp); err != nil {
			log.Printf("write response: %v", err)
			return
		}
	}
}

func handleSubscribe(conn net.Conn, scanner *portScanner) {
	ch := scanner.subscribe()
	defer scanner.unsubscribe(ch)

	// Send full state immediately on connect (covers cold start and resume).
	if err := writePortState(conn, scanner.currentState()); err != nil {
		log.Printf("write initial port state: %v", err)
		return
	}

	for state := range ch {
		if err := writePortState(conn, state); err != nil {
			log.Printf("write port state: %v", err)
			return
		}
	}
}

func dispatch(req *Request) Response {
	switch req.Cmd {
	case "health", "status":
		return Response{Status: "ok"}
	case "exec":
		return runExec(req.Args)
	default:
		return Response{Error: fmt.Sprintf("unknown command: %s", req.Cmd), ExitCode: 1}
	}
}

func runExec(args []string) Response {
	if len(args) == 0 {
		return Response{Error: "no command", ExitCode: 1}
	}

	// For container creation commands, refuse to run if the requested host port
	// is already bound by another running container. This prevents silent
	// mis-routing when two projects map to the same localhost port.
	if isNerdctlRun(args) {
		if ports, err := parsePublishHostPorts(args); err != nil {
			return Response{Error: fmt.Sprintf("invalid publish spec: %v", err), ExitCode: 1}
		} else if len(ports) > 0 {
			if conflict, err := findHostPortConflict(ports); err != nil {
				return Response{Error: fmt.Sprintf("host port conflict check failed: %v", err), ExitCode: 1}
			} else if conflict != nil {
				return Response{
					Error: fmt.Sprintf(
						"host port %d already in use by %s/%s",
						conflict.HostPort, conflict.Namespace, conflict.Name,
					),
					ExitCode: 1,
				}
			}
		}
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Response{Error: err.Error(), ExitCode: 1}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Response{Error: err.Error(), ExitCode: 1}
	}
	if err := cmd.Start(); err != nil {
		return Response{Error: err.Error(), ExitCode: 1}
	}

	out, _ := io.ReadAll(stdout)
	errOut, _ := io.ReadAll(stderr)
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return Response{
		Stdout:   string(out),
		Stderr:   string(errOut),
		ExitCode: exitCode,
	}
}

// isNerdctlRun reports whether args is a nerdctl command that creates a
// container and may bind host ports.
func isNerdctlRun(args []string) bool {
	if len(args) == 0 || !strings.HasSuffix(args[0], "nerdctl") {
		return false
	}
	for _, a := range args[1:] {
		if a == "run" || a == "create" {
			return true
		}
	}
	return false
}

// parsePublishHostPorts extracts host ports from nerdctl -p/--publish flags.
// Supported forms: 8080:80, 127.0.0.1:8080:80, --publish=8080:80.
func parsePublishHostPorts(args []string) ([]int, error) {
	var ports []int
	for i := 1; i < len(args); i++ {
		a := args[i]
		var spec string
		switch {
		case a == "-p" || a == "--publish":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("missing value for %s", a)
			}
			spec = args[i+1]
			i++
		case strings.HasPrefix(a, "-p="):
			spec = a[len("-p="):]
		case strings.HasPrefix(a, "--publish="):
			spec = a[len("--publish="):]
		default:
			continue
		}
		if spec == "" {
			continue
		}
		// Strip optional protocol suffix.
		spec = strings.SplitN(spec, "/", 2)[0]
		parts := strings.Split(spec, ":")
		var hostPart string
		switch len(parts) {
		case 2:
			hostPart = parts[0]
		case 3:
			hostPart = parts[1]
		default:
			// Range or unusual format; skip rather than fail.
			continue
		}
		if strings.Contains(hostPart, "-") {
			// Port ranges are not checked.
			continue
		}
		p, err := strconv.Atoi(hostPart)
		if err != nil {
			return nil, fmt.Errorf("invalid host port %q: %w", hostPart, err)
		}
		ports = append(ports, p)
	}
	return ports, nil
}

// findHostPortConflict checks whether any requested host port is already used
// by a running container in any namespace. It returns the first conflict found.
func findHostPortConflict(ports []int) (*PortMapping, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil, err
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, err
	}

	requested := make(map[int]struct{})
	for _, p := range ports {
		requested[p] = struct{}{}
	}

	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			continue
		}
		for _, c := range containers {
			labels, err := c.Labels(nsCtx)
			if err != nil {
				continue
			}
			task, err := c.Task(nsCtx, nil)
			if err != nil {
				continue
			}
			status, err := task.Status(nsCtx)
			if err != nil || status.Status != "running" {
				continue
			}
			portsJSON := getNerdctlPortsLabel(c, nsCtx)
			if portsJSON == "" {
				continue
			}
			var mapped []cniPortMapping
			if err := json.Unmarshal([]byte(portsJSON), &mapped); err != nil {
				continue
			}
			for _, m := range mapped {
				if _, ok := requested[m.HostPort]; ok {
					return &PortMapping{
						Namespace:   ns,
						ContainerID: c.ID(),
						Name:        labels["nerdctl/name"],
						HostPort:    m.HostPort,
					}, nil
				}
			}
		}
	}
	return nil, nil
}

func readRequest(r io.Reader) (*Request, error) {
	br := bufio.NewReader(r)
	var length uint32
	if err := binary.Read(br, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	if length > 1<<20 {
		return nil, fmt.Errorf("request too large: %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

func writeResponse(w io.Writer, resp Response) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(body))); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func writePortState(w io.Writer, state PortMapState) error {
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(body))); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// cniPortMapping is the shape of the nerdctl/ports label.
type cniPortMapping struct {
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
	HostIP        string `json:"hostIP"`
}

// getNerdctlPortsLabel returns the nerdctl/ports JSON from container labels or,
// as a fallback, from the container's OCI spec annotations. nerdctl stores port
// mappings in spec annotations for containers attached to non-default networks.
func getNerdctlPortsLabel(c client.Container, nsCtx context.Context) string {
	if labels, err := c.Labels(nsCtx); err == nil && labels != nil {
		if v := labels["nerdctl/ports"]; v != "" {
			return v
		}
	}
	var spec *specs.Spec
	var err error
	if spec, err = c.Spec(nsCtx); err == nil && spec != nil {
		if v := spec.Annotations["nerdctl/ports"]; v != "" {
			return v
		}
	}
	return ""
}

type portScanner struct {
	mu          sync.Mutex
	current     []PortMapping
	subscribers map[chan PortMapState]struct{}
	seenConfigs map[string]struct{}
	guestIP     string
}

func newPortScanner() *portScanner {
	return &portScanner{
		subscribers: make(map[chan PortMapState]struct{}),
		seenConfigs: make(map[string]struct{}),
	}
}

func (s *portScanner) run() {
	var (
		cl            *client.Client
		debounceTimer *time.Timer
	)

	connect := func() *client.Client {
		for {
			c, err := client.New(containerdSocket)
			if err == nil {
				log.Printf("[scanner] connected to containerd")
				return c
			}
			log.Printf("[scanner] waiting for containerd: %v", err)
			time.Sleep(500 * time.Millisecond)
		}
	}

	cl = connect()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Trigger an immediate full scan once containerd is up.
	s.scanAndNotify(cl)

	for range ticker.C {
		// Reconnect if containerd went away.
		if _, err := cl.NamespaceService().List(context.Background()); err != nil {
			log.Printf("[scanner] containerd connection lost, reconnecting")
			cl = connect()
		}

		changed := s.scanAndNotify(cl)
		if changed {
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(debounceDelay, func() {
				s.pushCurrentState()
			})
		}
	}
}

func (s *portScanner) scanAndNotify(cl *client.Client) bool {
	state, err := s.buildState(cl)
	if err != nil {
		log.Printf("[scanner] build state: %v", err)
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if stateEqual(s.current, state.Mappings) {
		return false
	}
	s.current = state.Mappings
	return true
}

func (s *portScanner) pushCurrentState() {
	s.mu.Lock()
	state := s.currentStateLocked()
	chans := make([]chan PortMapState, 0, len(s.subscribers))
	for ch := range s.subscribers {
		chans = append(chans, ch)
	}
	s.mu.Unlock()

	for _, ch := range chans {
		select {
		case ch <- state:
		default:
			// Drop to slow subscribers; they will get the next update.
		}
	}
}

func (s *portScanner) currentState() PortMapState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentStateLocked()
}

func (s *portScanner) currentStateLocked() PortMapState {
	mappings := make([]PortMapping, len(s.current))
	copy(mappings, s.current)
	return PortMapState{Mappings: mappings}
}

func (s *portScanner) subscribe() chan PortMapState {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan PortMapState, 1)
	s.subscribers[ch] = struct{}{}
	return ch
}

func (s *portScanner) unsubscribe(ch chan PortMapState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subscribers, ch)
	close(ch)
}

func (s *portScanner) buildState(cl *client.Client) (PortMapState, error) {
	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return PortMapState{}, fmt.Errorf("list namespaces: %w", err)
	}

	guestIP := s.guestIP
	if guestIP == "" {
		guestIP = detectGuestIP()
		s.guestIP = guestIP
	}

	var mappings []PortMapping

	for _, ns := range nss {
		// Ensure the per-namespace default bridge exists before any container
		// in this namespace tries to use it.
		s.ensureCNIConfig(ns)

		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			log.Printf("[scanner] list containers in %s: %v", ns, err)
			continue
		}

		for _, c := range containers {
			labels, err := c.Labels(nsCtx)
			if err != nil {
				continue
			}

			// Skip containers that are not running.
			task, err := c.Task(nsCtx, nil)
			if err != nil {
				continue
			}
			status, err := task.Status(nsCtx)
			if err != nil || status.Status != "running" {
				continue
			}

			portsJSON := getNerdctlPortsLabel(c, nsCtx)
			if portsJSON == "" {
				continue
			}

			var ports []cniPortMapping
			if err := json.Unmarshal([]byte(portsJSON), &ports); err != nil {
				log.Printf("[scanner] parse ports for %s: %v", c.ID(), err)
				continue
			}

			for _, p := range ports {
				proto := p.Protocol
				if proto == "" {
					proto = "tcp"
				}
				mappings = append(mappings, PortMapping{
					Namespace:     ns,
					ContainerID:   c.ID(),
					Name:          labels["nerdctl/name"],
					HostPort:      p.HostPort,
					ContainerPort: p.ContainerPort,
					Protocol:      proto,
					GuestIP:       guestIP,
				})
			}
		}
	}

	sortPortMappings(mappings)
	return PortMapState{Mappings: mappings}, nil
}

func stateEqual(a, b []PortMapping) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortPortMappings(m []PortMapping) {
	sort.Slice(m, func(i, j int) bool {
		if m[i].Namespace != m[j].Namespace {
			return m[i].Namespace < m[j].Namespace
		}
		if m[i].ContainerID != m[j].ContainerID {
			return m[i].ContainerID < m[j].ContainerID
		}
		if m[i].HostPort != m[j].HostPort {
			return m[i].HostPort < m[j].HostPort
		}
		return m[i].ContainerPort < m[j].ContainerPort
	})
}

func detectGuestIP() string {
	iface, err := net.InterfaceByName("eth0")
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return ""
}

func (s *portScanner) ensureCNIConfig(ns string) {
	s.mu.Lock()
	if _, ok := s.seenConfigs[ns]; ok {
		s.mu.Unlock()
		return
	}
	s.seenConfigs[ns] = struct{}{}
	s.mu.Unlock()

	if err := generateCNIConfig(ns); err != nil {
		log.Printf("[scanner] ensure cni config for %s: %v", ns, err)
	}
}

func generateCNIConfig(ns string) error {
	// Docker clients expect the default network to be called "bridge".
	// Per-project networks keep their own name (e.g. project-a, compose-test_default).
	netName := ns
	if ns == "default" {
		netName = "bridge"
	}
	base := sanitizeCNIName(netName)
	path := filepath.Join(cniConfDir, "nerdctl-"+base+".conflist")

	bridge := "br-" + base
	if len(bridge) > 15 {
		bridge = bridge[:15]
	}

	octet := projectSubnetOctet(ns)
	subnet := fmt.Sprintf("10.10.%d.0/24", octet)
	gateway := fmt.Sprintf("10.10.%d.1", octet)

	labels := map[string]string{}
	if ns == "default" {
		labels["nerdctl/default-network"] = "true"
	}
	// Docker Compose names its default network `<project>_default`. Add the
	// labels Compose expects so it treats a pre-created CNI network as its own.
	if strings.HasSuffix(ns, "_default") {
		labels["com.docker.compose.project"] = strings.TrimSuffix(ns, "_default")
		labels["com.docker.compose.network"] = "default"
	}

	conf := map[string]interface{}{
		"cniVersion":    cniVersion,
		"name":          netName,
		"nerdctlID":     networkID(ns),
		"nerdctlLabels": labels,
		"plugins": []interface{}{
			map[string]interface{}{
				"type":        "bridge",
				"bridge":      bridge,
				"isGateway":   true,
				"ipMasq":      true,
				"hairpinMode": true,
				"ipam": map[string]interface{}{
					"type": "host-local",
					"ranges": []interface{}{
						[]interface{}{
							map[string]interface{}{
								"subnet":     subnet,
								"rangeStart": fmt.Sprintf("10.10.%d.2", octet),
								"rangeEnd":   fmt.Sprintf("10.10.%d.254", octet),
								"gateway":    gateway,
							},
						},
					},
					"routes": []interface{}{
						map[string]interface{}{"dst": "0.0.0.0/0"},
					},
				},
			},
			map[string]interface{}{
				"type":         "portmap",
				"capabilities": map[string]bool{"portMappings": true},
			},
			map[string]interface{}{
				"type":          "firewall",
				"ingressPolicy": "same-bridge",
			},
			map[string]interface{}{
				"type": "tuning",
			},
		},
	}

	data, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cni config for %s: %w", ns, err)
	}

	if err := os.MkdirAll(cniConfDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", cniConfDir, err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write cni config %s: %w", path, err)
	}
	log.Printf("[cni-gen] created CNI config for namespace %s: %s", ns, subnet)
	return nil
}

func projectSubnetOctet(project string) int {
	h := fnv.New32a()
	h.Write([]byte(project))
	return int(h.Sum32()%250) + 1
}

func networkID(project string) string {
	h := fnv.New128a()
	h.Write([]byte("anvil-" + project))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func sanitizeCNIName(ns string) string {
	var b strings.Builder
	for _, r := range ns {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune('-')
		}
	}
	base := b.String()
	if base == "" {
		base = "default"
	}
	// Limit file name length.
	if len(base) > 64 {
		base = base[:64]
	}
	return base
}

// ---------------------------------------------------------------------------
// Docker API compatibility layer (M7)
// ---------------------------------------------------------------------------

const dockerAPIVersion = "1.24"

// dockerPort matches the Docker API port binding shape.
type dockerPort struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}

// dockerContainerSummary matches the JSON returned by GET /containers/json.
type dockerContainerSummary struct {
	Id      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Command string            `json:"Command"`
	Created int64             `json:"Created"`
	Ports   []dockerPort      `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
}

// dockerContainerInspect is a minimal subset of GET /containers/{id}/json.
type dockerContainerInspect struct {
	Id              string                `json:"Id"`
	Name            string                `json:"Name"`
	Image           string                `json:"Image"`
	State           dockerContainerState  `json:"State"`
	Config          dockerContainerConfig `json:"Config"`
	NetworkSettings dockerNetworkSettings `json:"NetworkSettings"`
}

type dockerContainerState struct {
	Status  string `json:"Status"`
	Running bool   `json:"Running"`
}

type dockerContainerConfig struct {
	Labels map[string]string `json:"Labels"`
	Image  string            `json:"Image"`
}

type dockerNetworkSettings struct {
	IPAddress string `json:"IPAddress"`
}

// dockerImageSummary matches the JSON returned by GET /images/json.
type dockerImageSummary struct {
	Id          string            `json:"Id"`
	RepoTags    []string          `json:"RepoTags"`
	RepoDigests []string          `json:"RepoDigests"`
	Created     int64             `json:"Created"`
	Size        int64             `json:"Size"`
	VirtualSize int64             `json:"VirtualSize"`
	Labels      map[string]string `json:"Labels"`
	ParentId    string            `json:"ParentId"`
	Containers  int               `json:"Containers"`
}

// dockerNetwork matches the JSON returned by GET /networks.
type dockerNetwork struct {
	Id         string            `json:"Id"`
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Scope      string            `json:"Scope"`
	Created    string            `json:"Created"`
	Internal   bool              `json:"Internal"`
	Attachable bool              `json:"Attachable"`
	Ingress    bool              `json:"Ingress"`
	IPAM       dockerIPAM        `json:"IPAM"`
	Options    map[string]string `json:"Options"`
	Labels     map[string]string `json:"Labels"`
}

// dockerIPAM is the IPAM configuration inside dockerNetwork.
type dockerIPAM struct {
	Driver  string             `json:"Driver"`
	Config  []dockerIPAMConfig `json:"Config"`
	Options map[string]string  `json:"Options"`
}

// dockerIPAMConfig is a single IPAM pool config.
type dockerIPAMConfig struct {
	Subnet string `json:"Subnet,omitempty"`
}

// dockerVolume matches the JSON returned by GET /volumes and /volumes/{name}.
type dockerVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
	Scope      string            `json:"Scope"`
}

// dockerVolumeList is the response for GET /volumes.
type dockerVolumeList struct {
	Volumes  []dockerVolume `json:"Volumes"`
	Warnings []string       `json:"Warnings"`
}

// nerdctlImage is the shape of `nerdctl images --format json` output.
type nerdctlImage struct {
	CreatedAt  string `json:"CreatedAt"`
	Digest     string `json:"Digest"`
	ID         string `json:"ID"`
	Repository string `json:"Repository"`
	Tag        string `json:"Tag"`
	Name       string `json:"Name"`
	Size       string `json:"Size"`
	BlobSize   string `json:"BlobSize"`
	Platform   string `json:"Platform"`
}

// dockerID returns a deterministic 64-hex Docker-compatible ID for a
// containerd container. It is stable across restarts because it is derived
// from the namespace and containerd ID.
func dockerID(namespace, containerID string) string {
	h := sha256.Sum256([]byte(namespace + "/" + containerID))
	return fmt.Sprintf("%x", h)[:64]
}

func dockerState(status string) string {
	switch status {
	case "running":
		return "running"
	case "stopped":
		return "exited"
	case "paused":
		return "paused"
	default:
		return "created"
	}
}

func dockerStatus(status string) string {
	switch status {
	case "running":
		return "running"
	case "stopped":
		return "exited"
	case "paused":
		return "paused"
	default:
		return "created"
	}
}

// listDockerContainers scans all containerd namespaces and returns a
// Docker-compatible summary for each container.
func listDockerContainers() ([]dockerContainerSummary, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	var result []dockerContainerSummary
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			log.Printf("[docker-api] list containers in %s: %v", ns, err)
			continue
		}

		for _, c := range containers {
			info, err := c.Info(nsCtx)
			if err != nil {
				continue
			}

			labels := info.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			name := labels["nerdctl/name"]
			if name == "" {
				name = c.ID()
			}

			state := "created"
			status := "created"
			task, err := c.Task(nsCtx, nil)
			if err == nil {
				st, err := task.Status(nsCtx)
				if err == nil {
					state = dockerState(string(st.Status))
					status = dockerStatus(string(st.Status))
				}
			}

			var ports []dockerPort
			if portsJSON := getNerdctlPortsLabel(c, nsCtx); portsJSON != "" {
				var pm []cniPortMapping
				if err := json.Unmarshal([]byte(portsJSON), &pm); err == nil {
					for _, p := range pm {
						proto := p.Protocol
						if proto == "" {
							proto = "tcp"
						}
						hostIP := p.HostIP
						if hostIP == "" {
							hostIP = "0.0.0.0"
						}
						ports = append(ports, dockerPort{
							IP:          hostIP,
							PrivatePort: p.ContainerPort,
							PublicPort:  p.HostPort,
							Type:        proto,
						})
					}
				}
			}

			imageName := info.Image
			if img, err := c.Image(nsCtx); err == nil && img != nil {
				imageName = img.Name()
			}

			result = append(result, dockerContainerSummary{
				Id:      dockerID(ns, c.ID()),
				Names:   []string{"/" + name},
				Image:   imageName,
				ImageID: info.Image,
				Command: "", // not trivial to extract from OCI spec; left empty
				Created: info.CreatedAt.Unix(),
				Ports:   ports,
				Labels:  labels,
				State:   state,
				Status:  status,
			})
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Id < result[j].Id
	})
	return result, nil
}

// parseHumanSize converts nerdctl size strings like "182.2 MiB" to bytes.
func parseHumanSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0
	}
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return 0
	}
	val, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	unit := parts[1]
	switch {
	case strings.HasPrefix(unit, "KiB"):
		return int64(val * 1024)
	case strings.HasPrefix(unit, "MiB"):
		return int64(val * 1024 * 1024)
	case strings.HasPrefix(unit, "GiB"):
		return int64(val * 1024 * 1024 * 1024)
	case strings.HasPrefix(unit, "TiB"):
		return int64(val * 1024 * 1024 * 1024 * 1024)
	case strings.HasPrefix(unit, "B"):
		return int64(val)
	default:
		return 0
	}
}

// listDockerImages collects images from all namespaces and returns a
// Docker-compatible summary, deduplicating by image ID.
func listDockerImages() ([]dockerImageSummary, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	seen := make(map[string]struct{})
	var result []dockerImageSummary

	for _, ns := range nss {
		stdout, stderr, code, err := runNerdctl(ns, "images", "--format", "json")
		if err != nil || code != 0 {
			log.Printf("[docker-api] list images in %s: %s%s", ns, stdout, stderr)
			continue
		}
		for _, line := range strings.Split(stdout, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var img nerdctlImage
			if err := json.Unmarshal([]byte(line), &img); err != nil {
				log.Printf("[docker-api] parse image line %q: %v", line, err)
				continue
			}
			if img.ID == "" {
				continue
			}
			if _, ok := seen[img.ID]; ok {
				continue
			}
			seen[img.ID] = struct{}{}

			created := int64(0)
			if t, err := time.Parse("2006-01-02 15:04:05 +0000 UTC", img.CreatedAt); err == nil {
				created = t.Unix()
			}

			size := parseHumanSize(img.Size)
			if size == 0 {
				size = parseHumanSize(img.BlobSize)
			}

			repoTag := ""
			if img.Repository != "" && img.Tag != "" {
				repoTag = img.Repository + ":" + img.Tag
			} else if img.Name != "" {
				repoTag = img.Name
			}

			repoDigest := ""
			if img.Digest != "" && repoTag != "" {
				// Docker digest format: repo@digest
				repo := repoTag
				if idx := strings.Index(repoTag, ":"); idx != -1 {
					repo = repoTag[:idx]
				}
				repoDigest = repo + "@" + img.Digest
			}

			dockerID := img.ID
			if !strings.HasPrefix(dockerID, "sha256:") {
				dockerID = "sha256:" + dockerID
			}

			var repoTags []string
			if repoTag != "" {
				repoTags = []string{repoTag}
			}
			var repoDigests []string
			if repoDigest != "" {
				repoDigests = []string{repoDigest}
			}

			result = append(result, dockerImageSummary{
				Id:          dockerID,
				RepoTags:    repoTags,
				RepoDigests: repoDigests,
				Created:     created,
				Size:        size,
				VirtualSize: size,
				Labels:      map[string]string{},
				ParentId:    "",
				Containers:  -1,
			})
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Id < result[j].Id
	})
	return result, nil
}

// findImageNamespace returns the containerd namespace that contains an image
// matching the given reference (name or name:tag). The empty string is returned
// if no matching image is found.
func findImageNamespace(ref string) string {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return ""
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return ""
	}

	// Normalize ref: strip tag if present.
	repo := ref
	if idx := strings.LastIndex(ref, ":"); idx != -1 {
		repo = ref[:idx]
	}

	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		images, err := cl.ListImages(nsCtx)
		if err != nil {
			continue
		}
		for _, img := range images {
			name := img.Name()
			if name == ref || name == repo {
				return ns
			}
			// Also match short names like "nginx" against "docker.io/library/nginx".
			short := name
			if strings.HasPrefix(name, "docker.io/library/") {
				short = strings.TrimPrefix(name, "docker.io/library/")
			} else if strings.HasPrefix(name, "docker.io/") {
				short = strings.TrimPrefix(name, "docker.io/")
			}
			if short == ref || short == repo {
				return ns
			}
		}
	}
	return ""
}

// tagDockerImage tags an image using nerdctl.
func tagDockerImage(source, target string) error {
	ns := findImageNamespace(source)
	if ns == "" {
		ns = "default"
	}
	stdout, stderr, code, err := runNerdctl(ns, "tag", source, target)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl tag failed (%d): %s%s", code, stdout, stderr)
	}
	return nil
}

// removeDockerImage removes an image using nerdctl.
func removeDockerImage(name string) error {
	ns := findImageNamespace(name)
	if ns == "" {
		ns = "default"
	}
	stdout, stderr, code, err := runNerdctl(ns, "rmi", name)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl rmi failed (%d): %s%s", code, stdout, stderr)
	}
	return nil
}

// inspectDockerImage returns a Docker-compatible image inspect payload.
func inspectDockerImage(name string) (map[string]interface{}, error) {
	ns := findImageNamespace(name)
	if ns == "" {
		ns = "default"
	}
	stdout, stderr, code, err := runNerdctl(ns, "image", "inspect", "--format", "json", name)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("nerdctl image inspect failed (%d): %s%s", code, stdout, stderr)
	}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var info struct {
			ID          string   `json:"ID"`
			RepoTags    []string `json:"RepoTags"`
			RepoDigests []string `json:"RepoDigests"`
			Comment     string   `json:"Comment"`
			Created     string   `json:"Created"`
			Author      string   `json:"Author"`
			Config      struct {
				Cmd          []string            `json:"Cmd"`
				Entrypoint   []string            `json:"Entrypoint"`
				Env          []string            `json:"Env"`
				ExposedPorts map[string]struct{} `json:"ExposedPorts"`
				Labels       map[string]string   `json:"Labels"`
				WorkingDir   string              `json:"WorkingDir"`
				User         string              `json:"User"`
				StopSignal   string              `json:"StopSignal"`
			} `json:"Config"`
			RootFS struct {
				Type   string   `json:"Type"`
				Layers []string `json:"Layers"`
			} `json:"RootFS"`
		}
		if err := json.Unmarshal([]byte(line), &info); err != nil {
			continue
		}
		if info.ID == "" {
			continue
		}
		return map[string]interface{}{
			"Id":          info.ID,
			"RepoTags":    info.RepoTags,
			"RepoDigests": info.RepoDigests,
			"Comment":     info.Comment,
			"Created":     info.Created,
			"Author":      info.Author,
			"Config":      info.Config,
			"RootFS":      info.RootFS,
			"Size":        0,
			"VirtualSize": 0,
			"GraphDriver": map[string]interface{}{"Data": map[string]interface{}{}, "Name": "overlayfs"},
		}, nil
	}
	return nil, fmt.Errorf("No such image: %s", name)
}

// pushDockerImage pushes an image and streams progress lines to w.
func pushDockerImage(name string, w io.Writer) error {
	ns := findImageNamespace(name)
	if ns == "" {
		ns = "default"
	}
	cmd := exec.Command("/opt/containerd/bin/nerdctl", "-n", ns, "push", name)
	cmd.Env = append(os.Environ(), "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Wait()

	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return nil
}

// --- Networks ---

// nerdctlNetworkLs is the shape of `nerdctl network ls --format json` output.
type nerdctlNetworkLs struct {
	ID     string `json:"ID"`
	Name   string `json:"Name"`
	Labels string `json:"Labels"`
}

// nerdctlNetworkInspect is the shape of `nerdctl network inspect --format json` output.
type nerdctlNetworkInspect struct {
	Name    string `json:"Name"`
	Id      string `json:"Id"`
	Driver  string `json:"Driver"`
	Scope   string `json:"Scope"`
	Created string `json:"Created"`
	IPAM    struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
	Labels  interface{}       `json:"Labels"`
	Options map[string]string `json:"Options"`
}

// listDockerNetworks returns networks from all namespaces.
func listDockerNetworks() ([]dockerNetwork, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	// Always include the default namespace even if it has no containers/images,
	// because networks and volumes may live there.
	hasDefault := false
	for _, ns := range nss {
		if ns == "default" {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		nss = append([]string{"default"}, nss...)
	}

	seen := map[string]struct{}{}
	var result []dockerNetwork
	for _, ns := range nss {
		stdout, stderr, code, err := runNerdctl(ns, "network", "ls", "--format", "json")
		if err != nil || code != 0 {
			log.Printf("[docker-api] network ls in %s: %d %s%s", ns, code, stdout, stderr)
			continue
		}
		for _, line := range strings.Split(stdout, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var nw nerdctlNetworkLs
			if err := json.Unmarshal([]byte(line), &nw); err != nil {
				continue
			}
			if nw.ID == "" {
				continue
			}
			if _, ok := seen[nw.ID]; ok {
				continue
			}
			seen[nw.ID] = struct{}{}
			result = append(result, dockerNetwork{
				Id:      nw.ID,
				Name:    nw.Name,
				Driver:  "bridge",
				Scope:   "local",
				Created: time.Now().UTC().Format(time.RFC3339),
				IPAM:    dockerIPAM{Driver: "default"},
				Options: map[string]string{},
				Labels:  map[string]string{},
			})
		}
	}
	return result, nil
}

// inspectDockerNetwork returns a network by name or ID.
func inspectDockerNetwork(name string) (*dockerNetwork, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	hasDefault := false
	for _, ns := range nss {
		if ns == "default" {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		nss = append([]string{"default"}, nss...)
	}

	for _, ns := range nss {
		stdout, stderr, code, err := runNerdctl(ns, "network", "inspect", "--format", "json", name)
		if err != nil || code != 0 {
			log.Printf("[docker-api] network inspect in %s: %d %s%s", ns, code, stdout, stderr)
			continue
		}
		for _, line := range strings.Split(stdout, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var nw nerdctlNetworkInspect
			if err := json.Unmarshal([]byte(line), &nw); err != nil {
				continue
			}
			dn := nerdctlNetworkInspectToDocker(nw)
			if dn.Name == name || strings.HasPrefix(dn.Id, name) {
				return &dn, nil
			}
		}
	}
	return nil, fmt.Errorf("No such network: %s", name)
}

// createDockerNetwork creates a network in the default namespace.
// If a CNI config already exists for the requested name (e.g. pre-generated by
// the guest-agent scanner for deterministic per-project subnets), it is reused
// instead of being overwritten.
func createDockerNetwork(req dockerNetworkCreateRequest) (*dockerNetwork, error) {
	ns := req.Namespace
	if ns == "" {
		ns = "default"
	}

	// If the scanner already created a deterministic config for this network,
	// return it without calling nerdctl network create, which would overwrite
	// the deterministic subnet with an auto-generated one.
	if existing, err := inspectDockerNetwork(req.Name); err == nil && existing != nil {
		return existing, nil
	}

	args := []string{"network", "create"}
	if req.Driver != "" {
		args = append(args, "-d", req.Driver)
	}
	if req.IPAM.Driver != "" || len(req.IPAM.Config) > 0 {
		args = append(args, "--ipam-driver", defaultString(req.IPAM.Driver, "default"))
		for _, cfg := range req.IPAM.Config {
			if cfg.Subnet != "" {
				args = append(args, "--subnet", cfg.Subnet)
			}
		}
	}
	for k, v := range req.Options {
		args = append(args, "-o", k+"="+v)
	}
	for k, v := range req.Labels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, req.Name)

	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("nerdctl network create failed (%d): %s%s", code, stdout, stderr)
	}
	// stdout is the network ID; re-inspect to return full payload.
	nwID := strings.TrimSpace(stdout)
	stdout, stderr, code, err = runNerdctl(ns, "network", "inspect", "--format", "json", nwID)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("nerdctl network inspect after create failed (%d): %s%s", code, stdout, stderr)
	}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var nw nerdctlNetworkInspect
		if err := json.Unmarshal([]byte(line), &nw); err != nil {
			continue
		}
		dn := nerdctlNetworkInspectToDocker(nw)
		return &dn, nil
	}
	return nil, fmt.Errorf("network created but inspect returned no data")
}

// removeDockerNetwork removes a network by name or ID.
func removeDockerNetwork(name string) error {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	hasDefault := false
	for _, ns := range nss {
		if ns == "default" {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		nss = append([]string{"default"}, nss...)
	}

	for _, ns := range nss {
		stdout, stderr, code, err := runNerdctl(ns, "network", "rm", name)
		if err == nil && code == 0 {
			return nil
		}
		_ = stdout
		_ = stderr
	}
	return fmt.Errorf("No such network: %s", name)
}

// nerdctlNetworkInspectToDocker converts nerdctl network inspect JSON to Docker API shape.
func nerdctlNetworkInspectToDocker(nw nerdctlNetworkInspect) dockerNetwork {
	ipam := dockerIPAM{Driver: "default"}
	for _, cfg := range nw.IPAM.Config {
		ipam.Config = append(ipam.Config, dockerIPAMConfig{Subnet: cfg.Subnet})
	}
	created := nw.Created
	if created == "" {
		created = time.Now().UTC().Format(time.RFC3339)
	}
	labels := map[string]string{}
	switch m := nw.Labels.(type) {
	case map[string]interface{}:
		for k, v := range m {
			if s, ok := v.(string); ok {
				labels[k] = s
			}
		}
	case map[string]string:
		for k, v := range m {
			labels[k] = v
		}
	}
	options := nw.Options
	if options == nil {
		options = map[string]string{}
	}
	if ipam.Options == nil {
		ipam.Options = map[string]string{}
	}
	return dockerNetwork{
		Id:         nw.Id,
		Name:       nw.Name,
		Driver:     defaultString(nw.Driver, "bridge"),
		Scope:      defaultString(nw.Scope, "local"),
		Created:    created,
		Internal:   false,
		Attachable: false,
		Ingress:    false,
		IPAM:       ipam,
		Options:    options,
		Labels:     labels,
	}
}

// dockerNetworkCreateRequest mirrors Docker's POST /networks/create body.
type dockerNetworkCreateRequest struct {
	Name      string            `json:"Name"`
	Driver    string            `json:"Driver"`
	Scope     string            `json:"Scope"`
	IPAM      dockerIPAM        `json:"IPAM"`
	Options   map[string]string `json:"Options"`
	Labels    map[string]string `json:"Labels"`
	Internal  bool              `json:"Internal"`
	Namespace string            `json:"-"`
}

// --- Volumes ---

// nerdctlVolumeLs is the shape of `nerdctl volume ls --format json` output.
type nerdctlVolumeLs struct {
	Name       string `json:"Name"`
	Driver     string `json:"Driver"`
	Mountpoint string `json:"Mountpoint"`
	Labels     string `json:"Labels"`
	Scope      string `json:"Scope"`
	Size       string `json:"Size"`
}

// nerdctlVolumeInspect is the shape of `nerdctl volume inspect --format json` output.
type nerdctlVolumeInspect struct {
	Name       string            `json:"Name"`
	Mountpoint string            `json:"Mountpoint"`
	Labels     map[string]string `json:"Labels"`
}

// listDockerVolumes returns volumes from the default namespace.
func listDockerVolumes() ([]dockerVolume, error) {
	stdout, stderr, code, err := runNerdctl("default", "volume", "ls", "--format", "json")
	if err != nil || code != 0 {
		return nil, fmt.Errorf("nerdctl volume ls failed (%d): %s%s", code, stdout, stderr)
	}

	var result []dockerVolume
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var vol nerdctlVolumeLs
		if err := json.Unmarshal([]byte(line), &vol); err != nil {
			continue
		}
		result = append(result, dockerVolume{
			Name:       vol.Name,
			Driver:     defaultString(vol.Driver, "local"),
			Mountpoint: vol.Mountpoint,
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
			Labels:     map[string]string{},
			Options:    map[string]string{},
			Scope:      defaultString(vol.Scope, "local"),
		})
	}
	return result, nil
}

// inspectDockerVolume returns a volume by name from the default namespace.
func inspectDockerVolume(name string) (*dockerVolume, error) {
	stdout, stderr, code, err := runNerdctl("default", "volume", "inspect", "--format", "json", name)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("nerdctl volume inspect failed (%d): %s%s", code, stdout, stderr)
	}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var vol nerdctlVolumeInspect
		if err := json.Unmarshal([]byte(line), &vol); err != nil {
			continue
		}
		if vol.Name == name {
			labels := vol.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			dv := dockerVolume{
				Name:       vol.Name,
				Driver:     "local",
				Mountpoint: vol.Mountpoint,
				CreatedAt:  time.Now().UTC().Format(time.RFC3339),
				Labels:     labels,
				Options:    map[string]string{},
				Scope:      "local",
			}
			return &dv, nil
		}
	}
	return nil, fmt.Errorf("No such volume: %s", name)
}

// createDockerVolume creates a volume in the default namespace.
// nerdctl volume create only supports --label, so Driver/Options are ignored.
func createDockerVolume(req dockerVolumeCreateRequest) (*dockerVolume, error) {
	ns := "default"
	args := []string{"volume", "create"}
	for k, v := range req.Labels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, req.Name)

	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 || strings.Contains(stderr, "fatal") {
		return nil, fmt.Errorf("nerdctl volume create failed (%d): %s%s", code, stdout, stderr)
	}
	volName := strings.TrimSpace(stdout)
	if volName == "" {
		volName = req.Name
	}
	return inspectDockerVolume(volName)
}

// removeDockerVolume removes a volume by name from the default namespace.
func removeDockerVolume(name string) error {
	stdout, stderr, code, err := runNerdctl("default", "volume", "rm", name)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl volume rm failed (%d): %s%s", code, stdout, stderr)
	}
	return nil
}

// dockerVolumeCreateRequest mirrors Docker's POST /volumes/create body.
type dockerVolumeCreateRequest struct {
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	Options map[string]string `json:"DriverOpts"`
	Labels  map[string]string `json:"Labels"`
}

// defaultString returns fallback if s is empty.
func defaultString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// findContainerByName returns the Docker ID of a container with the given name
// in the given namespace, or an empty string if none exists.
func findContainerByName(ns, name string) (string, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return "", err
	}
	defer cl.Close()
	nsCtx := namespaces.WithNamespace(context.Background(), ns)
	containers, err := cl.Containers(nsCtx)
	if err != nil {
		return "", err
	}
	for _, c := range containers {
		labels, err := c.Labels(nsCtx)
		if err != nil {
			continue
		}
		if labels["nerdctl/name"] == name {
			return dockerID(ns, c.ID()), nil
		}
	}
	return "", nil
}

// findContainerByDockerID looks up a container by full or prefix Docker ID
// across all namespaces. It returns the container, its namespace, and an error.
func findContainerByDockerID(prefix string) (client.Container, string, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil, "", fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("list namespaces: %w", err)
	}

	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			continue
		}
		for _, c := range containers {
			if strings.HasPrefix(dockerID(ns, c.ID()), prefix) {
				return c, ns, nil
			}
		}
	}
	return nil, "", fmt.Errorf("No such container: %s", prefix)
}

// inspectDockerContainer returns a minimal inspect payload for a container.
// The lookup accepts a Docker ID prefix, a container name, or a leading-slash
// name as returned by `docker ps`.
func inspectDockerContainer(prefix string) (*dockerContainerInspect, error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return nil, fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			continue
		}
		for _, c := range containers {
			info, err := c.Info(nsCtx)
			if err != nil {
				continue
			}

			labels := info.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			name := labels["nerdctl/name"]
			if name == "" {
				name = c.ID()
			}

			// Match by Docker ID prefix or by name (with or without leading slash).
			matched := strings.HasPrefix(dockerID(ns, c.ID()), prefix)
			if !matched {
				searchName := prefix
				if strings.HasPrefix(searchName, "/") {
					searchName = searchName[1:]
				}
				if name == searchName {
					matched = true
				}
			}
			if !matched {
				continue
			}

			status := "created"
			running := false
			task, err := c.Task(nsCtx, nil)
			if err == nil {
				st, err := task.Status(nsCtx)
				if err == nil {
					status = dockerStatus(string(st.Status))
					running = st.Status == "running"
				}
			}

			imageName := info.Image
			if img, err := c.Image(nsCtx); err == nil && img != nil {
				imageName = img.Name()
			}

			return &dockerContainerInspect{
				Id:    dockerID(ns, c.ID()),
				Name:  "/" + name,
				Image: imageName,
				State: dockerContainerState{
					Status:  status,
					Running: running,
				},
				Config: dockerContainerConfig{
					Labels: labels,
					Image:  imageName,
				},
				NetworkSettings: dockerNetworkSettings{
					IPAddress: detectGuestIP(),
				},
			}, nil
		}
	}
	return nil, fmt.Errorf("No such container: %s", prefix)
}

// ---------------------------------------------------------------------------
// Docker API mutating endpoints (M7 L2)
// ---------------------------------------------------------------------------

// dockerCreateRequest mirrors the minimal parts of Docker's container creation body.
type dockerCreateRequest struct {
	Hostname     string            `json:"Hostname"`
	Domainname   string            `json:"Domainname"`
	User         string            `json:"User"`
	AttachStdin  bool              `json:"AttachStdin"`
	AttachStdout bool              `json:"AttachStdout"`
	AttachStderr bool              `json:"AttachStderr"`
	Tty          bool              `json:"Tty"`
	OpenStdin    bool              `json:"OpenStdin"`
	StdinOnce    bool              `json:"StdinOnce"`
	Env          []string          `json:"Env"`
	Cmd          []string          `json:"Cmd"`
	Image        string            `json:"Image"`
	Labels       map[string]string `json:"Labels"`
	HostConfig   dockerHostConfig  `json:"HostConfig"`
}

type dockerHostConfig struct {
	Binds           []string                    `json:"Binds"`
	NetworkMode     string                      `json:"NetworkMode"`
	PortBindings    map[string][]dockerHostPort `json:"PortBindings"`
	RestartPolicy   dockerRestartPolicy         `json:"RestartPolicy"`
	AutoRemove      bool                        `json:"AutoRemove"`
	Privileged      bool                        `json:"Privileged"`
	PublishAllPorts bool                        `json:"PublishAllPorts"`
}

type dockerHostPort struct {
	HostIp   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerRestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

type dockerCreateResponse struct {
	Id       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

type dockerWaitResponse struct {
	StatusCode int `json:"StatusCode"`
}

// resolveDockerID maps a Docker ID prefix or container name to a containerd ID
// and its namespace. It returns an error if no unique match is found.
func resolveDockerID(prefix string) (ns, containerdID, name string, err error) {
	cl, err := client.New(containerdSocket)
	if err != nil {
		return "", "", "", fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("list namespaces: %w", err)
	}

	type match struct {
		ns, id, name string
	}
	var matches []match
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		containers, err := cl.Containers(nsCtx)
		if err != nil {
			continue
		}
		for _, c := range containers {
			info, err := c.Info(nsCtx)
			if err != nil {
				continue
			}
			labels := info.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			cname := labels["nerdctl/name"]
			if cname == "" {
				cname = c.ID()
			}

			if strings.HasPrefix(dockerID(ns, c.ID()), prefix) {
				matches = append(matches, match{ns, c.ID(), cname})
				continue
			}
			searchName := prefix
			if strings.HasPrefix(searchName, "/") {
				searchName = searchName[1:]
			}
			if cname == searchName {
				matches = append(matches, match{ns, c.ID(), cname})
			}
		}
	}

	if len(matches) == 0 {
		return "", "", "", fmt.Errorf("No such container: %s", prefix)
	}
	if len(matches) > 1 {
		return "", "", "", fmt.Errorf("multiple containers match %s", prefix)
	}
	return matches[0].ns, matches[0].id, matches[0].name, nil
}

// runNerdctl runs a nerdctl command in the given namespace and returns output.
func runNerdctl(ns string, args ...string) (stdout, stderr string, exitCode int, err error) {
	cmdArgs := append([]string{"-n", ns}, args...)
	cmd := exec.Command("/opt/containerd/bin/nerdctl", cmdArgs...)
	cmd.Env = append(os.Environ(), "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	return outBuf.String(), errBuf.String(), exitCode, err
}

// isNerdctlContainerRunning reports whether the named container is currently
// running, using nerdctl inspect.
func isNerdctlContainerRunning(ns, name string) bool {
	stdout, _, code, err := runNerdctl(ns, "inspect", "--format", "json", name)
	if err != nil || code != 0 {
		return false
	}
	var infos []struct {
		State struct {
			Running bool   `json:"Running"`
			Status  string `json:"Status"`
		} `json:"State"`
	}
	if err := json.Unmarshal([]byte(stdout), &infos); err != nil || len(infos) == 0 {
		return false
	}
	return infos[0].State.Running || infos[0].State.Status == "running"
}

// createDockerContainer creates a container via nerdctl and returns its Docker ID.
func createDockerContainer(req dockerCreateRequest, name string) (string, error) {
	ns := "default"
	networkMode := req.HostConfig.NetworkMode
	if networkMode != "" && networkMode != "default" && networkMode != "bridge" {
		ns = networkMode
	}

	// Docker refuses duplicate names; mimic that to avoid ambiguous lookups later.
	if name != "" {
		if existing, err := findContainerByName(ns, name); err == nil && existing != "" {
			return "", fmt.Errorf("Conflict. The container name \"/%s\" is already in use by container \"%s\". You have to remove (or rename) that container to be able to reuse that name.", name, existing)
		}
	}

	args := []string{"create"}
	if networkMode != "" && networkMode != "default" {
		args = append(args, "--net", networkMode)
	}
	// json-file logger makes `nerdctl logs -f` reliable enough for attach.
	args = append(args, "--log-driver", "json-file")
	if name != "" {
		args = append(args, "--name", name)
	}
	if req.Hostname != "" {
		args = append(args, "--hostname", req.Hostname)
	}
	for _, e := range req.Env {
		args = append(args, "-e", e)
	}
	for k, v := range req.Labels {
		args = append(args, "-l", k+"="+v)
	}
	if req.HostConfig.AutoRemove {
		args = append(args, "--rm")
	}
	if req.HostConfig.Privileged {
		args = append(args, "--privileged")
	}
	if req.HostConfig.RestartPolicy.Name != "" {
		args = append(args, "--restart", req.HostConfig.RestartPolicy.Name)
	}

	// Port bindings: containerPort/proto -> [{HostIp, HostPort}]
	for cportSpec, hostPorts := range req.HostConfig.PortBindings {
		proto := "tcp"
		parts := strings.SplitN(cportSpec, "/", 2)
		cport := parts[0]
		if len(parts) == 2 {
			proto = parts[1]
		}
		for _, hp := range hostPorts {
			host := hp.HostPort
			if host == "" {
				continue
			}
			spec := host + ":" + cport
			if proto != "tcp" {
				spec += "/" + proto
			}
			args = append(args, "-p", spec)
		}
	}

	args = append(args, req.Image)
	args = append(args, req.Cmd...)

	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 {
		return "", fmt.Errorf("nerdctl create failed (%d): %s%s", code, stdout, stderr)
	}

	// nerdctl create prints the container name/ID to stdout.
	createdName := strings.TrimSpace(stdout)
	if createdName == "" {
		return "", fmt.Errorf("nerdctl create returned empty ID")
	}

	// Look up the container by the ID nerdctl printed, then by requested name.
	cl, err := client.New(containerdSocket)
	if err != nil {
		return "", fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nsCtx := namespaces.WithNamespace(ctx, ns)

	// nerdctl create usually prints the containerd ID; try loading it directly.
	if c, err := cl.LoadContainer(nsCtx, createdName); err == nil {
		return dockerID(ns, c.ID()), nil
	}

	containers, err := cl.Containers(nsCtx)
	if err != nil {
		return "", fmt.Errorf("list containers: %w", err)
	}
	searchName := name
	if searchName == "" {
		searchName = createdName
	}
	for _, c := range containers {
		labels, err := c.Labels(nsCtx)
		if err != nil {
			continue
		}
		if labels["nerdctl/name"] == searchName {
			return dockerID(ns, c.ID()), nil
		}
	}
	return "", fmt.Errorf("could not find created container %s", searchName)
}

// startDockerContainer starts a container by Docker ID or name.
func startDockerContainer(id string) error {
	ns, containerdID, _, err := resolveDockerID(id)
	if err != nil {
		return err
	}
	stdout, stderr, code, err := runNerdctl(ns, "start", containerdID)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl start failed (%d): %s%s", code, stdout, stderr)
	}
	return nil
}

// stopDockerContainer stops a container by Docker ID or name.
func stopDockerContainer(id string, timeout int) error {
	ns, containerdID, _, err := resolveDockerID(id)
	if err != nil {
		return err
	}
	args := []string{"stop"}
	if timeout > 0 {
		args = append(args, "-t", strconv.Itoa(timeout))
	}
	args = append(args, containerdID)
	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl stop failed (%d): %s%s", code, stdout, stderr)
	}
	return nil
}

// waitDockerContainer waits for a container and returns its exit code.
// If the task has already exited and been cleaned up, it returns 0 as a best
// effort so that `docker run -d` does not print spurious errors.
func waitDockerContainer(id string) (int, error) {
	ns, containerdID, _, err := resolveDockerID(id)
	if err != nil {
		return -1, err
	}

	// If the task already stopped, return its exit status without blocking.
	cl, err := client.New(containerdSocket)
	if err == nil {
		nsCtx := namespaces.WithNamespace(context.Background(), ns)
		if c, err := cl.LoadContainer(nsCtx, containerdID); err == nil {
			if task, err := c.Task(nsCtx, nil); err == nil {
				if status, err := task.Status(nsCtx); err == nil {
					if status.Status != "running" {
						cl.Close()
						return int(status.ExitStatus), nil
					}
				}
			}
		}
		cl.Close()
	}

	stdout, stderr, code, err := runNerdctl(ns, "wait", containerdID)
	if err != nil || code != 0 {
		// Task may have exited and been deleted before nerdctl could attach.
		if strings.Contains(stderr, "not found") || strings.Contains(stdout, "not found") {
			return 0, nil
		}
		return -1, fmt.Errorf("nerdctl wait failed (%d): %s%s", code, stdout, stderr)
	}
	exitCode, _ := strconv.Atoi(strings.TrimSpace(stdout))
	return exitCode, nil
}

// deleteDockerContainer removes a container by Docker ID or name.
func deleteDockerContainer(id string, force bool) error {
	ns, containerdID, _, err := resolveDockerID(id)
	if err != nil {
		return err
	}
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, containerdID)
	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl rm failed (%d): %s%s", code, stdout, stderr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Docker exec compatibility (M7 L3)
// ---------------------------------------------------------------------------

// dockerExecCreateRequest mirrors Docker's POST /containers/{id}/exec body.
type dockerExecCreateRequest struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env"`
	User         string   `json:"User"`
	WorkingDir   string   `json:"WorkingDir"`
	Privileged   bool     `json:"Privileged"`
}

// dockerExecCreateResponse mirrors Docker's exec create response.
type dockerExecCreateResponse struct {
	Id string `json:"Id"`
}

// dockerExecStartRequest mirrors Docker's POST /exec/{id}/start body.
type dockerExecStartRequest struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

// dockerExecInspectResponse mirrors Docker's GET /exec/{id}/json response.
type dockerExecInspectResponse struct {
	ID            string `json:"ID"`
	Running       bool   `json:"Running"`
	ExitCode      int    `json:"ExitCode"`
	OpenStdin     bool   `json:"OpenStdin"`
	OpenStdout    bool   `json:"OpenStdout"`
	OpenStderr    bool   `json:"OpenStderr"`
	CanRemove     bool   `json:"CanRemove"`
	ContainerID   string `json:"ContainerID"`
	ProcessConfig struct {
		Tty        bool     `json:"tty"`
		Entrypoint string   `json:"entrypoint"`
		Arguments  []string `json:"arguments"`
	} `json:"ProcessConfig"`
}

// createDockerExec creates an exec instance and returns its Docker-compatible ID.
func createDockerExec(containerID string, req dockerExecCreateRequest) (string, error) {
	ns, containerdID, name, err := resolveDockerID(containerID)
	if err != nil {
		return "", err
	}

	spec := &execSpec{
		ID:                newExecID(),
		Namespace:         ns,
		ContainerName:     name,
		ContainerDockerID: dockerID(ns, containerdID),
		Cmd:               req.Cmd,
		Env:               req.Env,
		User:              req.User,
		WorkingDir:        req.WorkingDir,
		AttachStdin:       req.AttachStdin,
		AttachStdout:      req.AttachStdout,
		AttachStderr:      req.AttachStderr,
		Tty:               req.Tty,
		Privileged:        req.Privileged,
	}
	execs.add(spec)
	return spec.ID, nil
}

// buildNerdctlExecArgs builds the nerdctl exec argument slice for a spec.
func buildNerdctlExecArgs(spec *execSpec, detach bool) []string {
	args := []string{"exec"}
	if detach {
		args = append(args, "-d")
	}
	if spec.Tty {
		args = append(args, "-t")
	}
	// Only request interactive when stdin is wanted and we are not detaching.
	if spec.AttachStdin && !detach {
		args = append(args, "-i")
	}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	if spec.User != "" {
		args = append(args, "-u", spec.User)
	}
	if spec.WorkingDir != "" {
		args = append(args, "-w", spec.WorkingDir)
	}
	if spec.Privileged {
		args = append(args, "--privileged")
	}
	args = append(args, spec.ContainerName)
	args = append(args, spec.Cmd...)
	return args
}

// startDetachedExec runs an exec instance in the background and returns immediately.
func startDetachedExec(id string) error {
	spec := execs.get(id)
	if spec == nil {
		return fmt.Errorf("No such exec instance: %s", id)
	}

	args := buildNerdctlExecArgs(spec, true)
	stdout, stderr, code, err := runNerdctl(spec.Namespace, args...)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl exec failed (%d): %s%s", code, stdout, stderr)
	}
	return nil
}

// handleExecStart hijacks the HTTP connection, runs nerdctl exec, and streams
// stdout/stderr using Docker's raw-stream multiplexing format.
func handleExecStart(w http.ResponseWriter, r *http.Request, id string) {
	spec := execs.get(id)
	if spec == nil {
		http.Error(w, fmt.Sprintf(`{"message":"No such exec instance: %s"}`, id), http.StatusNotFound)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, `{"message":"hijacking not supported"}`, http.StatusInternalServerError)
		return
	}

	conn, bufrw, err := hj.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	// Write the upgrade response. Docker CLI expects 101 UPGRADED for attach.
	fmt.Fprintf(bufrw, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	if err := bufrw.Flush(); err != nil {
		return
	}

	spec.mu.Lock()
	spec.running = true
	spec.exitCode = 0
	spec.mu.Unlock()

	args := buildNerdctlExecArgs(spec, false)
	cmd := exec.Command("/opt/containerd/bin/nerdctl", append([]string{"-n", spec.Namespace}, args...)...)
	cmd.Env = append(os.Environ(), "PATH=/bin:/sbin:/usr/bin:/usr/sbin")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		spec.setExit(126)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		spec.setExit(126)
		return
	}

	stdinPipe, stdinWriter := io.Pipe()
	if spec.AttachStdin || spec.Tty {
		cmd.Stdin = stdinPipe
		go func() {
			// In TTY mode the client sends raw bytes; in non-TTY mode it sends
			// multiplexed frames. For the first implementation we forward raw
			// bytes only in TTY mode and discard stdin otherwise to avoid sending
			// Docker frame headers to the container process.
			if spec.Tty {
				io.Copy(stdinWriter, conn)
			} else {
				io.Copy(io.Discard, conn)
			}
			stdinWriter.Close()
		}()
	} else {
		stdinWriter.Close()
		stdinPipe.Close()
	}

	if err := cmd.Start(); err != nil {
		spec.setExit(126)
		return
	}

	var wg sync.WaitGroup
	writeMu := &sync.Mutex{}
	var (
		parsedExit   = -1
		parsedExitMu sync.Mutex
		execExitRe   = regexp.MustCompile(`exec failed with exit code (\d+)`)
	)

	stream := func(rc io.ReadCloser, streamType byte) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := rc.Read(buf)
			if n > 0 {
				if spec.Tty {
					writeMu.Lock()
					bufrw.Write(buf[:n])
					bufrw.Flush()
					writeMu.Unlock()
				} else {
					writeMu.Lock()
					writeDockerStream(bufrw, streamType, buf[:n])
					bufrw.Flush()
					writeMu.Unlock()
				}
			}
			if err != nil {
				return
			}
		}
	}

	// Scan stderr line-by-line so we can extract the real command exit code
	// from nerdctl's fatal message while still streaming it to the client.
	scanStderr := func(rc io.ReadCloser) {
		defer wg.Done()
		scanner := bufio.NewScanner(rc)
		for scanner.Scan() {
			line := scanner.Bytes()
			out := append([]byte{}, line...)
			out = append(out, '\n')
			writeMu.Lock()
			writeDockerStream(bufrw, 2, out)
			bufrw.Flush()
			writeMu.Unlock()
			if m := execExitRe.FindSubmatch(line); len(m) > 1 {
				if c, err := strconv.Atoi(string(m[1])); err == nil {
					parsedExitMu.Lock()
					parsedExit = c
					parsedExitMu.Unlock()
				}
			}
		}
	}

	wg.Add(2)
	go stream(stdout, 1)
	go scanStderr(stderr)

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			parsedExitMu.Lock()
			if parsedExit >= 0 {
				exitCode = parsedExit
			}
			parsedExitMu.Unlock()
		} else {
			exitCode = 126
		}
	}
	spec.setExit(exitCode)

	wg.Wait()
	bufrw.Flush()
	time.Sleep(50 * time.Millisecond)
}

// inspectDockerExec returns the running/exit state of an exec instance.
func inspectDockerExec(id string) (*dockerExecInspectResponse, error) {
	spec := execs.get(id)
	if spec == nil {
		return nil, fmt.Errorf("No such exec instance: %s", id)
	}
	running, code := spec.state()
	resp := &dockerExecInspectResponse{
		ID:          id,
		Running:     running,
		ExitCode:    code,
		OpenStdin:   spec.AttachStdin,
		OpenStdout:  spec.AttachStdout,
		OpenStderr:  spec.AttachStderr,
		CanRemove:   !running,
		ContainerID: spec.ContainerDockerID,
	}
	resp.ProcessConfig.Tty = spec.Tty
	if len(spec.Cmd) > 0 {
		resp.ProcessConfig.Entrypoint = spec.Cmd[0]
		resp.ProcessConfig.Arguments = spec.Cmd[1:]
	}
	return resp, nil
}

// pullDockerImage pulls an image via nerdctl.
func pullDockerImage(image string) (string, string, error) {
	ns := "default"
	stdout, stderr, code, err := runNerdctl(ns, "pull", image)
	if err != nil || code != 0 {
		return stdout, stderr, fmt.Errorf("nerdctl pull failed (%d): %s%s", code, stdout, stderr)
	}
	return stdout, stderr, nil
}

// writeDockerStream writes a Docker multiplexed stream frame.
// streamType: 0=stdin, 1=stdout, 2=stderr.
func writeDockerStream(w io.Writer, streamType byte, data []byte) error {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:], uint32(len(data)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// streamNerdctlLogsTo runs `nerdctl logs` (and optionally `nerdctl logs -f`)
// and writes output as Docker multiplexed stream frames until the command exits
// or the writer fails.
func streamNerdctlLogsTo(out io.Writer, ns, name string, follow bool) {
	flusher, _ := out.(http.Flusher)
	buf := make([]byte, 4096)

	// Helper that runs nerdctl logs with given args and copies output to out.
	runLogs := func(extraArgs ...string) (int, error) {
		args := []string{"-n", ns, "logs"}
		args = append(args, extraArgs...)
		args = append(args, name)
		cmd := exec.Command("/opt/containerd/bin/nerdctl", args...)
		cmd.Env = append(os.Environ(), "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return 0, err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return 0, err
		}
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		total := 0
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				total += n
				if writeErr := writeDockerStream(out, 1, buf[:n]); writeErr != nil {
					cmd.Process.Kill()
					cmd.Wait()
					return total, writeErr
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if err != nil {
				break
			}
		}
		io.Copy(io.Discard, stderr)
		cmd.Wait()
		return total, nil
	}

	// First replay any output that already exists. Short-lived containers may
	// need a brief moment for the json-file log to be flushed after exit.
	for attempt := 0; attempt < 10; attempt++ {
		n, err := runLogs()
		if err != nil {
			return
		}
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Then follow if requested and the container is still running.
	if follow && isNerdctlContainerRunning(ns, name) {
		runLogs("-f")
	}
}

// handleAttach hijacks the HTTP connection and streams container output using
// Docker's raw-stream multiplexing format. It uses `nerdctl logs` (non-following)
// first to replay output that already exists, then `nerdctl logs -f` only if
// the container is still running and the client asked for a stream. This avoids
// the race where short-lived containers exit before attach is called.
func handleAttach(w http.ResponseWriter, r *http.Request, id string) {
	ns, _, name, err := resolveDockerID(id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, `{"message":"hijacking not supported"}`, http.StatusInternalServerError)
		return
	}

	conn, bufrw, err := hj.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	fmt.Fprintf(bufrw, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	if err := bufrw.Flush(); err != nil {
		return
	}
	// Drain any stdin the client sends so the half-duplex pipe does not block.
	// Use the raw connection, not bufrw, because bufrw is used for writing below
	// and bufio types are not safe for concurrent read/write.
	go func() {
		io.Copy(io.Discard, conn)
	}()

	// Replay existing logs then follow if the container is still running.
	streamNerdctlLogsTo(bufrw, ns, name, true)

	// Ensure all buffered output reaches the client before closing.
	bufrw.Flush()
	time.Sleep(100 * time.Millisecond)
}

// handleLogs streams container logs using Docker's multiplexed stream format.
func handleLogs(w http.ResponseWriter, r *http.Request, id string) {
	ns, _, name, err := resolveDockerID(id)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
		return
	}

	args := []string{"-n", ns, "logs"}
	if r.URL.Query().Get("timestamps") == "1" || r.URL.Query().Get("timestamps") == "true" {
		args = append(args, "-t")
	}
	follow := r.URL.Query().Get("follow") == "1" || r.URL.Query().Get("follow") == "true"
	if follow {
		args = append(args, "-f")
	}
	args = append(args, name)

	cmd := exec.Command("/opt/containerd/bin/nerdctl", args...)
	cmd.Env = append(os.Environ(), "PATH=/bin:/sbin:/usr/bin:/usr/sbin")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	defer cmd.Wait()

	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	buf := make([]byte, 4096)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			if writeErr := writeDockerStream(w, 1, buf[:n]); writeErr != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

// stripAPIVersion removes a leading /v1.XX prefix so the same handlers work
// for any Docker client API version.
func stripAPIVersion(path string) string {
	if idx := strings.Index(path, "/v1."); idx == 0 {
		if slash := strings.Index(path[4:], "/"); slash != -1 {
			return path[4+slash:]
		}
	}
	return path
}

func runDockerAPIServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := stripAPIVersion(r.URL.Path)
		log.Printf("[docker-api] %s %s", r.Method, path)

		// Pre-compute container sub-resource IDs.
		containerSubresource := func(prefix, suffix string) (string, bool) {
			if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
				return "", false
			}
			return strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix), true
		}
		startID, isStart := containerSubresource("/containers/", "/start")
		stopID, isStop := containerSubresource("/containers/", "/stop")
		waitID, isWait := containerSubresource("/containers/", "/wait")
		deleteID, isDelete := containerSubresource("/containers/", "")
		inspectID, isInspect := containerSubresource("/containers/", "/json")
		attachID, isAttach := containerSubresource("/containers/", "/attach")
		logsID, isLogs := containerSubresource("/containers/", "/logs")
		execCreateID, isExecCreate := containerSubresource("/containers/", "/exec")
		execStartID, isExecStart := containerSubresource("/exec/", "/start")
		execInspectID, isExecInspect := containerSubresource("/exec/", "/json")

		// Pre-compute image sub-resource names. Avoid matching /images/json and /images/create.
		imageSubresource := func(prefix, suffix string) (string, bool) {
			if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
				return "", false
			}
			name := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
			if name == "" || name == "json" || name == "create" {
				return "", false
			}
			return name, true
		}
		tagName, isTag := imageSubresource("/images/", "/tag")
		pushName, isPush := imageSubresource("/images/", "/push")
		imageInspectName, isImageInspect := imageSubresource("/images/", "/json")
		rmiName := ""
		isRMI := false
		if r.Method == http.MethodDelete && strings.HasPrefix(path, "/images/") {
			name := strings.TrimPrefix(path, "/images/")
			if name != "" && name != "json" && name != "create" {
				rmiName, isRMI = name, true
			}
		}

		// Pre-compute network/volume sub-resource names.
		networkInspectID, isNetworkInspect := containerSubresource("/networks/", "")
		volumeInspectName, isVolumeInspect := containerSubresource("/volumes/", "")

		switch {
		case path == "/_ping":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		case path == "/version":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"Version":       "24.0.0",
				"ApiVersion":    dockerAPIVersion,
				"MinAPIVersion": dockerAPIVersion,
				"GitCommit":     "anvil",
				"Os":            "linux",
				"Arch":          "arm64",
				"KernelVersion": "",
				"BuildTime":     "",
			})
		case path == "/containers/json":
			all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
			containers, err := listDockerContainers()
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			if !all {
				var running []dockerContainerSummary
				for _, c := range containers {
					if c.State == "running" {
						running = append(running, c)
					}
				}
				containers = running
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(containers)
		case path == "/images/json":
			images, err := listDockerImages()
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(images)
		case path == "/images/create":
			image := r.URL.Query().Get("fromImage")
			if tag := r.URL.Query().Get("tag"); tag != "" {
				image += ":" + tag
			}
			if image == "" {
				http.Error(w, `{"message":"missing fromImage"}`, http.StatusBadRequest)
				return
			}
			stdout, stderr, err := pullDockerImage(image)
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				// Docker CLI expects a stream of progress objects; send one error line.
				json.NewEncoder(w).Encode(map[string]string{
					"status": fmt.Sprintf("error pulling %s: %s%s", image, stdout, stderr),
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{
				"status": fmt.Sprintf("Downloaded newer image for %s", image),
			})
		case isTag && r.Method == http.MethodPost:
			target := r.URL.Query().Get("repo")
			if tag := r.URL.Query().Get("tag"); tag != "" {
				target += ":" + tag
			}
			if target == "" {
				http.Error(w, `{"message":"missing repo/tag"}`, http.StatusBadRequest)
				return
			}
			if err := tagDockerImage(tagName, target); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case isPush && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			if err := pushDockerImage(pushName, w); err != nil {
				fmt.Fprintf(w, "{\"status\":\"error pushing %s: %s\"}\n", pushName, err.Error())
				return
			}
		case isRMI:
			if err := removeDockerImage(rmiName); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]string{{"Deleted": rmiName}})
		case isImageInspect && r.Method == http.MethodGet:
			info, err := inspectDockerImage(imageInspectName)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(info)
		case path == "/networks":
			networks, err := listDockerNetworks()
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(networks)
		case path == "/networks/create" && r.Method == http.MethodPost:
			var req dockerNetworkCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusBadRequest)
				return
			}
			nw, err := createDockerNetwork(req)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"Id": nw.Id, "Warning": ""})
		case isNetworkInspect && r.Method == http.MethodGet:
			nw, err := inspectDockerNetwork(networkInspectID)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(nw)
		case isNetworkInspect && r.Method == http.MethodDelete:
			if err := removeDockerNetwork(networkInspectID); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case path == "/volumes":
			volumes, err := listDockerVolumes()
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(dockerVolumeList{Volumes: volumes})
		case path == "/volumes/create" && r.Method == http.MethodPost:
			var req dockerVolumeCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusBadRequest)
				return
			}
			vol, err := createDockerVolume(req)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(vol)
		case isVolumeInspect && r.Method == http.MethodGet:
			vol, err := inspectDockerVolume(volumeInspectName)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(vol)
		case isVolumeInspect && r.Method == http.MethodDelete:
			if err := removeDockerVolume(volumeInspectName); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case path == "/containers/create" && r.Method == http.MethodPost:
			var req dockerCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusBadRequest)
				return
			}
			name := r.URL.Query().Get("name")
			id, err := createDockerContainer(req, name)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(dockerCreateResponse{Id: id})
		case isStart && r.Method == http.MethodPost:
			if err := startDockerContainer(startID); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isStop && r.Method == http.MethodPost:
			timeout := 10
			if t := r.URL.Query().Get("t"); t != "" {
				if v, err := strconv.Atoi(t); err == nil {
					timeout = v
				}
			}
			if err := stopDockerContainer(stopID, timeout); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isWait && r.Method == http.MethodPost:
			exitCode, err := waitDockerContainer(waitID)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(dockerWaitResponse{StatusCode: exitCode})
		case isAttach && r.Method == http.MethodPost:
			handleAttach(w, r, attachID)
		case isLogs && r.Method == http.MethodGet:
			handleLogs(w, r, logsID)
		case isExecCreate && r.Method == http.MethodPost:
			var req dockerExecCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusBadRequest)
				return
			}
			id, err := createDockerExec(execCreateID, req)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(dockerExecCreateResponse{Id: id})
		case isExecStart && r.Method == http.MethodPost:
			var req dockerExecStartRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusBadRequest)
				return
			}
			if req.Detach {
				if err := startDetachedExec(execStartID); err != nil {
					http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusOK)
				return
			}
			handleExecStart(w, r, execStartID)
		case isExecInspect && r.Method == http.MethodGet:
			info, err := inspectDockerExec(execInspectID)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(info)
		case isDelete && r.Method == http.MethodDelete:
			force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
			if err := deleteDockerContainer(deleteID, force); err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case isInspect && r.Method == http.MethodGet:
			inspect, err := inspectDockerContainer(inspectID)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"message":"%s"}`, err.Error()), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(inspect)
		default:
			http.NotFound(w, r)
		}
	})

	l, err := vsock.Listen(dockerAPIPort, nil)
	if err != nil {
		log.Printf("[docker-api] listen: %v", err)
		return
	}
	log.Printf("[docker-api] listening on vsock port %d", dockerAPIPort)
	srv := &http.Server{Handler: mux}
	if err := srv.Serve(l); err != nil {
		log.Printf("[docker-api] serve: %v", err)
	}
}
