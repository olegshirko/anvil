# Container start latency: catching up with Kiri

Date: 2026-11-15 · Branch: `worktree-bench-start-latency`

## Intent

Bare `docker run` latency is the project's headline metric. Kiri (a direct
competitor, Rust, vsock + custom kernel) publishes 2.55 ms for
create→start→exit→rm via the Docker API and 14.2 ms via the CLI. Anvil
measures **556 ms CLI / ~495 ms of the same API flow** on the same op
(`alpine true`, median of 20/15, dev instance, 2026-11-15). We are ~200×
behind on the API op. The goal is to close the gap in stages, without
trading away anvil's containerd-based robustness.

## Where the 556 ms goes (measured, per-phase instrumentation in guest-agent)

| Phase | ms | Notes |
|---|---|---|
| create API | 12 | unpack check 4, snapshot 1.7, meta 0.6, image ensure 2.4 |
| start API | 90–110 | **shim spawn 47**, CNI attach 13, task.Start 2.5 |
| **attach EOF lag** | **~450** | log follow: 2×100 ms poll debounce + 200 ms stopQuiet |
| inspect + rm | 15 | |
| container actually runs | 29 | exit observed at +29 ms; everything else is overhead |

Host proxy and vsock are innocent: `GET /_ping` is 0.3 ms, CLI floor
(`docker ps`) is 10 ms. The bottlenecks are all in the guest agent's
Docker-API emulation and containerd/shim path.

## Goals

- `docker run --rm alpine true` (CLI): 556 ms → **≤ 30 ms** (staged: 120 →
  40 → 30).
- Same op via API: ≤ 15 ms after Stages 1–3, **≤ 5 ms after Stage 4**
  (Kiri: 2.55 ms).
- Every stage keeps `make unit-tests` green and dev-instance integration
  suite passing; bench scripts (`scripts/time_run.py`, `time_api.py`) are
  the regression gate, run before/after each stage.

## Non-goals

- No Linux-host, Windows, Kubernetes, USB, or GUI features from Kiri.
- No new vsock channels or wire-protocol rewrites.

## Anti-goals (what would make this a failure even if faster)

- Breaking restart policy, port forwarding (scanner), exec, healthchecks,
  compose, or snapshot resume — all of these read authoritative state from
  containerd tasks and must keep working unchanged.
- Losing docker-log parity (json-file rotation, timestamps, tail/since,
  TTY console copying).
- Any latency trick that only helps the `alpine true` toy case but slows
  real services (postgres et al.) or compose up.

## Constraints

- Guest agent is PID 1 in an initramfs; every change rebuilds initramfs and
  invalidates the snapshot (`make dev-rebuild`).
- The shim never closes the stream fds of the shim-spawned binary-v2 logger
  (`--log-json`) at task exit — that logger never sees EOF. The FIFO-mode
  logger (stdin-attached non-TTY containers, `docker run -i`) is different:
  it reads the FIFOs by path and the shim closes them at process exit, so
  it exits naturally (logging.go: runFifoJSONLogger).
- `restart.go`, `scanner.go` (port push), `healthcheck.go`, exec and events
  all key off containerd tasks/events. The fast path must preserve the
  containerd task abstraction (a faster shim, not no shim) unless a stage
  explicitly migrates those consumers first.

## Stages

### Stage 1 — deterministic attach EOF (556 → ≤ 120 ms, low risk)

Today the attach/log follow loop ends a `docker run` only after two
consecutive 100 ms polls see the task exited, plus a 200 ms "quiet" window
(`stopQuiet = 2×partialFlushAfter`, `logging.go`). Replace the timer
heuristics with a deterministic signal:

- The logger (`guest-agent --log-json`, spawned by the shim) receives the
  container init PID on argv. It watches `/proc/<pid>` at ~10 ms; when the
  init disappears it flushes any partial line and appends a final
  `{"eof":true}` marker record to the json log (still under the rotation
  policy; markers are skipped by replay emitters).
- The attach follow loop ends on reading the marker — no debounce, no
  quiet window. `docker logs -f` gets the same semantics (marker ends the
  stream; the API layer maps it to EOF after emitting everything written).
- Poll cadence while the task runs drops to 50 ms only for containers with
  no marker support (old snapshots); everything else is marker-driven.
- Fallback: if the logger can't resolve the pid, today's timers stay as the
  safety net (the marker simply never appears).
- TTY containers keep today's `stopQuietTTY` timers end to end in Stage 1:
  the marker fires when the init pid dies, which is when the shim *begins*
  copying the console, so marker-driven EOF would truncate TTY tails. The
  marker is emitted only by the non-TTY loggers (binary-v2 on pid
  disappearance; the FIFO logger on its natural stream EOF, before exit —
  that also picks up `docker run -i`); `docker logs -f` and attach map the
  marker to EOF after emitting what precedes it.

### Builder-check resolutions (2026-11-15)

