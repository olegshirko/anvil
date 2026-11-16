# Stage 4: Direct Supervision — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove containerd from the task hot path for fast-shim containers (`docker run --rm alpine true` API op ≤ 5 ms) while runc-shim containers keep today's path unchanged.

**Architecture:** The agent spawns and dials `/bin/anvil-fast-shim` directly over its ttrpc socket; an agent-side supervisor registry becomes the task source of truth. Every containerd-task consumer (scanner, restart monitor, exec, /wait, events, stats) learns to read both registries. containerd keeps container records (store, snapshots, labels).

**Tech Stack:** Go guest-agent, ttrpc client (from the containerd module), the Task 6–8 shim from the stages-1–3 plan.

**Spec:** `docs/superpowers/specs/2026-11-15-container-start-latency-design.md` (Stage 4 section) + `docs/superpowers/plans/2026-11-15-start-latency-stages-1-3.md` (prerequisite: Tasks 6–10 landed).

## Global Constraints

Same as the stages-1–3 plan (dev instance only, unit-tests green at boundaries, English comments, sole-author commits, bench gate per stage) plus:

- runc-shim containers must keep working bit-for-bit: every merged consumer has a containerd branch.
- The exit-code cache + run-generation guard (`takeContainerExitCode`, `beginTaskRun`) remain the single source of truth for /wait answers.
- `ANVIL_FAST_SHIM=0` disables the whole direct-supervision path; everything reverts to today's containerd flow.

## Review Focus

1. **Restart policy fires on fast containers** — a person running `docker run --restart on-failure alpine sh -c 'exit 1'` expects the restart loop; the supervisor exit event must reach `restartMonitor.update`. Test: Task 2's integration step.
2. **Port publish after fast start** — `-p 8080:80` on a fast-shim container must be pushed to the host exactly as runc containers; scanner merge misses = broken publish. Test: Task 3's integration step with `curl 127.0.0.1:8080`.
3. **docker exec on a fast container** — healthchecks and interactive exec go through the shim dial; a half-open shim socket must not hang exec forever. Test: Task 4's timeout + error-path unit tests.
4. **Cold boot with live fast shims** — resume/crash kills shims without Delete; stale supervisor state must not answer /wait or block ports. Test: Task 6's cold-boot integration step.
5. **Mixed fleet** — fast and runc containers running side by side: scanner/restart/exec/list each pick the right registry per container. Test: Task 7's mixed-fleet integration step.

---

### Task 1: Supervisor registry

**Files:**
- Create: `guest-agent/supervise.go`
- Test: `guest-agent/supervise_test.go`

