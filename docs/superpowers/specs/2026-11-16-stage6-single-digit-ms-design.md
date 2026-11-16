# Stage 6: catching Kiri — a budget-driven plan

Date: 2026-11-16 · Branch: `worktree-bench-start-latency`
Supersedes the first draft of `2026-11-16-stage6-single-digit-ms-design.md`
(same file, rewritten after reading Kiri's published per-primitive budget).

## Kiri's own numbers (their `bench/results.json`, measured on Apple silicon)

Lifecycle measured: create `alpine` → run `true` → exit → remove, p50 of 30
runs after warm-up, driven through the Docker Engine API over the unix socket.

| Their measurement | ms |
|---|---|
| **Docker API lifecycle, bridge network** | **2.55** (p99 4.72) |
| Docker API lifecycle, `--network none` | 2.68 |
| docker CLI lifecycle | 14.15 |
| guest: namespaces + mounts + cgroup | 0.39 |
| guest: process lifetime (exec `true`, exit, reap) | 1.41 |

Their kernel-primitive budget (their measurement, Linux VM):

| Primitive | ms |
|---|---|
| fork + exec | 0.54 |
| + mnt/uts/ipc/pid namespaces + exec | 0.78 |
| + network namespace | 1.15 |
| cgroup v2 create + limit + remove | 1.23 |
| overlay mount + unmount | 0.36 |
| **veth create + attach to bridge** | **2.2** |
| **veth delete** (Docker does it before `--rm` returns) | **18.9** |

Three conclusions that shape everything below:

1. Their whole guest-side lifecycle is **1.8 ms**; the remaining ~0.75 ms is
   the host daemon, vsock and the API.
2. They pay the **18.9 ms veth delete asynchronously** — never on the
   response path. (We already do the same since 5c.)
3. They have **no containerd and no shim process** in the path: the image
   store is their own (`crates/kiri-image`), the overlay is mounted directly
   (0.36 ms), and containers are created by one `clone3` from their agent.

## Our numbers now (dev instance, same op, median of 20/15)

| Phase | ours | Kiri | gap |
|---|---|---|---|
| create (API) | 10.7 | included in 2.55 total | ~8 |
| — of which unpack check (containerd) | 3.2 | 0 (own store) | |
| — of which snapshot + record (containerd) | 1.5 | 0.36 (overlay) | |
| — of which image lookups | 1.7 | ~0 | |
| start (API→exec) | 25 (tail to 70) | 1.15 (ns+net) | ~20 |
| — of which CNI bridge plugin | 10 (tail 60) | 2.2 (veth) | |
| — of which init helper | 4 | 0.78 (fork+exec) | |
| — of which handler + HTTP | ~6 | ~0.75 (whole host side) | |
| attach / wait handoff | 1–25 (unexplained gaps) | — | |
| inspect in-flow | 39 (5 isolated) | — | contention |
| rm | 7–30 (veth delete already async) | 0 on the path | |
| API calls per lifecycle | 6 | 4 | |

**Total: 141 ms vs 2.55 ms** (~55×).

## The plan

Each rung is measured with `scripts/time_run.py --runs 20` and
`scripts/time_api.py --runs 15`; `make dev-integration` must stay 150/0.

### 6a — networking without the CNI bridge plugin

**Target: 10–60 ms → ~2 ms.** Kiri's own veth+bridge-attach is 2.2 ms; the
bridge *plugin* adds IPAM files, iptables chains and a per-container
masquerade rule, and its cost swings 10→60 ms under load.

- `directnet.go`: veth pair + rtnetlink address/route + nftables/iptables
  exactly as the plugin does today, with the per-network rules installed
  once per network instead of per container.
- Deterministic IP allocation against the existing bookkeeping
  (`claimNetInfo`/`awaitNetRelease` discipline, the `anvil/ports` label, the
  scanner's contract, `connect`/`disconnect` events).
- **veth delete stays async** (18.9 ms in Kiri's own measurement) and is
  already off our response path.
- **Warm veth pool** (from the external-guide review): pre-create pairs and
  move one end into the container's netns at start, refilling the pool
  asynchronously. Constraint: a created-not-started container must still
  hold no IP, so the address is assigned at start, not at pool creation.
- Parity gate: run the whole networking part of the suite with
  `ANVIL_CNI=plugin` and with the new path, and require identical observable
  state (IPs, `-p` on all interfaces, `127.0.0.1:` bindings, DNS by
  name/alias, `--network container:`, secondary networks, host/none, IPv6).

### 6b — create without containerd's snapshot service

**Target: 10.7 ms → ~2 ms.** Kiri's overlay mount is 0.36 ms; our create
spends 3.2 ms asking containerd whether the image is unpacked and 1.5 ms
creating a snapshot, plus 1.7 ms of image lookups.

- Mount the image's unpacked layer directories directly as overlay lowerdirs
  into the bundle's rootfs; keep the content store as the source of layers.
- Memoize image → (layer paths, config, exposed ports) in the agent,
  invalidated on pull/remove/GC.
- Cache the default seccomp BPF program (keyed by the capability set — the
  profile is capability-aware) instead of assembling it per container.
- Keep `docker commit`, `docker save`, `system df` and GC working: they read
  the snapshotter today, so either keep a snapshot record per container or
  move those readers to the same layer source (decide by what they need:
  `commit` needs a diff, which the overlay upperdir provides).

### 6c — the container init without a helper process

**Target: ~4 ms → ~1 ms.** Kiri forks and execs in 0.54 ms with namespaces
at 0.78 ms and no helper. We pay an exec (1.1 ms) plus the helper's Go
stage B (1.6 ms).

- Precompute the whole init script in the agent from the spec (mounts,
  masked/readonly paths, devices, sysctls, hostname, cwd — all known once
  the rootfs is mounted), then fork a raw child that runs a fixed syscall
  sequence and `execve`s the user process.
- Keep the helper as the fallback for specs the script cannot precompute.
- This is optional if 6a+6b+6d already land near the target; it is the
  rung that removes the last process from the path.

### 6d — wait/attach/inspect handoff

**Target: 30–40 ms of gaps → ~1 ms.** Two artefacts: `/wait` answering
20–25 ms after an exit already recorded, and `inspect` taking 39 ms in-flow
against 5 ms isolated. Both smell of the exit watcher's teardown sharing
locks with the request path.

- Instrument the wait/attach paths with the same wall/CPU split used for the
  helper trace, then make `/wait` answer from the registry without any
  polling on the direct path (the registry already has the exit).
- Confirm inspect's in-flow cost drops once 6a's lock discipline lands; if
  not, memoize the record read.

### 6e — fewer round trips (stretch)

Their measured lifecycle makes 4 API calls; ours makes 6 (create, attach,
wait, start, wait, inspect, delete). At 0.32 ms per vsock call, 2 extra
calls cost ~0.6 ms — small, but the CLI adds its own attach stream (the
CLI number, 14.15 ms for Kiri, is mostly CLI startup).

## Expected landing

| after | API | CLI |
|---|---|---|
| 6a | ~130 | ~185 |
| 6a+6b | ~120 | ~175 |
| +6d | ~90–100 | ~150 |
| +6c | ~80–90 | ~140 |
| stretch (6e, leaner handler) | ~60–70 | ~120 |

Honest reading: Kiri's 2.55 ms assumes **1.8 ms of guest work** and a
daemon that never crosses containerd. Our floor with containerd still
owning images is dominated by per-call work we have not yet removed, and
the biggest single remaining term — our per-request handler cost (~6 ms for
a start, ~3 ms for a create) — is not containerd at all: it is Go handler
work in the agent (meta JSON loads, image lookups, event publishing) that
their Rust daemon simply does not do per request.

So: **6a–6d plausibly lands 80–100 ms API (≈35× → ≈35…≈ 2.4× of Kiri per
rung accounting); exact parity needs a leaner request path (6e) and, at the
extreme, replacing containerd's image bookkeeping with our own (6b taken
all the way).** Every rung above is independently shippable and gated by the
integration suite; none of them risks container semantics.

## 6d measured (2026-11-16)

Three timers, each on the hot path, each costing 50–100 ms per run:

| where | was | now | what it did |
|---|---|---|---|
| `/wait?condition=removed` (what `docker run --rm` blocks on) | 100 ms | 5 ms | polled for the container to disappear |
| `/wait` before the task exists (armed before `/start`) | 50 ms + a containerd call per tick | 2 ms (containerd 20 ms) | polled for the task to appear |
| `waitForAttachDrain` before the auto-remove | 100 ms | 5 ms | waited for the client's attach stream to close |

Per-step medians of the API flow (`scripts/time_api_steps.py`, 20 runs):

    create 10.9 · attach-open 0.0 · wait-headers 3.9 · start 12.7
    attach-eof 1.3 · wait-exit 0.0 · inspect 3.6 · rm 4.6   TOTAL 59.7 ms (min 32.4)

- CLI: 192 → **114.8 ms** (min 50.3, p10 51.1) — bimodal on a loaded host.
- API: 141 → **59.7 ms** (min 32.4).
- Integration: 149 passed / 4 skipped / 1 failed (`k3s cluster: 'pod "probe"
  deleted'`), and that test passes standalone; a second full run is the
  discriminator between a k8s timing flake and an ordering dependency.

### The lesson worth keeping

On this path, latency was not in syscalls or container work — it was in
**poll intervals**. Three sleeps of 50–100 ms accounted for ~180 ms of a
~200 ms CLI run. Before optimising any phase, grep the hot path for
`time.Sleep`; each one is a latency floor for whoever waits behind it.
