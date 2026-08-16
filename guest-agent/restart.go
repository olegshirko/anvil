package main

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// nerdctl accepts --restart but has no restart monitor, so policies were
// silently ignored (documented as a limitation until now). This monitor
// watches policy-tracked containers via the containerd task status (the
// authoritative state — nerdctl's own inspect reports Status:""/Restarting
// for policy containers) and brings them back per Docker semantics:
//   - "always" / "unless-stopped": restart on any exit
//   - "on-failure[:max]": restart only on non-zero exit, up to max retries
// A user-initiated stop (docker stop / kill / rm) clears the policy.
//
// Restarts are paced with an exponential backoff (1s doubling to 30s) that
// resets once the container is observed running, so rapid-exit containers
// do not spin the monitor but are still retried like Docker does.

type restartPolicy struct {
	name string
	max  int // -1 = unlimited
}

const restartBackoffMax = 30 * time.Second

type restartMonitor struct {
	mu       sync.Mutex
	policies map[string]restartPolicy // dockerID -> policy
	retries  map[string]int           // dockerID -> consecutive failed restarts
	backoff  map[string]time.Duration // dockerID -> current backoff
	nextAt   map[string]time.Time     // dockerID -> earliest next restart
	// specs keeps the originally requested policy (including "no"-adjacent
	// forms) for docker inspect fidelity; counts tracks performed restarts.
	specs   map[string]restartPolicy // dockerID -> requested spec
	counts  map[string]int           // dockerID -> restarts performed
	stopped map[string]bool          // dockerID -> user-stopped (policy disabled)
}

var restarts = &restartMonitor{
	policies: make(map[string]restartPolicy),
	retries:  make(map[string]int),
	backoff:  make(map[string]time.Duration),
	nextAt:   make(map[string]time.Time),
	specs:    make(map[string]restartPolicy),
	counts:   make(map[string]int),
	stopped:  make(map[string]bool),
}

// register records the requested policy at create time: active policies go
// to the monitor loop, every spec is kept for inspect fidelity.
func (m *restartMonitor) register(dockerID, name string, max int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := restartPolicy{name: name, max: max}
	m.specs[dockerID] = p
	if name == "" || name == "no" {
		return
	}
	m.policies[dockerID] = p
	m.resetLocked(dockerID)
}

// policySpecFor returns the requested spec for docker inspect.
func (m *restartMonitor) policySpecFor(dockerID string) dockerRestartPolicy {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.specs[dockerID]
	hc := dockerRestartPolicy{Name: p.name}
	if p.max >= 0 {
		hc.MaximumRetryCount = p.max
	}
	if hc.Name == "" {
		hc.Name = "no"
	}
	return hc
}

// countFor returns how many restarts the monitor performed.
func (m *restartMonitor) countFor(dockerID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[dockerID]
}

// clear disables the policy (user stop/kill/rm wins) but keeps the
// requested spec for inspect fidelity.
func (m *restartMonitor) clear(dockerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.policies, dockerID)
	delete(m.stopped, dockerID)
	m.resetLocked(dockerID)
}

func (m *restartMonitor) resetLocked(dockerID string) {
	delete(m.retries, dockerID)
	delete(m.backoff, dockerID)
	delete(m.nextAt, dockerID)
}

// parseRestartPolicy splits "on-failure:3" into ("on-failure", 3). The
// docker CLI sends the count separately (MaximumRetryCount); callers merge
// it in before set().
func parseRestartPolicy(spec string) restartPolicy {
	if spec == "" {
		return restartPolicy{}
	}
	parts := strings.SplitN(spec, ":", 2)
	p := restartPolicy{name: parts[0], max: -1}
	if len(parts) == 2 {
		if n, err := strconv.Atoi(parts[1]); err == nil {
			p.max = n
		}
	}
	return p
}

// runRestartMonitor polls policy-tracked containers once a second.
func runRestartMonitor() {
	for {
		time.Sleep(1 * time.Second)
		maybeRestartContainers()
	}
}

