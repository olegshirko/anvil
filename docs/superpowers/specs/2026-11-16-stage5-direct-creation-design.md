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

## 5a results (measured, 2026-11-16, dev instance, `alpine true`)

- `docker run --rm` CLI: median 344 → **242 ms** (n=20, debug agent; host
  load ~3.5 during the run).
- API-shaped flow: median 246 → **212 ms** (n=15): create 12, start RPC 103
  (guest-side trace 87: CNI attach 13.6 + helper 68 + stage B/gate ~3),
  attach EOF **0 ms** (was 112), inspect 3, rm 68.
- Start is **not** ≤ 40 ms and the API flow is not ≤ 120 ms. The measured
  reason: the init helper's stage A costs ~67 ms on its own, the same work
  the fast shim did inside NewTask — the shim process and its ttrpc
  handshake were only ~30 ms of the old 123 ms start. Helper trace split
  (medians, 8 runs): read spec 0.17, prepare cgroup 0.07, **joinCgroup
  28.0**, **pre-clone setup (pipe/argv/env/dup) 40.1**, clone 0.14; stage
  B's entire join/mount/device/pivot work 1.6 ms. The cgroup-move cost and
  the pre-clone block are what 5b's in-agent fork window and
  `CLONE_INTO_CGROUP` remove.
- The attach drain is gone by mechanism, not a constant: the agent owns the
  helper's output pipes and writes the json-file log itself, so the eof
  marker lands within ~5 ms of the exit. The hijacked attach handler's
  unconditional 100 ms post-stream sleep was the last timer in the path and
  is removed.
- rm now pays the CNI DEL (~70–100 ms) that the attach drain used to hide;
  moving it off the response path is 5c's job, as the ladder says.
- 0 hangs and 0 failures in 40 back-to-back runs. One race the fast path
  exposed was fixed: `/wait?condition=removed` is armed before `/start`, and
  a run that finishes inside one 50 ms poll can be observed only after its
  removal — the waiter now answers the condition with the recorded exit code
  (a lost exit code made `docker run --rm ... exit 42` return 0 in ~1/5
  runs).
- TTY containers keep the containerd fast-shim path (their console copy is
  unchanged); privileged and non-eligible containers keep runc. Direct exec,
  healthchecks, restart policy, ports, events, pause, stats, top, export,
  diff, cp and cold-boot reconciliation all run against the registry.


## Measured rungs (dev instance, 2026-11-16, host ~2.5 load avg)

| rung | CLI median | API median | start RPC |
|---|---|---|---|
| baseline (containerd + fast shim) | 344 ms | 246 ms | 123 ms |
| 5a — agent spawns the helper directly | 242 ms | 192 ms | 91 ms |
| + fixed fds below NR_OPEN_DEFAULT (72476cb) | — | 160 ms | 67 ms |
| + clone3 CLONE_INTO_CGROUP (a2db6f7) | 192 ms | 128 ms | 46 ms |

Two waits dominated the helper's stage A and both were pure waits (~0.03 ms
of CPU): the first `dup3` onto a descriptor >= 64 (fd-table expansion,
20–50 ms) and the post-start cgroup self-move (27 ms). Neither existed in
Kiri because their agent never leaves the low-fd range and births the child
straight into its cgroup.

5b (forking the container init inside the agent, no helper exec) was
measured at a realistic 2–3 ms gain for the highest risk in the ladder — it
is not the route to the target. Remaining order, cheapest first: CNI
pre-allocation at create (−13 ms of start), create-path trims (snapshot
memo), and the exit-path CNI teardown competing with inspect.

## Correctness pass (post-5a)

The first integration run after 5a found 13 regressions in the direct path
(137 passed / 13 failed vs 150 / 0 before). All are fixed (b9ec423..35a0e3a)
and the suite is green — 150 passed, 4 skipped, 0 failed, twice, one of the
runs verified independently. Root causes worth remembering, because each is
behaviour containerd used to provide for free:

- **cgroup view**: the container's `/sys/fs/cgroup` was a bind of the guest
  hierarchy; a bind keeps the superblock root, so the private cgroup
  namespace never restricted it and `--memory`/`--cpus` were invisible to
  the workload. A private cgroup namespace needs a *fresh* cgroup2 mount.
- **controller enabling** walked bottom-up; `cgroup.controllers` only lists
  what the parent's `subtree_control` already enables, so the first
  container after every boot saw no limit files. Walk root→namespace.
- **`--memory-swap`** is the combined total; `memory.swap.max` takes the
  swap-only remainder.
- **`--pid host`** was eligible for a path that always clones a new pid
  namespace — eligibility must exclude every host-namespace-sharing flag.
- **credentials before capabilities** cleared the effective set, so the
  bounding-set drops failed with EPERM; runc's order (drop while root,
  PR_SET_KEEPCAPS, setuid, install sets, clear) is required.
- **exec** needed `chdir` after `chroot`, a `CloseIO` that waits for the
  stdin copier (else the last chunk is truncated), and a pty taken from the
  container's own devpts (TIOCSPTLCK(0) then TIOCGPTPEER), not the agent's.
- **live `docker update`** must write the direct task's cgroup v2 files
  (`pids.max` takes `max`, not -1).

One unreproduced flake: a single `long name hostname` mismatch (the guest's
hostname leaked) in the first full run; not reproducible in 25 sequence,
60 standalone, 200-exec or two full-suite iterations. Debug diagnostics were
added at direct start/exec (spec hostname, UTS namespace identity) so a
recurrence is attributable without another rebuild.

## Final measured state (2026-11-16)

- CLI `docker run --rm alpine true`: **556 → 194 ms** (min 140, p10 187).
- API-shaped flow: **495 → 147 ms** (min 92).
- Integration: 150 passed / 4 skipped / 0 failed.
- Remaining, largest first: CNI attach ~13 ms inside start, create ~15 ms
  (snapshot/unpack through containerd), removal teardown ~40–90 ms (now off
  the response path but still competing with inspect on the guest), and the
  helper's residual start work (~25 ms after the fd and cgroup waits were
  removed).
