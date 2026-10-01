package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// Docker's default ephemeral range (Linux ip_local_port_range).
const (
	ephemeralPortLo = 32768
	ephemeralPortHi = 60999
	// Candidates sent to the host per port-check round trip.
	ephemeralProbeBatch = 32
)

var (
	// ephemeralMu serializes host-port picks, so two containers starting
	// at once (compose up) cannot pick the same port.
	ephemeralMu sync.Mutex
	// ephemeralPending holds ports picked for a start that is still in
	// progress: the container's task is not running yet, so the running-
	// container scan does not see them.
	ephemeralPending = map[int]string{}
)

// assignEphemeralPorts gives every ephemeral binding of the container a free
// host port (`-p 80`, `-P`, `-p 8000-8010:80`), persisting it in the
// container metadata and the anvil/ports label so the port forwarder,
// `docker port` and inspect see it. A port assigned by an earlier start is
// kept while it is still free. The returned func drops the pending
// reservation; call it once the start has finished either way.
func assignEphemeralPorts(ctx context.Context, ns, id string) (func(), error) {
	noop := func() {}
	meta, err := loadContainerMeta(ns, id)
	if err != nil || !hasEphemeralPorts(meta.Ports) {
		return noop, nil
	}

	ephemeralMu.Lock()
	defer ephemeralMu.Unlock()

	used := runningHostPorts(ctx, id)
	for p, owner := range ephemeralPending {
		if owner != id {
			used[p] = true
		}
	}
	for _, m := range meta.Ports {
		if !m.Ephemeral && m.HostPort > 0 {
			used[m.HostPort] = true
		}
	}

	changed := false
	var mine []int
	for i := range meta.Ports {
		m := &meta.Ports[i]
		if !m.Ephemeral {
			continue
		}
		if m.HostPort > 0 && !used[m.HostPort] && inEphemeralRange(*m, m.HostPort) && !hostPortBusy(*m, m.HostPort) {
			used[m.HostPort] = true
			mine = append(mine, m.HostPort)
			continue
		}
		p, perr := pickHostPort(*m, used)
		if perr != nil {
			return noop, perr
		}
		m.HostPort = p
		used[p] = true
		mine = append(mine, p)
		changed = true
	}

	if changed {
		if err := saveContainerMeta(meta); err != nil {
			return noop, fmt.Errorf("save port assignment: %w", err)
		}
		if err := setPortsLabel(ctx, ns, id, meta.Ports); err != nil {
			return noop, fmt.Errorf("save port assignment: %w", err)
		}
	}
	for _, p := range mine {
		ephemeralPending[p] = id
	}
	return func() {
		ephemeralMu.Lock()
		defer ephemeralMu.Unlock()
		for _, p := range mine {
			if ephemeralPending[p] == id {
				delete(ephemeralPending, p)
			}
		}
	}, nil
}

func hasEphemeralPorts(ports []cniPortMapping) bool {
	for _, m := range ports {
		if m.Ephemeral {
			return true
		}
	}
	return false
}

func ephemeralBounds(m cniPortMapping) (int, int) {
	if m.RangeLo > 0 && m.RangeHi >= m.RangeLo {
		return m.RangeLo, m.RangeHi
	}
	return ephemeralPortLo, ephemeralPortHi
}

func inEphemeralRange(m cniPortMapping, p int) bool {
	lo, hi := ephemeralBounds(m)
	return p >= lo && p <= hi
}

// hostPortBusy asks the Mac whether a foreign process holds the TCP port.
// UDP is not checked: the host query covers TCP binds only.
func hostPortBusy(m cniPortMapping, p int) bool {
	if m.Protocol != "" && m.Protocol != "tcp" {
		return false
	}
	return len(busyForeignHostPorts([]int{p})) > 0
}

