# Performance Improvement Plan — Anvil

## Overview

The anvil project (Docker alternative for macOS Apple Silicon) has several performance optimization opportunities, primarily in the guest-agent's interaction with containerd and the Docker API emulation layer.

## Priority 1: Persistent containerd client

**Impact:** Medium latency reduction on every Docker API call  
**Complexity:** Low  
**Files:** `guest-agent/images.go`, `containers.go`, `volumes.go`, `networks.go`, `restart.go`, `events.go`

- Currently ~40 `client.New(containerdSocket)` calls create/destroy gRPC connections per API request
- Scanner (`scanner.go:134`) already maintains a persistent client — extend this pattern
- Add reconnection logic for broken connections
- **Estimated gain:** 5-20ms per Docker API call

## Priority 2: Reduce nerdctl fork+exec

**Impact:** High for compose/workloads with many operations  
**Complexity:** High  
**Files:** `guest-agent/containers.go:476-491`, all 23 `exec.Command` call sites

- Every Docker API call forks a new `nerdctl` process (50-150ms overhead)
- A `docker compose up` with 5 services triggers dozens of sequential forks
- Use containerd Go client API directly for: container create/start/stop/kill, image pull/push, volume/network CRUD
- Keep nerdctl only for operations with no containerd API equivalent
- **Estimated gain:** Significant for compose workloads (could halve startup time)

## Priority 3: Event-driven port scanner

**Impact:** Medium CPU savings on idle systems  
**Complexity:** Medium  
**Files:** `guest-agent/scanner.go:26,146-168`

- Currently polls `buildState()` every 500ms unconditionally
- `events.go` already streams containerd events — subscribe and trigger re-scan on container start/stop/update
- Fallback periodic scan at longer interval (5-10s) for safety
- **Estimated gain:** 90%+ reduction in idle CPU usage

## Priority 4: Restart monitor connection reuse

**Impact:** Low-medium  
**Complexity:** Low  
**Files:** `guest-agent/restart.go:128-133,148-152`

- Creates new containerd client every second in `runRestartMonitor()`
- Reuse scanner's persistent client or maintain a separate one
- Cache namespace/containerdID mapping to avoid repeated nerdctl calls

## Priority 5: Namespace list caching

**Impact:** Low  
**Complexity:** Low  
**Files:** `guest-agent/volumes.go`, `networks.go`, `images.go`, `containers.go`

- Nearly every list/inspect function calls `NamespaceService().List()` independently
- Namespace set changes rarely (only on containerd restart)
- Add short TTL cache (30s) or invalidate on reconnection

## Priority 6: Buffer pooling in port forwarding

**Impact:** Low (only matters at high concurrency)  
**Complexity:** Low  
**Files:** `Sources/vz-runner/PortForwarder.swift:619-640`

- Allocates new 65536-byte buffer per connection
- Use buffer pool (`DispatchData` or manual free-list)
- Guest-side already uses Go's efficient `io.Copy` with pooled buffers

## Priority 7: Image save/load optimization

**Impact:** Low-medium (only for large images)  
**Complexity:** Medium  
**Files:** `guest-agent/images.go:332-370,562-605`

- Currently rounds-trips through temp files on `/var/lib`
- Investigate if nerdctl pipe issue is fixed in newer versions
- Use containerd `Export` API directly to stream without disk I/O

## Priority 8: Direct containerd exec for healthchecks

**Impact:** Low  
**Complexity:** Medium  
**Files:** `guest-agent/healthcheck.go:187-203`

- Forks nerdctl every health check tick (50-150ms overhead)
- Use containerd client's `Task.Exec` API directly
- Only matters for short-interval checks (5s)

---

## Implementation Order

1. **Persistent containerd client** — foundational change, enables all other optimizations
2. **Restart monitor reuse** — quick win once persistent client exists
3. **Namespace caching** — quick win once persistent client exists
4. **Event-driven scanner** — medium effort, good idle performance gain
5. **Reduce nerdctl forks** — high effort, highest impact for active workloads
6. **Remaining items** — as time permits

## Verification

- `make unit-tests` — Go + Swift unit tests (no VM needed)
- `make service-debug-rebuild && make integration` — full integration suite
- Benchmark with `make harness` before/after to measure improvement
- Profile with `go tool pprof` on guest-agent for CPU/heap hotspots
