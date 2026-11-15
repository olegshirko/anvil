# Stage 5: direct container creation (in-agent supervision)

Date: 2026-11-16 · Branch: `worktree-bench-start-latency`
Supersedes Stage 4 (`2026-11-15-stage4-direct-supervision.md`) as the route
to Kiri-level latency. Stage 4's supervisor registry survives; its
"agent dials the shim socket" step does not — the shim process itself is
the cost.

## Why

Measured on the rebuilt dev instance (2026-11-16), API-shaped flow
(create→attach→wait→start→exit→inspect→rm), median 246.5 ms:

| Phase | ms | What it is |
|---|---|---|
| create | 11 | unpack check, snapshot, metadata |
| start RPC | 123 | CNI 12 + **containerd NewTask 72** + task start 1.3 + handler overhead ~37 |
| attach drain | 112 | output-final detection |
| inspect + rm | 17 | |
| `/wait?condition=removed` tail (CLI only) | ~100 | removal poll + synchronous CNI detach |

`NewTask`'s 72 ms is containerd spawning `/bin/anvil-fast-shim`, which
spawns a ttrpc server, which forks the init helper: three execs and a
socket handshake per container. Kiri has none of that — one `clone3` from
its agent. No amount of tuning inside containerd removes this; the shim
process must leave the hot path.

## Design

The guest-agent becomes the container supervisor for eligible containers.
containerd keeps what it is good at (image store, snapshots, unpacking);
it no longer learns about tasks.

```
docker CLI → guest-agent HTTP  ─┬─ create: snapshot + spec (containerd, local)
                                ├─ start:  CNI attach, spawn init helper, waitpid
                                ├─ wait:   agent's own exit registry (instant)
                                └─ attach: agent's own log sink (deterministic EOF)
```

### Components

1. **`supervise.go`** — registry of direct tasks: (ns, container id, helper
   pid, state, exit code, log path, wait channel), exit subscribers, and
   `runningTasks(ns) map[containerdID]pid` shaped like scanner's
   `namespaceRunningTasks` so port push merges without changes.

2. **`direct.go`** — the start path for eligible containers:
   - reuse the existinshared prepare step (snapshot/spec/meta, already in
     `createNativeContainer`),
   - CNI attach (unchanged),
   - spawn `/bin/anvil-fast-shim init -id <id> -bundle <bundle>` (the
     **same helper the shim uses**, its proven two-stage clone3/mounts/
     pivot/attrs/exec code untouched) with the container's io wired to
     agent-owned pipes (json-file logging done by a logger fed from the
     helper's report/stream protocol as today),
   - `cmd.Wait()` in a goroutine → registry exit → `/wait` answers, the
     docker `die` event fires, restart policy notices, auto-remove runs.

3. **Wait/attach**: `/wait` reads the registry, not containerd. Attach reads
   the agent's own log sink, whose EOF is deterministic (the agent owns the
   write end — no shim logger, no marker heuristics): the 112 ms drain goes
   away with the mechanism, not a tuning constant.

4. **Consumers**: scanner merges `supervisor.runningTasks(ns)` with
   containerd's task list; restart.go subscribes to registry exits (its 1 s
   containerd poll stays for runc-shim containers); exec uses the helper's
   `exec` mode against the registry pid (the shim's exec-apply machinery is
   reused as a child of the agent).

5. **Fallback**: every non-eligible container (privileged, custom runtime,
   custom seccomp profile, `--pid container:`, subpath mounts, secondary
   networks) and every direct-start failure keeps today's containerd+runc
   path, invisibly. `ANVIL_FAST_SHIM=0` disables the direct path wholesale.

### Ladder (each rung measured and committed separately)

- **5a** — agent spawns the helper directly (one exec instead of three, no
  ttrpc). Target: start ≤40 ms, API ≤120 ms.
- **5b** — fold the helper's startup into the agent's own raw fork window
  (moved `init.go` mount/namespace code, no exec of the helper).
  Target: start ≤15 ms, API ≤60 ms.
- **5c** — CNI pre-allocation at create, snapshot memo, `/start` handler
  trims, removal-path detach off the response. Target: API ≤10 ms,
  CLI ≤25 ms.

## Non-goals

- No changes to what a container *is* (namespaces, seccomp, mounts, cgroups
  stay the helper's proven behavior).
- No removal of containerd (images/snapshots stay).
- No TTY-path redesign in 5a (TTY keeps the timer-based attach end).

## Anti-goals

- A container that runs but whose exit the agent misses (the bug class we
  just fixed in the shim) — the registry must be crash-safe: at boot, any
  recorded direct task whose pid is gone is marked exited with the recorded
  code, and stale helpers are reaped.
- Restart policy, port forwarding, exec, healthchecks, compose, save/resume
  must behave identically for direct containers.
- Silent divergence between `docker inspect`'s state and reality.

## Measurement

`scripts/time_run.py` (CLI, 20 runs, 30 s per-run hang guard) and
`scripts/time_api.py` (API-shaped flow with per-step trace), on the dev
instance, before/after each rung. Integration: `make dev-integration`.
Hang count must stay 0/30.

## 5a implementation notes (verified feasibility)

- The shim's init helper is directly reusable: `exec.Command(<shim binary>,
  "init", "-id", id, "-bundle", bundle)` with ExtraFiles
  `[gateR, reportW, stdin, stdout, stderr]` and the report protocol
  `P <pid>` / `R` / `D <code>` / `E <err>`. The agent does not need any of the
  shim's ttrpc layer.
- The helper currently assumes the shim created the cgroup before it runs
  (`prepareCgroup`). 5a adds a helper flag (e.g. `-cgroup`) so the helper
  creates and removes its own cgroup, keeping all cgroup code in one place.
- The agent already has snapshotter access and `mount.All`
  (`guest-agent/snapshot_linux.go`, `container_fs.go`...), so it can prepare
  the container rootfs itself: snapshotter `Prepare` for the container id on
  the image's target digest, then mount at `<bundle>/rootfs`.
- The bundle (`config.json`) is written by the agent under the container's
  metadata dir; containerd no longer needs to be involved in task creation.
- containerd v2 has no shim grouping (checked), so keeping the shim in the
  path cannot avoid the per-container spawn — 5a is the right first rung.