// pickHostPort returns a port in the binding's range that no running
// container publishes and no Mac process holds. An explicit range is
// scanned from its low end, as Docker does; the ephemeral range from a
// random offset, so restarts do not keep colliding on the same port.
func pickHostPort(m cniPortMapping, used map[int]bool) (int, error) {
	lo, hi := ephemeralBounds(m)
	size := hi - lo + 1
	start := 0
	if m.RangeLo == 0 {
		start = rand.IntN(size)
	}
	checkHost := m.Protocol == "" || m.Protocol == "tcp"
	batch := make([]int, 0, ephemeralProbeBatch)
	flush := func() int {
		if len(batch) == 0 {
			return 0
		}
		busy := map[int]bool{}
		if checkHost {
			for _, p := range busyForeignHostPorts(batch) {
				busy[p] = true
			}
		}
		for _, p := range batch {
			if !busy[p] {
				return p
			}
		}
		batch = batch[:0]
		return 0
	}
	for i := 0; i < size; i++ {
		p := lo + (start+i)%size
		if used[p] {
			continue
		}
		batch = append(batch, p)
		if len(batch) == ephemeralProbeBatch {
			if got := flush(); got > 0 {
				return got, nil
			}
		}
	}
	if got := flush(); got > 0 {
		return got, nil
	}
	if m.RangeLo > 0 {
		return 0, fmt.Errorf("Bind for %s:%d-%d failed: port is already allocated", bindHostIP(m), lo, hi)
	}
	return 0, fmt.Errorf("no free host port for %d/%s: all ports are allocated", m.ContainerPort, protoOrTCP(m.Protocol))
}

func bindHostIP(m cniPortMapping) string {
	if m.HostIP == "" {
		return "0.0.0.0"
	}
	return m.HostIP
}

func protoOrTCP(p string) string {
	if p == "" {
		return "tcp"
	}
	return p
}

// runningHostPorts returns the host ports published by every running
// container except excludeID.
func runningHostPorts(ctx context.Context, excludeID string) map[int]bool {
	used := map[int]bool{}
	cl, err := pc.get(ctx)
	if err != nil {
		return used
	}
	nss, err := cl.NamespaceService().List(ctx)
	if err != nil {
		return used
	}
	for _, ns := range nss {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		live := namespaceRunningTasks(nsCtx, cl)
		for cid := range live {
			if cid == excludeID {
				continue
			}
			c, err := cl.LoadContainer(nsCtx, cid)
			if err != nil {
				continue
			}
			portsJSON := portsLabel(c, nsCtx)
			if portsJSON == "" {
				continue
			}
			var mapped []cniPortMapping
			if json.Unmarshal([]byte(portsJSON), &mapped) != nil {
				continue
			}
			for _, m := range mapped {
				if m.HostPort > 0 {
					used[m.HostPort] = true
				}
			}
		}
	}
	return used
}

// setPortsLabel rewrites the container's anvil/ports label.
func setPortsLabel(ctx context.Context, ns, id string, ports []cniPortMapping) error {
	cl, err := pc.get(ctx)
	if err != nil {
		return err
	}
	nsCtx := namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(nsCtx, id)
	if err != nil {
		return err
	}
	data, err := json.Marshal(ports)
	if err != nil {
		return err
	}
	if _, err := c.SetLabels(nsCtx, map[string]string{labelPorts: string(data)}); err != nil {
		return err
	}
	log.Printf("[ports] %s: host ports assigned %s", truncateID(id), data)
	return nil
}

// publishAllMappings is `-P`: every port the container or its image exposes,
// and that has no explicit binding, gets an ephemeral host port.
func publishAllMappings(req dockerCreateRequest, imageExposed map[string]struct{}) []cniPortMapping {
	exposed := map[string]bool{}
	for k := range imageExposed {
		exposed[k] = true
	}
	for k := range req.ExposedPorts {
		exposed[k] = true
	}
	keys := make([]string, 0, len(exposed))
	for k := range exposed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []cniPortMapping
	for _, spec := range keys {
		port, proto, _ := strings.Cut(spec, "/")
		if proto == "" {
			proto = "tcp"
		}
		if _, bound := req.HostConfig.PortBindings[port+"/"+proto]; bound {
			continue
		}
		if proto == "tcp" {
			if _, bound := req.HostConfig.PortBindings[port]; bound {
				continue
			}
		}
		cPorts, err := expandPortRange(port)
		if err != nil {
			continue
		}
		for _, c := range cPorts {
			out = append(out, cniPortMapping{ContainerPort: c, Protocol: proto, HostIP: "0.0.0.0", Ephemeral: true})
		}
	}
	return out
}
