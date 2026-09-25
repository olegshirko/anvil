package main

// Container state that must survive a stop/start cycle and must not leak
// from one run of a container into the next.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// Docker resolves a reference as full ID, then exact name, then unique ID
// prefix. A container named "db" must win over another whose ID starts with
// "db", and an empty reference must match nothing.
func TestPickContainerRefOrder(t *testing.T) {
	var hexy containerRef
	for i := 0; ; i++ {
		c := containerRef{ns: "p", id: fmt.Sprintf("c%d", i), name: "other"}
		if strings.HasPrefix(dockerID(c.ns, c.id), "db") {
			hexy = c
			break
		}
	}
	named := containerRef{ns: "p", id: "named", name: "db"}
	cands := []containerRef{hexy, named}

	if got, err := pickContainerRef("db", cands); err != nil || got != named {
		t.Fatalf("name must win over ID prefix: got %+v, %v", got, err)
	}
	if got, err := pickContainerRef("/db", cands); err != nil || got != named {
		t.Fatalf("leading slash: got %+v, %v", got, err)
	}
	full := dockerID(hexy.ns, hexy.id)
	if got, err := pickContainerRef(full, cands); err != nil || got != hexy {
		t.Fatalf("full ID: got %+v, %v", got, err)
	}
	if got, err := pickContainerRef(full[:5], cands); err != nil || got != hexy {
		t.Fatalf("unique prefix: got %+v, %v", got, err)
	}
	if _, err := pickContainerRef("", cands); err == nil {
		t.Fatal("empty reference must not match")
	}
	twins := []containerRef{{ns: "a", id: "1", name: "web"}, {ns: "b", id: "2", name: "web"}}
	if _, err := pickContainerRef("web", twins); err == nil {
		t.Fatal("a name in two namespaces is ambiguous")
	}
}

// `docker run -d` sends /wait and disconnects right after /start. The
// aborted wait must not cache an exit code, or the next `docker wait`
// returns it immediately instead of blocking until the real exit.
func TestAbortedWaitDoesNotCacheExitCode(t *testing.T) {
	startFakeContainerd(t, fakeNamespace, fixtureNS()...)
	did := dockerID(fakeNamespace, "c2-idle")
	t.Cleanup(func() { takeContainerExitCode(did) })

	// Cancelled while the handler polls for the (never created) task, i.e.
	// after the container was resolved — the client hanging up.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/containers/"+did+"/wait", nil).WithContext(ctx)
	handleContainerWait(httptest.NewRecorder(), req, did)

	if code, ok := takeContainerExitCode(did); ok {
		t.Fatalf("aborted wait cached exit code %d", code)
	}
}