**Interfaces:**
- Produces: `type fastTask struct { ns, id, socket string; pid uint32; state string; exitCode int }`; `supervisor` singleton with `register(t fastTask)`, `markExited(id string, code int)`, `remove(id string)`, `runningTasks(ns string) map[string]uint32` (containerdID → pid, shape-compatible with `namespaceRunningTasks` in scanner.go), `lookup(id string) (fastTask, bool)`, `subscribeExit() <-chan exitEvent` / `unsubscribeExit(ch)` (1-slot channel semantics copied from scanner's `offerLatest`).

- [ ] **Step 1: Write the failing tests** — register/lookup/remove; runningTasks filtered by ns and state; markExited fires exactly one exit event with the code.
- [ ] **Step 2: Verify they fail** — `go test -run TestSupervisor ./...` → FAIL
- [ ] **Step 3: Implement** (mutex-guarded map + exit-event fan-out)
- [ ] **Step 4: Verify** — `go test ./...` → PASS
- [ ] **Step 5: Commit** — `git commit -m "supervisor: fast-task registry with exit events"`

### Task 2: Agent spawns the shim directly; /start and /wait via supervisor

**Files:**
- Modify: `guest-agent/container_ops.go` (fast branch in startNativeTask), `guest-agent/containers.go` (waitContainerTaskCond fast branch)

**Interfaces:**
- Consumes: supervisor (Task 1); shim binary (stages plan Task 6–8).
- Produces: `spawnFastShim(ns, id, bundle string) (*shimConn, error)` in a new `guest-agent/shimclient.go` — ttrpc dial of the shim socket (client from the containerd module), Create+Start, returns pid. Fast branch: bundle prepared from the existing snapshot/spec work, shim spawned, task registered in the supervisor; /wait for a supervisor-known container blocks on the supervisor's exit (feeding the exit-code cache), never on containerd.

- [ ] **Step 1: Implement shimclient.go** (dial, Create/Start/Wait/Delete calls; `ErrShimGone` on socket loss)
- [ ] **Step 2: Wire the fast branch** with a single-point fallback: any error before the task is running → tear down shim, redo the start through the containerd path (client-visible behavior identical).
- [ ] **Step 3: Verify on the dev instance** — `make dev-rebuild ANVIL_BUILD_CONTEXT=anvil`; `time_api.py` full op ≤ 5 ms target tracking; `docker wait`, `docker run` (cached exit code) correct; then **restart-policy integration**: `docker run --restart on-failure alpine sh -c 'exit 1'` restarts (supervisor exit event → `restarts.update`; the 1 s poll untouched for runc).
- [ ] **Step 4: `make dev-integration` → pass; commit** — `git commit -m "supervision: agent-spawned fast shim, /wait via supervisor"`

### Task 3: Scanner merge

**Files:**
- Modify: `guest-agent/scanner.go` (buildState)

**Interfaces:**
- Consumes: `supervisor.runningTasks(ns)` (Task 1) — same shape as `namespaceRunningTasks`.

- [ ] **Step 1: Write the failing test** — `TestBuildStateMergesSupervisorTasks` (a supervisor-only task appears in PortMapState; a containerd task unchanged; the union dedupes by id).
- [ ] **Step 2: Verify it fails** — `go test -run TestBuildStateMerges ./...` → FAIL
- [ ] **Step 3: Implement** — buildState unions both maps before the port-label pass.
- [ ] **Step 4: Verify on the dev instance** — `-p 8080:80` on a fast container answers on the host (`curl`); `make dev-integration` → pass.
- [ ] **Step 5: Commit** — `git commit -m "scanner: merge supervisor tasks into port state"`

### Task 4: Exec via the shim dial

**Files:**
- Modify: `guest-agent/exec.go` (fast branch), `guest-agent/shimclient.go` (Exec/CloseIO/Kill exec helpers)

**Interfaces:**
- Consumes: `shimConn` (Task 2); today's hijack/exec plumbing unchanged above the task boundary.
- Produces: exec on supervisor-known containers goes shim-direct; on runc containers unchanged. Shim socket gone mid-exec → exec fails with a 404-shaped error (Docker's "no such exec"), never a hang: every shim call gets a 5 s deadline.

- [ ] **Step 1: Implement exec helpers on shimConn** (Exec create/start, io via the same FIFO/log contract)
- [ ] **Step 2: Wire the fast branch in exec.go** behind `supervisor.lookup`
- [ ] **Step 3: Write the failing test** — `TestExecDeadShimFailsFast` (removed socket → error within deadline, not hang); run → FAIL → implement the deadline wiring → PASS
- [ ] **Step 4: Verify on the dev instance** — `docker exec`, `-it`, and a healthcheck container all work on fast containers; `make dev-integration` → pass.
- [ ] **Step 5: Commit** — `git commit -m "exec: shim-direct path for supervised containers"`

### Task 5: Events, stats, inspect via supervisor

**Files:**
- Modify: `guest-agent/events.go`, `guest-agent/container_inspect.go`, `guest-agent/stats` path (fast branch reads supervisor state / shim Stats)

- [ ] **Step 1: Wire** — container events (start/exit/die) published from supervisor state transitions; inspect/stats read supervisor for fast containers.
- [ ] **Step 2: Verify** — `docker events --since` shows start/die for a fast run; `docker stats`, `docker inspect .State.{Status,ExitCode}` correct on the dev instance.
- [ ] **Step 3: Commit** — `git commit -m "events/stats/inspect: supervisor-backed fast branches"`

### Task 6: Boot, cold-boot, snapshot interplay

**Files:**
- Modify: `guest-agent/bootfinalize.go` (orphan sweep: kill stray shims, clear supervisor), `guest-agent/restart.go` (cold-boot resume path consults supervisor first, unchanged for runc)

- [ ] **Step 1: Boot sweep** — bootfinalize kills leftover anvil-fast-shim processes and socket files; supervisor starts empty (rebuilt on demand).
- [ ] **Step 2: Verify on the dev instance** — fast start → `kill -9` the daemon → `make service-start` (cold boot): `docker ps` clean, stale /wait answers correctly (container exited, metadata path), ports released (`PortCheckServer`), then a normal fast run works. Then `make dev-validate` robustness subset (save/resume, kill -9).
- [ ] **Step 3: Commit** — `git commit -m "boot: reap fast-shim orphans, supervisor rebuilds at boot"`

### Task 7: Mixed-fleet verification + Stage 4 bench gate

- [ ] **Step 1: Mixed fleet** — run one privileged (runc) and one fast container together on the dev instance: ports, exec, restart, events, logs, `docker ps -q` all correct simultaneously.
- [ ] **Step 2: Bench** — `python3 scripts/time_api.py --runs 15`
  Expected: API full op ≤ 5 ms (Kiri: 2.55 ms).
- [ ] **Step 3: `make unit-tests && make dev-integration`** → pass
- [ ] **Step 4: Update the spec's measurement appendix; commit with recorded numbers**
