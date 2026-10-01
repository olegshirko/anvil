package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// dockerHealthState mirrors the State.Health object Docker returns from
// GET /containers/{id}/json. It is populated by the guest-agent because
// Docker-compatible healthchecks are implemented in-agent via task exec.
type dockerHealthState struct {
	Status        string            `json:"Status"`
	FailingStreak int               `json:"FailingStreak"`
	Log           []dockerHealthLog `json:"Log,omitempty"`
}

type dockerHealthLog struct {
	Start    time.Time `json:"Start"`
	End      time.Time `json:"End"`
	ExitCode int       `json:"ExitCode"`
	Output   string    `json:"Output"`
}

type activeHealthCheck struct {
	mu       sync.RWMutex
	state    dockerHealthState
	stopChan chan struct{}
}

var healthChecks = struct {
	mu   sync.RWMutex
	byID map[string]*activeHealthCheck
}{
	byID: make(map[string]*activeHealthCheck),
}

var healthcheckConfigs = struct {
	mu   sync.RWMutex
	byID map[string]*dockerHealthcheck
}{
	byID: make(map[string]*dockerHealthcheck),
}

var healthcheckUsers = struct {
	mu   sync.RWMutex
	byID map[string]string
}{
	byID: make(map[string]string),
}

// setHealthcheckConfig stores a container's healthcheck configuration and
// OCI user so it can be started when the container is started.
func setHealthcheckConfig(dockerID string, hc *dockerHealthcheck, containerUser string) {
	if hc == nil || len(hc.Test) == 0 {
		return
	}
	healthcheckConfigs.mu.Lock()
	healthcheckConfigs.byID[dockerID] = hc
	healthcheckConfigs.mu.Unlock()

	healthcheckUsers.mu.Lock()
	healthcheckUsers.byID[dockerID] = containerUser
	healthcheckUsers.mu.Unlock()
}

// startHealthCheck begins monitoring a container's health. It is a no-op if
// the healthcheck is nil, empty, or explicitly disabled with ["NONE"].
// containerUser is the OCI user the container runs as; healthcheck exec
// should use the same user so tools like psql/pg_isready pick the right role.
func startHealthCheck(dockerID, ns, containerdID string, hc *dockerHealthcheck, containerUser string) {
	if hc == nil || len(hc.Test) == 0 {
		return
	}
	if hc.Test[0] == "NONE" {
		return
	}

	setHealthcheckConfig(dockerID, hc, containerUser)

	interval := time.Duration(hc.Interval)
	if interval <= 0 {
		interval = 30 * time.Second
	}
	startPeriod := time.Duration(hc.StartPeriod)
	if startPeriod < 0 {
		startPeriod = 0
	}
	timeout := time.Duration(hc.Timeout)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	retries := hc.Retries
	if retries <= 0 {
		retries = 3
	}
	startInterval := time.Duration(hc.StartInterval)
	if startInterval <= 0 {
		startInterval = 5 * time.Second // Docker's default
	}

	var cmd []string
	switch hc.Test[0] {
	case "CMD":
		cmd = hc.Test[1:]
	case "CMD-SHELL":
		shellCmd := strings.Join(hc.Test[1:], " ")
		cmd = []string{"sh", "-c", shellCmd}
	default:
		// Treat unknown test types as a raw command list, matching Docker's
		// behaviour for the legacy list form.
		cmd = hc.Test
	}

	h := &activeHealthCheck{
		state: dockerHealthState{
			Status: "starting",
			Log:    make([]dockerHealthLog, 0, retries+1),
		},
		stopChan: make(chan struct{}),
	}

	healthChecks.mu.Lock()
	if old, ok := healthChecks.byID[dockerID]; ok {
		close(old.stopChan)
	}
	healthChecks.byID[dockerID] = h
	healthChecks.mu.Unlock()

	goSafe("healthcheck", func() {
		// As Docker: during StartPeriod probe every StartInterval; a success
		// makes the container healthy at once, a failure does not count.
		// Sleeping through the whole start period first kept a container
		// with start_period: 60s from turning healthy for over a minute.
		began := time.Now()
		consecutiveFailures := 0
		timer := time.NewTimer(nextHealthDelay(began, time.Now(), startPeriod, startInterval, interval, true))
		defer timer.Stop()

		for {
			select {
			case <-h.stopChan:
				return
			case <-timer.C:
			}

			start := time.Now()
			exitCode, output := runHealthCheckCommand(ns, containerdID, containerUser, cmd, timeout)
			end := time.Now()

			h.mu.Lock()
			h.state.Log = append(h.state.Log, dockerHealthLog{
				Start:    start,
				End:      end,
				ExitCode: exitCode,
				Output:   output,
			})
			// Keep only the most recent entries to bound memory.
			if len(h.state.Log) > 5 {
				h.state.Log = h.state.Log[len(h.state.Log)-5:]
			}

			prev := h.state.Status
			inStart := prev == "starting" && end.Sub(began) < startPeriod
			if exitCode == 0 || !inStart {
				h.state.Status, consecutiveFailures = nextHealthStatus(prev, exitCode == 0, consecutiveFailures, retries)
			}
			h.state.FailingStreak = consecutiveFailures
			status := h.state.Status
			h.mu.Unlock()
			if status != prev {
				publishHealthEvent(ns, containerdID, status)
			}
			timer.Reset(nextHealthDelay(began, time.Now(), startPeriod, startInterval, interval, status == "starting"))
		}
	})
}