- Stage 2 shim gating (Oleg's call): **default-on with fallback**. The fast
  shim is the default for eligible containers — not privileged, no custom
  `--runtime`, no seccomp profile beyond the default, no `--pid container:`,
  no subpath mounts, no secondary networks at stage-2 entry — and runc-shim
  stays the fallback for everything else and on any fast-shim failure. An
  env kill-switch (`ANVIL_FAST_SHIM=0`) disables it wholesale. Eligibility
  is decided at NewTask time from the create request; falling back is never
  an error the client sees.
- CNI move: netns + veth pair are prepared at create (parallel with
  snapshot creation); the full CNI ADD (IP assignment, iptables, net.json,
  hosts files, connect events) stays at start with its existing
  self-healing and rollback. A created-never-started container holds no IP.
- Exit-code truth: the direct shim wait feeds the same exit-code cache and
  run-generation guard watchTaskExit uses today (single source of truth);
  containerd's task.Wait stays the fallback when the shim subscription
  fails (the shim deletes its socket at exit). restart.go keeps its
  containerd-state poll: a ~1 s restart-policy detection on fast-shim
  containers is accepted while /wait answers in < 5 ms.

### Stage 2 — fast task path (≤ 120 → ≤ 40 ms, medium risk)

`/start` costs ~90–110 ms: 47 ms shim spawn (containerd runc shim binary +
ttrpc handshake), 13 ms CNI (bridge + veth + iptables per container), ~10 ms
HTTP/gRPC roundtrips. Plan:

- Custom lightweight shim: implement the containerd runtime-v2 shim API
  (Create/Start/State/Wait/Kill/Delete/exec/stats) as a small static binary
  that sets up namespaces and cgroups with one clone3 into a prepared
  cgroup (`CLONE_INTO_CGROUP`), overlayfs mounts prepared by the agent
  before Create. Registered per-container via `WithRuntime` at
  `NewTask`-time only for a benchmark-gated profile (e.g. containers
  without exotic runtime needs), runc shim stays the default and fallback.
- CNI: pre-allocate the veth pair in create (parallel with snapshot
  creation) and/or hand-rolled rtnetlink+nf_tables setup, keeping CNI DEL
  semantics for teardown.
- Trim create: memoize `img.Unpack` per image+snapshotter (skip the 4 ms
  lease/verify dance when the image is already unpacked), parallelize
  rootfs-prep with netns creation.
- Exit event: agent subscribes to shim wait events directly (ttrpc) instead
  of the containerd event poll lag; `/wait` answers on the event, the 29 ms
  observed-exit becomes < 5 ms.

### Stage 3 — hot-path trims (≤ 40 → ≤ 30 ms)

- Guest HTTP: the Docker API handler creates per-request client instances
  and re-parses metadata files; pool them.
- create+attach+wait+start over one vsock round-trip each is already near
  minimal (0.3 ms ping); verify no accidental extra round-trips per phase.
- The CLI-shaped flow includes inspect+rm: DELETE teardown must not run CNI
  DEL on the response path — answer first, detach async (exit watcher
  already does; verify rm waits on nothing it shouldn't).

### Stage 4 — direct supervision (≤ 30 → ≤ 15 ms API; Oleg's call: in scope)

Remove containerd from the task hot path for fast-shim containers. The
agent spawns and dials the fast shim directly (ttrpc over its socket);
containerd keeps the container record (store, snapshots, labels, leases)
but never learns about the task. An agent-side supervisor registry becomes
the task source of truth:

- `supervise.go`: registry of fast tasks (ns, containerd id, shim socket,
  pid, state, exit code) with exit-event subscribers and a
  `RunningTasks(ns)` view. Built at start; rebuilt at boot by the existing
  orphan cleanup, which learns to reap stale shim sockets/processes.
- scanner: `buildState` merges `supervisor.RunningTasks` with containerd's
  `TaskService().List` (runc-shim containers) — port push unchanged.
- restart.go: supervisor exit events call the existing
  `restartMonitor.update(..., running=false)`; the 1 s containerd poll
  stays as fallback for runc-shim containers.
- exec: `docker exec`/healthchecks on fast containers dial the shim's
  ttrpc Exec directly (the shim owns init, so exec goes through it as
  before — only the containerd middleman drops out).
- /wait, events, stats and watchTaskExit read the supervisor; the
  exit-code cache and run-generation guard remain the single source of
  truth they are today.

Runc-shim containers keep the exact today path end to end; every consumer
works against both registries. Full parity arithmetic: create (snapshot +
spec, ~5 ms after Stage 3) + start (clone3, ~2 ms) + exit observed (~2 ms)
≈ Kiri's 2.55 ms regime; the residual gap is HTTP+snapshot work Kiri does
not have at all.

## What the builder decides

- Exact marker record shape and how `docker logs` replay skips it.
- How the logger learns the init PID (argv vs env vs fd).
- Whether Stage 2's CNI work ships before or after the custom shim.
- Poll intervals and any tunable constants, provided they keep the
  fallback timers.

## Measurement appendix (dev instance, 2026-11-15)

- `docker run --rm alpine true` CLI: median 556 ms (n=20, min 501, max 709)
- CLI-shaped API flow: median 495 ms (n=15) — create 12, start 89,
  attach-EOF +450, inspect+rm 19
- Guest phases: shim spawn (new-task) 46.8 ms, cni-attach 13.3 ms,
  task-start 2.5 ms, unpack 4.0 ms, new-container 1.7 ms
- `GET /_ping`: 0.3 ms · CLI floor (`docker ps`): 10 ms
- Kiri published: API 2.55 ms, CLI 14.2 ms (same op, median of 30)
