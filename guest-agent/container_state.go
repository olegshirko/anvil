package main

import (
	"sync"
	"time"
)

// In-memory per-container state kept by the agent (TTY, entrypoint, stop
// signal, auto-remove, attach counts, cached exit codes). Everything here is
// dropped on removal by forgetContainerState.

// autoRemoveContainers tracks Docker IDs that were created with AutoRemove.
// Removal happens in-agent after the exit code is captured so /wait can
// still read it.
var autoRemoveContainers = struct {
	mu  sync.RWMutex
	ids map[string]struct{}
}{
	ids: make(map[string]struct{}),
}

// containerTTYFlags remembers which containers were created with Tty=true:
// attach needs it to pick the raw (non-multiplexed) stream format, and
// inspect must return it.
var containerTTYFlags = struct {
	mu sync.RWMutex
	m  map[string]bool
}{
	m: make(map[string]bool),
}

// containerEntryPoints remembers WorkingDir/Entrypoint per container for
// inspect responses.
var containerEntryPoints = struct {
	mu    sync.RWMutex
	dirs  map[string]string
	eps   map[string][]string
	stops map[string]string
}{
	dirs:  make(map[string]string),
	eps:   make(map[string][]string),
	stops: make(map[string]string),
}

func setContainerStopSignal(dockerID, sig string) {
	containerEntryPoints.mu.Lock()
	if sig != "" {
		containerEntryPoints.stops[dockerID] = sig
	} else {
		delete(containerEntryPoints.stops, dockerID)
	}
	containerEntryPoints.mu.Unlock()
}

func getContainerStopSignal(dockerID string) string {
	containerEntryPoints.mu.RLock()
	defer containerEntryPoints.mu.RUnlock()
	return containerEntryPoints.stops[dockerID]
}

func setContainerEntryPointInfo(dockerID, workdir string, entrypoint []string) {
	containerEntryPoints.mu.Lock()
	containerEntryPoints.dirs[dockerID] = workdir
	if len(entrypoint) > 0 {
		containerEntryPoints.eps[dockerID] = entrypoint
	} else {
		delete(containerEntryPoints.eps, dockerID)
	}
	containerEntryPoints.mu.Unlock()
}

func getContainerWorkingDir(dockerID string) string {
	containerEntryPoints.mu.RLock()
	defer containerEntryPoints.mu.RUnlock()
	return containerEntryPoints.dirs[dockerID]
}

func getContainerEntrypoint(dockerID string) []string {
	containerEntryPoints.mu.RLock()
	defer containerEntryPoints.mu.RUnlock()
	return containerEntryPoints.eps[dockerID]
}

func setContainerTTY(dockerID string, tty bool) {
	containerTTYFlags.mu.Lock()
	if tty {
		containerTTYFlags.m[dockerID] = true
	} else {
		delete(containerTTYFlags.m, dockerID)
	}
	containerTTYFlags.mu.Unlock()
}

func getContainerTTY(dockerID string) bool {
	containerTTYFlags.mu.RLock()
	defer containerTTYFlags.mu.RUnlock()
	return containerTTYFlags.m[dockerID]
}

func markAutoRemove(dockerID string) {
	autoRemoveContainers.mu.Lock()
	autoRemoveContainers.ids[dockerID] = struct{}{}
	autoRemoveContainers.mu.Unlock()
}

func isAutoRemove(dockerID string) bool {
	autoRemoveContainers.mu.RLock()
	defer autoRemoveContainers.mu.RUnlock()
	_, ok := autoRemoveContainers.ids[dockerID]
	return ok
}

func unmarkAutoRemove(dockerID string) {
	autoRemoveContainers.mu.Lock()
	delete(autoRemoveContainers.ids, dockerID)
	autoRemoveContainers.mu.Unlock()
}

// attachTracker counts active attach connections per container. AutoRemove
// deletion waits for attaches to drain so a fast-exiting --rm container is
// not deleted before its output is replayed to the client.
var attachTracker = struct {
	mu     sync.Mutex
	counts map[string]int
}{
	counts: make(map[string]int),
}

func attachBegin(dockerID string) {
	attachTracker.mu.Lock()
	attachTracker.counts[dockerID]++
	attachTracker.mu.Unlock()
}

func attachEnd(dockerID string) {
	attachTracker.mu.Lock()
	if attachTracker.counts[dockerID] > 0 {
		attachTracker.counts[dockerID]--
	}
	attachTracker.mu.Unlock()
}

// waitForAttachDrain blocks until no attach connections remain for the
// container or the timeout elapses.
func waitForAttachDrain(dockerID string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		attachTracker.mu.Lock()
		n := attachTracker.counts[dockerID]
		attachTracker.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// containerExitCodes caches exit codes for containers that may be auto-removed
// before /wait can read their task status.
var containerExitCodes = struct {
	mu    sync.RWMutex
	codes map[string]int
}{
	codes: make(map[string]int),
}

func cacheContainerExitCode(dockerID string, code int) {
	containerExitCodes.mu.Lock()
	containerExitCodes.codes[dockerID] = code
	containerExitCodes.mu.Unlock()
}

func takeContainerExitCode(dockerID string) (int, bool) {
	containerExitCodes.mu.Lock()
	defer containerExitCodes.mu.Unlock()
	code, ok := containerExitCodes.codes[dockerID]
	if ok {
		delete(containerExitCodes.codes, dockerID)
	}
	return code, ok
}

// peekContainerExitCode reads the cached exit code without consuming it.
func peekContainerExitCode(dockerID string) (int, bool) {
	containerExitCodes.mu.RLock()
	defer containerExitCodes.mu.RUnlock()
	code, ok := containerExitCodes.codes[dockerID]
	return code, ok
}

// forgetContainerState drops every in-memory record kept per container.
// Without it each removed container (CI loops, `docker run --rm`) left
// entries behind in a dozen maps until the next cold boot.
func forgetContainerState(did string) {
	forgetHealthCheck(did)
	forgetTaskRuns(did)
	restarts.forget(did)
	unmarkAutoRemove(did)
	takeContainerExitCode(did)
	setContainerLinks(did, nil)

	containerTTYFlags.mu.Lock()
	delete(containerTTYFlags.m, did)
	containerTTYFlags.mu.Unlock()

	containerEntryPoints.mu.Lock()
	delete(containerEntryPoints.dirs, did)
	delete(containerEntryPoints.eps, did)
	delete(containerEntryPoints.stops, did)
	containerEntryPoints.mu.Unlock()

	attachTracker.mu.Lock()
	delete(attachTracker.counts, did)
	attachTracker.mu.Unlock()
}