func runHealthCheckCommand(ns, containerdID, containerUser string, cmd []string, timeout time.Duration) (int, string) {
	res, err := runSimpleExec(context.Background(), ns, containerdID, cmd, containerUser, "", timeout)
	if res == nil {
		if err != nil {
			return 126, err.Error()
		}
		return 126, "exec failed"
	}
	output := strings.TrimSpace(res.stdout)
	if output == "" {
		output = strings.TrimSpace(res.stderr)
	}
	return res.exitCode, output
}

// getHealthcheckUser returns the stored container user for a healthcheck.
func getHealthcheckUser(dockerID string) string {
	healthcheckUsers.mu.RLock()
	defer healthcheckUsers.mu.RUnlock()
	return healthcheckUsers.byID[dockerID]
}

// stopHealthCheck stops the running health monitor for a container. The
// stored configuration is kept so the next start (docker start, docker
// restart, the restart policy) re-attaches the monitor.
func stopHealthCheck(dockerID string) {
	healthChecks.mu.Lock()
	if h, ok := healthChecks.byID[dockerID]; ok {
		close(h.stopChan)
		delete(healthChecks.byID, dockerID)
	}
	healthChecks.mu.Unlock()
}

// forgetHealthCheck stops the monitor and drops the stored configuration.
// Only container removal may call it.
func forgetHealthCheck(dockerID string) {
	stopHealthCheck(dockerID)

	healthcheckConfigs.mu.Lock()
	delete(healthcheckConfigs.byID, dockerID)
	healthcheckConfigs.mu.Unlock()

	healthcheckUsers.mu.Lock()
	delete(healthcheckUsers.byID, dockerID)
	healthcheckUsers.mu.Unlock()
}

// getHealthcheckConfig returns the stored healthcheck configuration for a
// container, or nil if none was configured.
func getHealthcheckConfig(dockerID string) *dockerHealthcheck {
	healthcheckConfigs.mu.RLock()
	defer healthcheckConfigs.mu.RUnlock()
	return healthcheckConfigs.byID[dockerID]
}

// getHealthState returns a copy of the current health state for a container,
// or nil if no healthcheck is configured.
func getHealthState(dockerID string) *dockerHealthState {
	healthChecks.mu.RLock()
	h, ok := healthChecks.byID[dockerID]
	healthChecks.mu.RUnlock()
	if !ok {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	state := h.state
	return &state
}

// formatHealthStatus returns a human-readable status string including the
// health state, used in Status fields like "Up 5 seconds (healthy)".
func formatHealthStatus(dockerID, baseStatus string) string {
	state := getHealthState(dockerID)
	if state == nil {
		return baseStatus
	}
	if state.Status == "starting" {
		return baseStatus + " (health: starting)" // as Docker words it
	}
	return fmt.Sprintf("%s (%s)", baseStatus, state.Status)
}

// nextHealthStatus applies one probe result. As in Docker, a container turns
// unhealthy only after `retries` consecutive failures — from starting and
// from healthy alike — and one success makes it healthy.
func nextHealthStatus(status string, ok bool, failures, retries int) (string, int) {
	if ok {
		return "healthy", 0
	}
	failures++
	if failures >= retries {
		return "unhealthy", failures
	}
	return status, failures
}

// publishHealthEvent emits Docker's "health_status: <status>" container
// event (docker events, compose).
func publishHealthEvent(ns, containerdID, status string) {
	attrs := map[string]string{}
	if meta, err := loadContainerMeta(ns, containerdID); err == nil {
		attrs["name"] = strings.TrimPrefix(meta.Name, "/")
		attrs["image"] = meta.ImageRef
	}
	publishAgentEvent(dockerEvent{Type: "container", Action: "health_status: " + status,
		Actor: dockerEventActor{ID: dockerID(ns, containerdID), Attributes: attrs}})
}

// nextHealthDelay is the wait before the next probe: StartInterval while the
// container is still starting inside its start period, Interval otherwise.
func nextHealthDelay(began, now time.Time, startPeriod, startInterval, interval time.Duration, starting bool) time.Duration {
	if starting && startPeriod > 0 && now.Sub(began) < startPeriod {
		return startInterval
	}
	return interval
}
