// guest-agent runs inside the Linux VM and accepts control commands over
// virtio-vsock. It is intentionally small and static-linked.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/mdlayher/vsock"
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
	ContainerIP   string `json:"container_ip,omitempty"`
}

// PortMapState is the full snapshot pushed to vz-runner.
type PortMapState struct {
	Mappings []PortMapping `json:"mappings"`
}

func main() {
	log.SetPrefix("[guest-agent] ")
	setupDebugLogRotation()

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

	// seed-entropy <file>: credit the kernel entropy pool with a host-provided
	// seed via RNDADDENTROPY. The virt kernel lacks RANDOM_TRUST_CPU and VZ
	// has no virtio-rng, so crng init otherwise takes ~10 s on boot.
	if len(os.Args) > 2 && os.Args[1] == "seed-entropy" {
		if err := seedEntropy(os.Args[2]); err != nil {
			log.Fatalf("seed-entropy: %v", err)
		}
		return
	}

	log.Printf("listening on vsock port %d", listenPort)

	// guest-agent is PID 1 inside the VM. Orphaned children reparent to PID 1,
	// and containerd/runc may create short-lived hook/helper processes. Reap
	// them so they do not accumulate as zombies and deadlock containerd-shim.
	go reapZombies()
	go servePortProxy()
	go runRestartMonitor()

	// Recreate CNI conflists from the host share after a cold boot. nerdctl
	// keeps network state on the persistent containerd disk, but the conflist
	// itself is on tmpfs and disappears on reboot.
	restoreNetworkConfigs()

	// The cold-boot metadata cleanup in stage2 removes containers bypassing
	// nerdctl rm, leaving their network-store entries behind; prune them once
	// containerd is reachable.
	go func() {
		time.Sleep(2 * time.Second)
		pruneStaleNetworkStore()
	}()

	// VZ does not guarantee a sane RTC and snapshot resume leaves the clock
	// frozen at pause time; sync it from the host-written time file.
	syncClockFromShare()
	go periodicClockSync()

	// Discard unused blocks on the containerd ext4 once a day so the sparse
	// disk image on the host can return space after image prune.
	go periodicFstrim()

	scanner := newPortScanner()
	go scanner.run()

	// Docker API server on a separate vsock port so the existing control
	// channel stays untouched.
	go runDockerAPIServer()

	// Buildkit bridge for the buildx remote driver (lazy buildkitd start).
	go serveBuildkitBridge()

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

	// A fresh subscribe means the host daemon (re)started; if this VM was
	// restored from a snapshot, the clock is stale — sync it again.
	syncClockFromShare()

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

// syncClockFromShare sets the VM clock from the epoch file vz-runner writes
// into the virtiofs share at VM start. VZ does not guarantee a sane RTC
// (boots can start at 1970-01-01) and after a snapshot resume the clock is
// frozen at pause time; TLS to registries fails in both cases.
func syncClockFromShare() {
	data, err := os.ReadFile("/mnt/anvil/.anvil-host-time")
	if err != nil {
		return
	}
	epoch := strings.TrimSpace(string(data))
	if epoch == "" {
		return
	}
	if out, err := exec.Command("date", "-s", "@"+epoch).CombinedOutput(); err != nil {
		log.Printf("[clock] sync from host time failed: %v: %s", err, out)
		return
	}
	log.Printf("[clock] synced from host time file (%s)", epoch)
}

// periodicClockSync keeps the guest clock within ~1 s of the host: the VZ
// RTC ticks unreliably and every idle-pause adds the paused interval as
// drift. vz-runner refreshes the time file every 5 s while the VM runs;
// we poll it and step the clock when the drift grows beyond the threshold.
// Drifted timestamps break `docker events --until` (computed host-side) and
// log timestamps.
//
// Only forward steps are applied: a backward step mid-connection breaks
// TCP timers (seen as TLS handshakes to auth.docker.io timing out inside
// buildkitd), and in practice the guest clock only ever runs behind (slow
// RTC + frozen during pauses). When the guest is momentarily ahead (poll
// racing the file refresh), no correction is needed — it falls behind on
// its own within seconds.
func periodicClockSync() {
	const hostTimeFile = "/mnt/anvil/.anvil-host-time"
	const minDrift = time.Second
	corrected := 0
	for {
		time.Sleep(5 * time.Second)
		data, err := os.ReadFile(hostTimeFile)
		if err != nil {
			continue
		}
		epoch, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			continue
		}
		hostTime := time.Unix(epoch, 0)
		// How far the host time (per the file) is AHEAD of the guest clock.
		ahead := hostTime.Sub(time.Now())
		if ahead <= minDrift {
			continue
		}
		if out, err := exec.Command("date", "-s", "@"+strconv.FormatInt(epoch, 10)).CombinedOutput(); err != nil {
			log.Printf("[clock] periodic sync failed: %v: %s", err, out)
			continue
		}
		corrected++
		if corrected <= 3 || corrected%20 == 0 {
			log.Printf("[clock] stepped clock forward to host time (was %s behind, correction #%d)", ahead, corrected)
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
			if conflict, err := findHostPortConflict(ports, ""); err != nil {
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

	// Drain stdout and stderr concurrently: sequential reads deadlock as soon
	// as the child writes more than a pipe buffer (64 KiB) to stderr while
	// stdout stays open (e.g. nerdctl compose progress output).
	var outBuf, errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&outBuf, stdout)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&errBuf, stderr)
	}()
	wg.Wait()
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	return Response{
		Stdout:   outBuf.String(),
		Stderr:   errBuf.String(),
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
// The container excludeID is skipped: it is the one being started.
func findHostPortConflict(ports []int, excludeID string) (*PortMapping, error) {
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
			if excludeID != "" && c.ID() == excludeID {
				continue
			}
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

// periodicFstrim runs fstrim on the containerd filesystem once a day. The
// virtio-blk disk reports discard support, so trimmed blocks punch holes in
// the sparse image on the host and `make disk-compact` becomes unnecessary
// for routine prune cleanup. Failures are logged and ignored.
func periodicFstrim() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		out, err := exec.Command("fstrim", "-v", "/var/lib/containerd").CombinedOutput()
		msg := strings.TrimSpace(string(out))
		if err != nil {
			log.Printf("fstrim failed: %v (%s)", err, msg)
		} else {
			log.Printf("fstrim: %s", msg)
		}
	}
}

// reapZombies runs for the lifetime of the process and reaps orphaned
// grandchildren that get reparented to PID 1. Direct children spawned via
// os/exec are reaped by their own Wait(); to avoid racing it (a stolen
// zombie makes Wait fail with "waitid: no child processes"), a zombie is
// reaped only after it stays unclaimed for a grace period.
func reapZombies() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGCHLD)

	// Drain any orphans that already exist at startup.
	reapOrphans()

	for range sigCh {
		reapOrphans()
	}
}

