# Container Start Latency, Stages 1–3 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cut bare `docker run --rm alpine true` from 556 ms to ≤ 30 ms CLI / ≤ 15 ms API via deterministic log EOF, a fast shim, and hot-path trims.

**Architecture:** Stage 1 replaces the attach follow loop's timer heuristics with an eof-marker record the loggers write deterministically. Stage 2 adds a lightweight containerd runtime-v2 shim (clone3, no runc) selected at NewTask time for eligible containers. Stage 3 trims the create path (unpack memo, parallel phases, async rm). Every stage is gated by the bench scripts on the dev instance.

**Tech Stack:** Go guest-agent (PID 1), containerd v2 client + runtime-v2 shim packages (ttrpc), golang.org/x/sys clone3, CNI, Alpine initramfs.

**Spec:** `docs/superpowers/specs/2026-11-15-container-start-latency-design.md` (read it first; the staged targets, anti-goals and builder-check resolutions live there).

## Global Constraints

- Guest-agent only runs on Linux (PID 1); pure-Go helpers are unit-testable on darwin (`cd guest-agent && go test ./...`). Linux-only code is verified via `make dev-rebuild && make integration` on the dev instance — never the user's brew service.
- `make unit-tests` green and `make dev-integration` passing at every task boundary.
- English only in comments and commit messages; sole author Oleg Shirko, no attribution trailers.
- The shim is default-on with fallback (Oleg's call): eligibility decided at NewTask from the create request; `ANVIL_FAST_SHIM=0` kills it wholesale; any fast-shim failure falls back to the runc shim, never visible to the client.
- TTY containers keep `stopQuietTTY` timers end to end (builder-check resolution).
- Bench gate commands: `python3 scripts/time_run.py --runs 20` (CLI) and `python3 scripts/time_api.py --runs 15` (API) against `--context anvil-dev` after `make dev-rebuild`. Record numbers in the commit message of each stage's final task.

## Review Focus

1. **TTY console tail truncation** — a person running `docker run -t alpine sh -c 'echo hi'` expects the full console in `docker logs`; the marker must never end a TTY stream. Test: Task 4 pins the TTY opt-out.
2. **`docker run -i` after Stage 1** — stdin-attached non-TTY containers use the FIFO logger whose natural EOF must still produce the marker; a person expects the run to finish promptly. Test: Task 3 covers the FIFO-logger marker path.
3. **`docker logs -f` on a still-running container must not end** — follow mode stays open indefinitely for a live task; the marker ends it only for the dead one. Test: Task 3's follow test asserts a running task never returns.
4. **Fast-shim fallback invisibility** — a container failing the fast path must start identically via runc shim; the client never sees an error from the fallback. Test: Task 9's eligibility unit tests + integration start of a non-eligible (privileged) container.
5. **Snapshot resume after fast starts** — cold boot kills tasks; orphan cleanup must reap stale shim state or `docker ps`/start break after resume. Test: Task 10's cold-boot integration step.

---

### Task 1: EOF marker record type and replay skipping

**Files:**
- Modify: `guest-agent/logging.go` (logLine struct near the top; emitRecord in readTaskLog)
- Test: `guest-agent/logging_test.go`

**Interfaces:**
- Produces: `logLine.EOF bool` field (JSON `"eof"`); `eofMarker() logLine` helper returning `logLine{EOF: true}`. Marker records are written by loggers (Task 2) and consumed by the follow loop (Task 3).

- [ ] **Step 1: Write the failing test**

```go
func TestReadTaskLogSkipsEOFMarkerOnReplay(t *testing.T) {
	// temp json-file log with one real record then {"eof":true}
	// readTaskLog with follow=false must emit exactly the real record's line
	// and return nil; the marker must not surface as output.
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd guest-agent && go test -run TestReadTaskLogSkipsEOFMarkerOnReplay ./...`
Expected: FAIL (no EOF field / marker surfaces as garbage record)

- [ ] **Step 3: Implement**

Add `EOF bool \`json:"eof,omitempty"\`` to logLine; in emitRecord return early when `rec.EOF` (no emit, no since/until filtering).

- [ ] **Step 4: Run test to verify it passes**

Run: `cd guest-agent && go test -run TestReadTaskLog ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add guest-agent/logging.go guest-agent/logging_test.go
git commit -m "logging: eof marker record, skipped by replay"
```

### Task 2: Loggers write the marker deterministically

**Files:**
- Modify: `guest-agent/logging.go` (runJSONLogger: pid watch; runFifoJSONLogger: stream-EOF marker; taskLogURI: query params)
- Modify: `guest-agent/container_ops.go` (taskLogURI call site: pass init pid + enable marker only for non-TTY)
- Test: `guest-agent/logging_test.go`

**Interfaces:**
- Consumes: `logLine.EOF`, `eofMarker()` from Task 1.
- Produces: taskLogURI query args `--init-pid=<pid>` and `--eof-marker=1` (set only when the task is non-TTY); `watchInitPid(pid int, poll time.Duration) <-chan struct{}` — closed channel when `/proc/<pid>` disappears (pure helper, linux-build-tagged, decision logic factored into a darwin-testable func `initGone(procEntry string) bool`).

- [ ] **Step 1: Write the failing tests**

```go
func TestInitGoneDetects(t *testing.T)        // empty/absent /proc entry → true, "state" line → false
func TestTaskLogURICarriesMarker(t *testing.T) // non-tty → both args; tty → neither
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd guest-agent && go test -run 'TestInitGone|TestTaskLogURI' ./...`
Expected: FAIL

- [ ] **Step 3: Implement**

- container_ops: `taskLogURI(logPath, rot)` gains `initPID int, tty bool` params (update the one call site; the shim already knows the pid only after create, so the URI is built with pid 0 and rewritten post-Start — see the call-site note in container_ops.go's taskIO block; simpler: the shim passes the pid on the logger's argv per binary-v2 convention, so the query arg carries it and the logger needs no rewrite).
- runJSONLogger: when `--eof-marker=1` and `--init-pid` present, spawn the watcher; on channel close flush partial buffers then write `eofMarker()` before returning.
- runFifoJSONLogger: on stream EOF (both fifos closed) write `eofMarker()` before exit.
- TTY tasks get neither arg (watcher absent → today's behavior).

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd guest-agent && go test ./... && go vet ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add guest-agent/logging.go guest-agent/logging_test.go guest-agent/container_ops.go
git commit -m "logging: deterministic eof marker from pid watch / stream eof"
```

### Task 3: Follow loop ends on the marker

**Files:**
- Modify: `guest-agent/logging.go` (readTaskLog follow loop)
- Test: `guest-agent/logging_test.go`

**Interfaces:**
- Consumes: `logLine.EOF` (Task 1).
- Produces: marker-driven follow termination — `docker logs -f` and attach return as soon as the marker is read; running containers never see it (no marker written while the task lives).

- [ ] **Step 1: Write the failing tests**

```go
func TestFollowEndsOnMarker(t *testing.T)      // log with record+marker, follow=true, stop=nil → returns after emitting the record
func TestFollowRunsPastQuietWithoutMarker(t *testing.T) // live task, no marker, stop=nil, noStopWait short → only ends on deadline, not on quiet
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd guest-agent && go test -run TestFollow ./...`
Expected: FAIL

- [ ] **Step 3: Implement**

In the follow loop's record decode: `if rec.EOF { flush pending; return nil }` — before the stopFired/quiet logic. The stop/quiet path stays untouched as fallback.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd guest-agent && go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add guest-agent/logging.go guest-agent/logging_test.go
git commit -m "logging: follow ends deterministically on eof marker"
```

### Task 4: TTY keeps the old timers (pin the opt-out)

**Files:**
- Modify: `guest-agent/container_ops.go` (marker arg gating, from Task 2 — this task only pins it)
- Test: `guest-agent/api_test.go`

**Interfaces:**
- Consumes: taskLogURI gating from Task 2.

- [ ] **Step 1: Write the failing test**

```go
func TestTTYTasksGetNoMarkerArgs(t *testing.T) // setContainerTTY(did,true); build URI for that container → no eof-marker/init-pid args
```

- [ ] **Step 2: Run to verify it fails** — `go test -run TestTTYTasksGetNoMarkerArgs ./...` → FAIL

- [ ] **Step 3: Implement** — assert the Task 2 gating holds at the call site; if the gate leaks, fix the call site (non-TTY only).

- [ ] **Step 4: Run to verify it passes** — `go test ./...` → PASS

- [ ] **Step 5: Commit**

```bash
git add guest-agent/container_ops.go guest-agent/api_test.go
git commit -m "logging: pin tty opt-out from eof marker"
```

### Task 5: Stage 1 bench gate

**Files:** none (verification task)

- [ ] **Step 1: Rebuild + cold-boot the dev instance** — `make dev-rebuild ANVIL_BUILD_CONTEXT=anvil`
- [ ] **Step 2: Bench** — `python3 scripts/time_run.py --runs 20 && python3 scripts/time_api.py --runs 15`
  Expected: CLI median ≤ 120 ms (was 556), API full op ≤ 120 ms (was ~495).
- [ ] **Step 3: Integration** — `make dev-integration`
  Expected: suite passes (log/attach/TTY cases are the ones to watch).
- [ ] **Step 4: Record numbers + commit any doc touch-ups**

### Task 6: Fast shim skeleton (ttrpc service that answers)

**Files:**
- Create: `guest-shim/` (new Go binary, cross-compiled like guest-agent, packed into the initramfs by `scripts/build_initramfs_containerd.sh`)
- Modify: `scripts/build_initramfs_containerd.sh` (build + install `/bin/anvil-fast-shim`)
- Modify: `Makefile` (`rebuild-all` builds the shim binary)

**Interfaces:**
- Produces: `/bin/anvil-fast-shim` implementing the containerd runtime-v2 shim contract (binary name form `<id>=<binary>`; ttrpc TaskService) with State/Connect/Shutdown answering and Create/Start/Kill/Delete/exec returning `ErrNotImplemented` for now. Uses `github.com/containerd/containerd/v2/core/runtime/v2/shim` + `pkg/shim` scaffolding from the existing containerd dependency — no new module.

- [ ] **Step 1: Build the skeleton** — shim.Run wiring, empty TaskService methods. (Linux-only; no darwin unit test — the integration run is the test.)
- [ ] **Step 2: `make rebuild-all ANVIL_BUILD_CONTEXT=anvil`** — compiles for linux/arm64, lands in initramfs.
- [ ] **Step 3: Smoke** — cold boot; inside the guest (via `docker exec` on any running container, `ctr` shim socket list) or via the daemon log confirming the binary exists and is executable. Expected: initramfs contains `/bin/anvil-fast-shim`; no runtime errors at boot.
- [ ] **Step 4: Commit**

```bash
git add guest-shim scripts/build_initramfs_containerd.sh Makefile
git commit -m "shim: anvil-fast-shim skeleton (runtime-v2 ttrpc service)"
```

### Task 7: Create/Start via clone3

**Files:**
- Modify: `guest-shim/main.go` + `guest-shim/task.go` (Create, Start)
- Create: `guest-shim/mounts.go` (spec mounts → host mounts, pivot_root), `guest-shim/cgroup.go` (cgroup dir + CLONE_INTO_CGROUP)

**Interfaces:**
- Consumes: containerd bundle layout (the agent passes `--bundle` per shim v2 convention; the spec is in `bundle/config.json`).
- Produces: `Create(ctx, req)`: read spec, apply cgroup (mkdir under the agent-provided cgroup path), `unix.Clone3` with namespaces from the spec (CLONE_INTO_CGROUP), hold the child until Start (pipe handshake, like runc's init). `Start(ctx, req)`: release the child, return its pid. Console/FIFO io setup matches the binary-v2 log contract (the shim spawns `/bin/guest-agent --log-json …` exactly as the runc shim does — reuse the URI the agent passes in io).

- [ ] **Step 1: mounts.go** — apply spec.Mounts into rootfs, set up /proc /dev, pivot_root, re-exec init as pid-1-of-namespace (algorithm per kiri's kiri-init/linux/pid1.rs, in Go; this step is the bulk).
- [ ] **Step 2: cgroup.go** — mkdir cgroup2 dir, write spec limits (memory.max, cpu.max, pids.max), open fd for CLONE_INTO_CGROUP.
- [ ] **Step 3: Create/Start** — clone3 + handshake + pid report; seccomp/caps applied pre-exec from the spec (copy the logic shape from kiri's seccomp.rs: parse the OCI seccomp JSON into BPF via libseccomp-free hand-rolled program — gate: reuse `github.com/elastic/go-seccomp-bpf` only if vendoring is clean, else defer unconfined-only and mark eligibility excludes it, see Task 9).
- [ ] **Step 4: Verify on the dev instance** — `make dev-rebuild ANVIL_BUILD_CONTEXT=anvil`, then run a container explicitly forced onto the shim (env `ANVIL_FAST_SHIM=1` debug path, Task 9's gate temporarily inverted): `docker --context anvil-dev run --rm alpine true` exits 0, `docker logs` shows output for an `echo` container.
- [ ] **Step 5: Commit**

```bash
git add guest-shim
git commit -m "shim: create/start via clone3, mounts, cgroups"
```

### Task 8: Wait/Kill/Delete/exec/stats + IO parity

**Files:**
- Modify: `guest-shim/task.go` (Wait, Kill, Delete, Exec, Pids, Stats), `guest-shim/io.go` (io forwarding, console copy for TTY)

**Interfaces:**
- Produces: full TaskService. Wait signals exit (code cached for late waiters, mirroring watchTaskExit semantics); Kill signals; Delete tears down cgroup + netns refcount-safe; Exec forks an extra process into the same namespaces with io via the same FIFO/log contract; TTY console copy on exit lands in the log **before** any eof marker (TTY loggers carry no marker per Task 2 — the old stopQuietTTY covers the tail).

- [ ] **Step 1: Wait/Kill/Delete** — exit propagation + cleanup ordering (io shutdown before cgroup delete).
- [ ] **Step 2: Exec + Stats** — exec create/start via clone3 into the init's namespaces; stats from cgroup files.
- [ ] **Step 3: IO parity check** — `docker run -i`, `docker run -t`, `docker exec -it`, logs, attach all behave on a fast-shim container; `make dev-integration` passes.
- [ ] **Step 4: Commit**

```bash
git add guest-shim
git commit -m "shim: wait/kill/delete/exec/stats with io parity"
```

### Task 9: Eligibility + wiring (default-on with fallback)

**Files:**
- Create: `guest-agent/fastshim.go` (eligibility + gate)
- Modify: `guest-agent/container_ops.go` (startNativeTask: `WithRuntime` when eligible; fallback on any shim error)
- Test: `guest-agent/fastshim_test.go`

**Interfaces:**
- Produces: `fastShimEligible(req dockerCreateRequest, meta *containerMeta) bool` — false for: privileged, custom `--runtime`, non-default seccomp profile, `--pid container:`, subpath mounts, secondary networks, TTY-with-console-needs (none — TTY allowed, io is parity), `ANVIL_FAST_SHIM=0` kills all. `withFastShim(opts []containerd.NewContainerOpts) []containerd.NewContainerOpts` appending `WithRuntime("anvil-fast-shim", …)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestFastShimEligibility(t *testing.T) // table: each excluded feature → false; plain container → true; ANVIL_FAST_SHIM=0 → always false
```

- [ ] **Step 2: Run to verify it fails** — `go test -run TestFastShimEligibility ./...` → FAIL
- [ ] **Step 3: Implement** eligibility + `WithRuntime` wiring in createNativeContainer/startNativeTask; on NewTask/start error where runtime=="anvil-fast-shim": recreate the container record with the default runtime (fallback, logged via debugLog, never surfaced).
- [ ] **Step 4: Run to verify it passes** — `go test ./... && go vet ./...` → PASS
- [ ] **Step 5: Commit**

```bash
git add guest-agent/fastshim.go guest-agent/fastshim_test.go guest-agent/container_ops.go guest-agent/container_runtime.go
git commit -m "shim: default-on eligibility with silent runc fallback"
```

### Task 10: Boot/cold-boot hygiene + Stage 2 bench gate

**Files:**
- Modify: `guest-agent/bootfinalize.go` (orphan cleanup reaps stray anvil-fast-shim processes and socket files)

- [ ] **Step 1: Orphan sweep** — after containerd-socket wait, kill leftover `/bin/anvil-fast-shim` processes, remove their socket paths; runc-shim cleanup path unchanged.
- [ ] **Step 2: `make dev-rebuild ANVIL_BUILD_CONTEXT=anvil`** then a start→stop→cold-boot cycle; `docker ps`, `docker run`, restart policy (`docker run --restart on-failure`) behave.
- [ ] **Step 3: Bench** — `python3 scripts/time_run.py --runs 20 && python3 scripts/time_api.py --runs 15`
  Expected: CLI median ≤ 40 ms; /start API ≤ 20 ms (shim spawn gone).
- [ ] **Step 4: `make dev-integration`** → pass. Commit with recorded numbers.

### Task 11: Create-path trims (Stage 3)

**Files:**
- Modify: `guest-agent/container_runtime.go` (unpack memo; parallel rootfs-prep/netns)
- Modify: `guest-agent/images.go` or `anvilstore.go` (unpacked-set cache)
- Test: `guest-agent/container_runtime_test.go`

**Interfaces:**
- Produces: `imageUnpacked(ctx, img) bool` — cached per (image digest, snapshotter) in the agent store, invalidated on pull/load; create skips `img.Unpack` when true. `prepareCreate(id, hostname, req)` bundling rootfs-prep + netns creation run in parallel goroutines (both touch only disjoint paths).

- [ ] **Step 1: Write the failing test** — `TestImageUnpackedMemo` (warm image → true without containerd roundtrip; digest change → false).
- [ ] **Step 2: Verify it fails** — `go test -run TestImageUnpackedMemo ./...` → FAIL
- [ ] **Step 3: Implement** the memo + skip; parallelize rootfs-prep/netns via errgroup (join before first use of either result); per the spec's builder-check resolution, the veth pair creation moves to create alongside netns (full CNI ADD — IP, iptables, net.json — stays at start).
- [ ] **Step 4: Verify** — `go test ./...` → PASS
- [ ] **Step 5: Commit**

```bash
git add guest-agent/container_runtime.go guest-agent/container_runtime_test.go
git commit -m "create: unpack memo + parallel rootfs/netns prep"
```

### Task 12: Stage 3 bench gate + stage wrap-up

- [ ] **Step 1: `make dev-rebuild ANVIL_BUILD_CONTEXT=anvil`**
- [ ] **Step 2: Bench** — `python3 scripts/time_run.py --runs 20 && python3 scripts/time_api.py --runs 15`
  Expected: CLI median ≤ 30 ms, API full op ≤ 15 ms.
- [ ] **Step 3: `make dev-integration` + `make unit-tests`** → pass
- [ ] **Step 4: Update the spec's measurement appendix with the new numbers; commit**
