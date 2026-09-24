package main

// Container state that must survive a stop/start cycle and must not leak
// from one run of a container into the next.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// docker stop, the task exit watcher and the restart path all stop the health
// monitor; only removal may drop the stored healthcheck, otherwise the next
// start runs the container with no health state and compose's
// `condition: service_healthy` never resolves.
func TestStopHealthCheckKeepsConfig(t *testing.T) {
	id := dockerID("testns", "hc-1")
	hc := &dockerHealthcheck{Test: []string{"CMD", "true"}}
	setHealthcheckConfig(id, hc, "postgres")

	stopHealthCheck(id)
	if getHealthcheckConfig(id) != hc {
		t.Fatal("stop dropped the healthcheck config")
	}
	if getHealthcheckUser(id) != "postgres" {
		t.Fatal("stop dropped the healthcheck user")
	}

	forgetHealthCheck(id)
	if getHealthcheckConfig(id) != nil || getHealthcheckUser(id) != "" {
		t.Fatal("forget must drop config and user")
	}
}

// The exit watcher of a previous run must recognize that it was superseded
// by a newer start of the same container.
func TestTaskRunGeneration(t *testing.T) {
	id := dockerID("testns", "run-1")
	first := beginTaskRun(id)
	if !isCurrentTaskRun(id, first) {
		t.Fatal("fresh run must be current")
	}
	second := beginTaskRun(id)
	if isCurrentTaskRun(id, first) {
		t.Fatal("previous run must be superseded")
	}
	if !isCurrentTaskRun(id, second) {
		t.Fatal("latest run must be current")
	}
	forgetTaskRuns(id)
	if isCurrentTaskRun(id, second) {
		t.Fatal("forget must drop the run counter")
	}
}

// Exit codes are cached by Docker ID. /wait addressed by name must resolve the
// name first and find the code of the exited container instead of blocking
// on a task that no longer runs.
func TestHandleContainerWaitByName(t *testing.T) {
	startFakeContainerd(t, fakeNamespace, fixtureNS()...)
	did := dockerID(fakeNamespace, "c2-idle")
	cacheContainerExitCode(did, 3)
	t.Cleanup(func() { takeContainerExitCode(did) })

	req := httptest.NewRequest(http.MethodPost, "/containers/idle/wait", nil)
	w := httptest.NewRecorder()
	handleContainerWait(w, req, "idle")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != `{"StatusCode":3}` {
		t.Fatalf("body = %q", got)
	}
	if _, ok := takeContainerExitCode(did); ok {
		t.Fatal("/wait must consume the cached code")
	}
}
