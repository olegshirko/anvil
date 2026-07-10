// guest-agent runs inside the Linux VM and accepts control commands over
// virtio-vsock. It is intentionally small and static-linked.
package main

import (
	"bufio"
	"context"
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/mdlayher/vsock"
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
			portsJSON := labels["nerdctl/ports"]
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

			portsJSON := labels["nerdctl/ports"]
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
	base := sanitizeCNIName(ns)
	path := filepath.Join(cniConfDir, "nerdctl-"+base+".conflist")

	bridge := "br-" + base
	if len(bridge) > 15 {
		bridge = bridge[:15]
	}

	octet := projectSubnetOctet(ns)
	subnet := fmt.Sprintf("10.10.%d.0/24", octet)
	gateway := fmt.Sprintf("10.10.%d.1", octet)

	conf := map[string]interface{}{
		"cniVersion": cniVersion,
		"name":       ns,
		"nerdctlID":  networkID(ns),
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
			if portsJSON := labels["nerdctl/ports"]; portsJSON != "" {
				var pm []cniPortMapping
				if err := json.Unmarshal([]byte(portsJSON), &pm); err == nil {
					for _, p := range pm {
						proto := p.Protocol
						if proto == "" {
							proto = "tcp"
						}
						ports = append(ports, dockerPort{
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

// createDockerContainer creates a container via nerdctl and returns its Docker ID.
func createDockerContainer(req dockerCreateRequest, name string) (string, error) {
	ns := "default"
	if req.HostConfig.NetworkMode != "" && req.HostConfig.NetworkMode != "default" {
		ns = req.HostConfig.NetworkMode
	}

	args := []string{"create"}
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

	// Look up the containerd ID by the name we requested.
	cl, err := client.New(containerdSocket)
	if err != nil {
		return "", fmt.Errorf("containerd client: %w", err)
	}
	defer cl.Close()

	ctx := context.Background()
	nsCtx := namespaces.WithNamespace(ctx, ns)
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
	ns, _, name, err := resolveDockerID(id)
	if err != nil {
		return err
	}
	stdout, stderr, code, err := runNerdctl(ns, "start", name)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl start failed (%d): %s%s", code, stdout, stderr)
	}
	return nil
}

// stopDockerContainer stops a container by Docker ID or name.
func stopDockerContainer(id string, timeout int) error {
	ns, _, name, err := resolveDockerID(id)
	if err != nil {
		return err
	}
	args := []string{"stop"}
	if timeout > 0 {
		args = append(args, "-t", strconv.Itoa(timeout))
	}
	args = append(args, name)
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
	ns, containerdID, name, err := resolveDockerID(id)
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

	stdout, stderr, code, err := runNerdctl(ns, "wait", name)
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
	ns, _, name, err := resolveDockerID(id)
	if err != nil {
		return err
	}
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, name)
	stdout, stderr, code, err := runNerdctl(ns, args...)
	if err != nil || code != 0 {
		return fmt.Errorf("nerdctl rm failed (%d): %s%s", code, stdout, stderr)
	}
	return nil
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
			// Minimal images list: forward nerdctl images output.
			stdout, stderr, code, err := runNerdctl("default", "images", "--format", "json")
			if err != nil || code != 0 {
				http.Error(w, fmt.Sprintf(`{"message":"%s%s"}`, stdout, stderr), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(stdout))
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