func maybeRestartContainers() {
	snapshot := func() map[string]restartPolicy {
		restarts.mu.Lock()
		defer restarts.mu.Unlock()
		out := make(map[string]restartPolicy, len(restarts.policies))
		for k, v := range restarts.policies {
			out[k] = v
		}
		return out
	}()
	if len(snapshot) == 0 {
		return
	}
	cl, err := client.New(containerdSocket)
	if err != nil {
		return
	}
	defer cl.Close()
	for did := range snapshot {
		ns, containerdID, _, err := resolveDockerID(did)
		if err != nil {
			// Container gone (auto-removed); drop the policy.
			restarts.clear(did)
			continue
		}
		running, exit, ok := taskExitState(cl, ns, containerdID)
		if !ok {
			continue
		}
		if running {
			// A healthy run resets the backoff so a future failure starts
			// pacing from scratch.
			restarts.mu.Lock()
			if _, exists := restarts.backoff[did]; exists {
				restarts.resetLocked(did)
			}
			restarts.mu.Unlock()
			continue
		}
		if !restartDue(did) {
			continue
		}
		// Re-check the policy under the lock right before acting: a user
		// stop may have cleared it while this iteration was resolving the
		// container/task state from the snapshot above.
		p, ok := func() (restartPolicy, bool) {
			restarts.mu.Lock()
			defer restarts.mu.Unlock()
			p, ok := restarts.policies[did]
			return p, ok
		}()
		if !ok {
			continue
		}
		if p.name == "on-failure" && exit == 0 {
			restarts.clear(did)
			continue
		}
		if p.name == "on-failure" && p.max >= 0 && restarts.bumpRetries(did) > p.max {
			restarts.clear(did)
			continue
		}
		restarts.mu.Lock()
		restarts.counts[did]++
		restarts.paceNextLocked(did)
		restarts.mu.Unlock()
		log.Printf("[restart] restarting %s (policy %s, exit %d)", truncateID(did), p.name, exit)
		if err := startDockerContainer(did); err != nil {
			log.Printf("[restart] start %s failed: %v", truncateID(did), err)
		}
	}
}

// restartDue reports whether the backoff window has elapsed.
func restartDue(dockerID string) bool {
	restarts.mu.Lock()
	defer restarts.mu.Unlock()
	next, exists := restarts.nextAt[dockerID]
	return !exists || !time.Now().Before(next)
}

// paceNextLocked doubles the backoff and schedules the next attempt.
// Caller must hold the lock.
func (m *restartMonitor) paceNextLocked(dockerID string) {
	b := m.backoff[dockerID]
	if b <= 0 {
		b = time.Second
	} else {
		b *= 2
		if b > restartBackoffMax {
			b = restartBackoffMax
		}
	}
	m.backoff[dockerID] = b
	m.nextAt[dockerID] = time.Now().Add(b)
}

func (m *restartMonitor) bumpRetries(dockerID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retries[dockerID]++
	return m.retries[dockerID]
}

// taskExitState reads the authoritative run state from the containerd task:
// running, the real exit code (nerdctl reports 0 for policy containers),
// and whether the state could be determined at all.
func taskExitState(cl *client.Client, ns, containerdID string) (running bool, exit int, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = namespaces.WithNamespace(ctx, ns)
	c, err := cl.LoadContainer(ctx, containerdID)
	if err != nil {
		return false, 0, false
	}
	task, err := c.Task(ctx, nil)
	if err != nil {
		// No task yet: created but never started — not a restart candidate.
		return false, 0, false
	}
	st, err := task.Status(ctx)
	if err != nil {
		return false, 0, false
	}
	switch st.Status {
	case "running":
		return true, 0, true
	case "stopped":
		return false, int(st.ExitStatus), true
	default: // created / paused / unknown
		return false, 0, false
	}
}

func truncateID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
