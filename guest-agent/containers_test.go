package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// nerdctl >= 2.1 prints a single JSON object from `inspect --format json`;
// older versions printed an array with one element. When nerdctl 2.3.x
// switched to the single-object form, the old parser silently returned
// not-ok and every `docker run` with attach spun through all 100 wait
// iterations (+7 s). Accept both formats — see IMPROVEMENTS.md §2.1.
func TestContainerStateFromInspectArray(t *testing.T) {
	stdout := `[{"State":{"Running":true,"Status":"running"}}]`
	running, status, ok := containerStateFromInspect(stdout)
	if !ok || !running || status != "running" {
		t.Fatalf("array form: got (%v, %q, %v)", running, status, ok)
	}
}

func TestContainerStateFromInspectSingleObject(t *testing.T) {
	stdout := `{"State":{"Running":false,"Status":"exited"}}`
	running, status, ok := containerStateFromInspect(stdout)
	if !ok || running || status != "exited" {
		t.Fatalf("single-object form: got (%v, %q, %v)", running, status, ok)
	}
}

func TestContainerStateFromInspectGarbage(t *testing.T) {
	for _, in := range []string{"", "not json", `{}`, `[]`} {
		if _, _, ok := containerStateFromInspect(in); ok {
			t.Errorf("containerStateFromInspect(%q) = ok, want not ok", in)
		}
	}
}

// AutoRemove is implemented by the guest-agent itself (not `nerdctl --rm`),
// keyed by Docker ID in an in-memory set — see ARCHITECTURE.md §4.3.
func TestAutoRemoveLifecycle(t *testing.T) {
	id := dockerID("testns", "ar-1")
	if isAutoRemove(id) {
		t.Fatal("fresh ID must not be marked")
	}
	markAutoRemove(id)
	if !isAutoRemove(id) {
		t.Fatal("marked ID must be visible")
	}
	unmarkAutoRemove(id)
	if isAutoRemove(id) {
		t.Fatal("unmarked ID must not be visible")
	}
	// Unmarking an unknown ID must not panic.
	unmarkAutoRemove("no-such-id")
}

// Exit codes of auto-removed containers are cached because the container
// may be deleted before /wait reads the task status. take must clear the
// entry so the code is returned exactly once.
func TestContainerExitCodeCache(t *testing.T) {
	id := dockerID("testns", "exit-1")
	if _, ok := takeContainerExitCode(id); ok {
		t.Fatal("fresh ID must have no cached code")
	}
	cacheContainerExitCode(id, 42)
	code, ok := takeContainerExitCode(id)
	if !ok || code != 42 {
		t.Fatalf("take = (%d, %v), want (42, true)", code, ok)
	}
	if _, ok := takeContainerExitCode(id); ok {
		t.Fatal("take must clear the entry")
	}
}

func TestAttachTrackerCounting(t *testing.T) {
	id := dockerID("testns", "att-1")
	attachBegin(id)
	attachEnd(id)
	// Drain with zero outstanding attaches must return immediately, not
	// burn the timeout.
	start := time.Now()
	waitForAttachDrain(id, 2*time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("drain with no attaches took %v", elapsed)
	}
	attachEnd(id) // must not go negative
	attachTracker.mu.Lock()
	n := attachTracker.counts[id]
	attachTracker.mu.Unlock()
	if n != 0 {
		t.Fatalf("attach count = %d, want 0 (no negatives)", n)
	}
}

// POST /containers/{id}/wait returns the cached exit code immediately for
// containers that were already auto-removed — the contract `docker run --rm`
// relies on to get a non-zero exit code (ARCHITECTURE.md §4.3).
func TestHandleContainerWaitCachedExitCode(t *testing.T) {
	id := dockerID("testns", "wait-1")
	cacheContainerExitCode(id, 137)

	req := httptest.NewRequest(http.MethodPost, "/containers/"+id+"/wait", nil)
	w := httptest.NewRecorder()
	handleContainerWait(w, req, id)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != `{"StatusCode":137}` {
		t.Fatalf("body = %q", got)
	}
}

// An unknown container must fail fast with 404 instead of hanging the CLI
// (resolveDockerID errors when no containerd socket / no match exists).
func TestHandleContainerWaitUnknownContainer(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/containers/doesnotexist/wait", nil)
	w := httptest.NewRecorder()
	handleContainerWait(w, req, "doesnotexist")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