// zombiePids lists current zombie children via /proc (state 'Z', ppid = us)
// without reaping them.
func zombiePids() []int {
	self := strconv.Itoa(os.Getpid())
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// stat format: pid (comm) state ppid ... — comm may contain spaces
		// and parens, so parse from the last ')'.
		s := string(data)
		i := strings.LastIndex(s, ")")
		if i < 0 || i+2 >= len(s) {
			continue
		}
		fields := strings.Fields(s[i+1:])
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "Z" && fields[1] == self {
			pids = append(pids, pid)
		}
	}
	return pids
}

// isZombie reports whether pid is still an unclaimed zombie child of ours.
func isZombie(pid int) bool {
	for _, z := range zombiePids() {
		if z == pid {
			return true
		}
	}
	return false
}

func reapOrphans() {
	pids := zombiePids()
	if len(pids) == 0 {
		return
	}
	// os/exec claims its children immediately after they exit; anything still
	// a zombie after the grace period is an orphan reparented to PID 1.
	time.Sleep(2 * time.Second)
	for _, pid := range pids {
		if !isZombie(pid) {
			continue // already claimed by os/exec
		}
		var status syscall.WaitStatus
		if wpid, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); err == nil && wpid > 0 {
			log.Printf("reaped orphaned child pid %d", pid)
		}
	}
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
