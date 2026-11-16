# Stage 6: toward single-digit milliseconds

Date: 2026-11-16 · Branch: `worktree-bench-start-latency`
Predecessors: `2026-11-15-container-start-latency-design.md` (stages 1–3),
`2026-11-16-stage5-direct-creation-design.md` (stage 5 / rung 5a).

## Where we stand (measured, dev instance, 2026-11-16)

| | project start | now | Kiri |
|---|---|---|---|
| CLI `docker run --rm alpine true` | 556 ms | **192 ms** (min 137) | 14.2 ms |
| API-shaped flow | 495 ms | **141 ms** (min 105) | 2.55 ms |
| guest-side start (direct path) | — | **16 ms** median | — |
| integration suite | — | 150 pass / 0 fail | — |

Inside one sampled run the guest start trace was: netns 0.09, spec 0.17,
rootfs 0.36, spawn 0.79, init-born 1.57, init-ready 2.18, **CNI 60.6**,
gate 0.5, init-exec 0.75 — i.e. **the bare container start is ~5 ms and the
bridge-plugin CNI attach is the whole remainder, with a 10→60 ms spread**.

So the remaining time is no longer "container start" in any meaningful
sense. It is three things: the CNI plugin path, containerd's snapshot/create
work, and teardown.

## Goal

- API-shaped flow **≤ 15 ms** (stretch ≤ 5 ms), CLI **≤ 40 ms**.
- Keep: 150/0 integration, 0 hangs, runc fallback for ineligible
  containers, `ANVIL_FAST_SHIM=0`.

## Non-goals

- No change to container semantics (namespaces, seccomp, mounts, cgroups,
  cgroup namespace root, TTY) — the helper's proven code stays.
- No removal of containerd's image store or of runc as the fallback.

## Anti-goals

- A created-but-not-started container must not hold an IP, an endpoint or
  a port binding (docker semantics; the integration suite checks it).
- Networking must keep: `-p` on all interfaces, `127.0.0.1:` bindings, DNS
  by name/alias, `--network container:`, secondary networks, host/none
  modes, IPv6 containers, compose labels, the port scanner's view.
- No silent fallback that hides a broken path: a failed fast path must
  fall back *and* log in debug builds.

## Rungs

### 6a — networking without the CNI bridge plugin (target: start ≤ 8 ms, tail gone)

The bridge plugin costs 10–60 ms per container (veth, bridge port, IPAM
files, iptables chains, per-container masquerade) and it is the last
unbounded term in the hot path. Kiri does the same work with hand-written
rtnetlink and nf_tables messages.

- Agent-side `directnet.go`: create the veth pair, move one end into the
  container's netns, set the address (deterministic allocation from the
  network's pool, reusing the existing IPAM bookkeeping), add the default
  route, and install exactly the per-container nf_tables/iptables rules the
  plugin installs today (port DNAT for `-p`, the masquerade rule the plugin
  adds per container).
- The per-network, shareable rules (bridge setup, subnet masquerade) are
  installed once per network, not per container.
- Keep the CNI path behind the same interface and compare outputs in an
  integration-only switch (`ANVIL_CNI=plugin`) until both produce the same
  state for every networking test; then make the fast path the default.
- Must keep: IP release on stop (the same claim/release discipline as
  `claimNetInfo`/`awaitNetRelease`), the port scanner's contract, and the
  events (`connect`/`disconnect`).

### 6b — create without containerd's snapshot service (target: create ≤ 4 ms)

`create` today: unpack check 3.2 ms, `NewContainer` (snapshot + spec) 1.5 ms,
metadata 0.6 ms, image lookups ~1.7 ms.

- Mount the image's unpacked layers directly (overlay with the content
  store's layer directories as lowerdirs) instead of asking the snapshotter
  for a prepared snapshot; keep the snapshotter only for `docker commit`/GC.
- Memoize image→layer paths and image config in the agent (invalidated on
  pull/remove/GC), so create does not re-read containerd metadata.
- Keep `docker commit`, `docker save`, GC and `system df` working off the
  same source of truth (they read the snapshotter/store today).

### 6c — no separate init process (target: start ≤ 4 ms)

Rung 5b was rejected because the mount work would have to be rewritten as
raw syscalls *for the whole spec*. With 6a in place the remaining per-start
process cost is the helper exec (~1.1 ms) plus its stage B Go work
(~1.6 ms) — small, but it is also the last thing that makes a container's
init a child of a helper rather than of the agent.

- Precompute the entire mount/namespace/cgroup script in the agent from the
  spec (all inputs are known once the rootfs is mounted), then fork a raw
  child that executes a fixed, prebuilt syscall sequence and `execve`s the
  user process. No helper exec, no Go code in the child.
- Keep the helper as the fallback for specs the script does not cover
  (measured, not guessed: anything with a mount type the script lacks).
- This rung is optional if 6a+6b already meet the goal; it exists because
  it removes the helper process from `docker top`, `ps` inside the guest and
  the exit-watch chain.

### 6d — teardown and inspect off each other's way (target: API ≤ 15 ms overall)

- Removal answers as soon as the record is gone (done in 5a) — the CNI DEL
  must not share the guest's RTNL/iptables locks with a client's inspect.
  With 6a's lock discipline this becomes a queue, not a contention point.
- `inspect` measured 39 ms in-flow vs 5 ms isolated; confirm it drops once
  the detach no longer contends, otherwise memoize the record read.

## Measurement

`scripts/time_run.py --runs 20` (hang-guarded) and `scripts/time_api.py
--runs 15` (per-step trace) on the dev instance after each rung; the
guest-side phase table comes from the direct-path trace in
`guest-agent.log`. Integration: `make dev-integration` — 150/0 unchanged is
the bar for every rung. Report medians *and* min/p90: the host is shared
with a 100 %-CPU system daemon, so tails say more than medians.

## Honest arithmetic

6a removes 10–60 ms (networking), 6b ~6 ms (create), 6d ~30 ms (teardown/
inspect contention). That lands the API flow near **15–25 ms**, i.e. 6–10×
from Kiri rather than 55×. The last factor needs 6c plus a leaner HTTP path
(the flow makes ~6 vsock calls; each costs 0.32 ms) and is not guaranteed.

