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
- Integration: first full run 149 passed / 1 failed (`k3s cluster`, a k8s
  timing flake — it passes standalone), second full run **150 passed / 4
  skipped / 0 failed**.
- Best measurement on a quiet moment: CLI median 92.0 ms (min 50.6, p10 58.2),
  API flow min 31.1 ms.

### The lesson worth keeping

On this path, latency was not in syscalls or container work — it was in
**poll intervals**. Three sleeps of 50–100 ms accounted for ~180 ms of a
~200 ms CLI run. Before optimising any phase, grep the hot path for
`time.Sleep`; each one is a latency floor for whoever waits behind it.

## 6b.1 measured (2026-11-16)

Every create asked containerd the same three questions about the same image.
The answers only change when the image does, so they are memoized:

| guest phase (`create`/`native` trace) | was | after | what it was |
|---|---|---|---|
| `image-ensure` | 1.43 ms | **0.12 ms** | `findImageLocally` walking the descriptor tree to prove the record is healthy in this namespace |
| `unpack` | 2.94 ms | **0.17 ms** | `img.Unpack` asking the snapshotter whether the layer chain is applied |
| `meta` | 0.55 ms | **0.12 ms** | `img.Spec` + `dockerConfigExtensions` re-reading the image config blob |
| `new-container` | 1.51 ms | 2.65 ms | unchanged code: it now pays the manifest/config reads that the skipped `img.Unpack` used to cache on the image object |
| **API `create` step** | **9.7 ms** | **4.6 ms** | |

What is memoized and where:

- **unpacked** per (image digest, requested platform) — persisted in
  `/var/lib/anvil/image-memo.json`, since it describes snapshots on the
  persistent disk and survives resume/restart;
- **present** per (namespace, canonical ref, platform) — persisted the same
  way; the stored digest is compared with one `GetImage` on every hit, so a
  tag move or a re-pull that changed the digest misses instead of skipping a
  check;
- **config facts** per (digest, platform) — in memory only: config,
  platform, emulated-platform warning and the Docker-only extensions
  (`Healthcheck`/`OnBuild`/`Shell`). The config blob is read once instead of
  four times per create.

Invalidation: `docker rmi`, image prune, system prune, pulls, and dangling-tree
refreshes drop the affected entries. containerd's GC can also remove unpacked
snapshots asynchronously without telling anyone, so a memo hit is never a
correctness dependency: `NewContainer` failing with a missing parent snapshot
clears the entry, unpacks once and retries, and the client never sees that
failure.

- API flow (`scripts/time_api_steps.py --runs 20`): create **4.6 ms**, total
  49.2 ms median (min 26.0). Second sample: create 5.0, total 60.3 (min 28.5).
- CLI (`scripts/time_run.py --runs 20`): median **92.0 ms**, **min 43.8 ms**
  (was median 92.0 / min 50.6) — no regression, and the floor moved down.
- Restart: after `make service-restart` the first create logged
  `layers of docker.io/library/alpine:3.19 already unpacked (memo)`, i.e. the
  persisted file was reused, not re-derived.
- Correctness: pull → run → rmi → run (re-pulls and works); `docker commit`,
  `docker save`, `docker export`, `docker system df`, `docker image prune`,
  and a container after `docker system prune` all pass.
- `make dev-integration`: **150 passed / 4 skipped / 0 failed**.

Known pre-existing quirk observed while verifying (not introduced here):
`docker image prune` reports dangling IDs as `deleted` but the records survive
because the prune path passes a digest to `canonicalizeImageRef`. It is
unrelated to this rung; the memo invalidation on that path is conservative.

## 6b.2 — direct rootfs mounting: no-go

### How the layer directories would be discovered

The chain-ID → on-disk mapping is owned by the **metadata snapshotter**, not
the content store. For every unpacked layer the metadata layer allocates a
backend key

    <namespace>/<sequence>/<chainID>

(`createKey` in `core/metadata/snapshot.go`) and the overlay backend stores
that layer's diff under

    /var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/<namespace>/<sequence>/<chainID>/fs

with `work/` beside it (the overlay snapshotter's `upperPath`/`workPath`). A
chain ID is `identity.ChainID(diffIDs[:i+1])` for the image's i-th diff ID;
the top layer's chain ID is the rootfs chain ID set in the image config blob's
`containerd.io/gc.ref.snapshot.overlayfs` label. The mapping itself lives in
containerd's metadata bolt database
(`/var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db`, bucket
`v1/<namespace>/snapshots/overlayfs/<snapshot key>`), which containerd holds
with bolt's exclusive lock. So "discover the layer dirs without the
snapshotter" means either reading containerd's private, locked state (copying
the DB while GC may be rewriting it) or asking the snapshotter API — the round
trip this rung wanted to remove.

### What depends on the per-container snapshot record

- **start**: `direct.go` mounts the rootfs from
  `snapshotter.Mounts(info.SnapshotKey)`;
- **`docker commit`**: `rootfs.CreateDiff` over the container's SnapshotKey +
  snapshotter (`image_commit.go`);
- **`docker export`, `docker cp`, `docker diff`**: `archive.go` and
  `container_fs.go` call `snapshotter.Mounts(SnapshotKey)` and
  `overlayLayers` parses the returned `upperdir=`/`lowerdir=` options;
- **fast shim**: creates the task with
  `client.WithSnapshotter`/`client.WithSnapshot` (`fastshim.go`);
- **`docker system df` / `docker ps -s`**: `snapshotter.Usage(SnapshotKey)`
  for the writable-layer size (`systemdf.go`);
- **delete**: `client.WithSnapshotCleanup` removes the container snapshot
  (`container_ops.go`, `bootfinalize.go`);
- **GC**: while a container exists, its snapshot record (and parent chain) is
  what pins the image's layer snapshots. `docker rmi` removes the image record
  and its `gc.ref.snapshot` label; without a container snapshot an rmi would
  let GC sweep the lowerdirs under a running container unless we added a
  separate pin (a lease or a dummy snapshot record — i.e. kept the record).

Only `docker save` is independent (it exports content-store blobs).

### Verdict

Mounting the rootfs directly would rework commit, export/cp/diff, system df,
fast-shim task creation, container deletion and GC pinning — seven consumers
across six files — with GC-race risk, to remove part of a ~1.5 ms
`new-container` phase whose spec build (`WithNewSpec`) remains either way.
**No-go. Keeping the snapshot record is the right outcome.**

A cheap follow-up stays available inside 6b.1's design, and now has a concrete
target: the ~1 ms of manifest/config reads that moved from `unpack` into
`new-container` when the unpack memo began skipping the call that populated
the image object's RootFS cache. Cache the rootfs diff IDs per (digest,
platform) and call `snapshotter.Prepare` directly instead of
`client.WithNewSnapshot` (or reuse the image object across creates), so every
consumer above keeps working unchanged. Not implemented here; the create
target was met without it.

## 6c measured (2026-11-16)

### The helper's cost, split (30 runs, `alpine true`, debug trace)

Per-phase medians of the Go init helper from the agent's spawn to the user
exec's "S" report, wall and calling-thread CPU (µs):

| phase | wall | CPU |
|---|---|---|
| process create + execve + Go runtime (stage A) | 512 | — |
| spec read (`config.json` JSON parse) | 201 | 201 |
| cgroup create + limits | 64 | 64 |
| confirm pipe, `os.Executable`, argv/env ptrs, dup confirm, report P | 32 | 31 |
| clone3 (new pid ns + `CLONE_INTO_CGROUP`) | 46 | 46 |
| **stage B execve + Go runtime** | **424** | — |
| stage B spec read (the same JSON parsed again) | 175 | 175 |
| stdio + namespaces | 30 | 30 |
| spec mounts | 110 | 110 |
| devices + masked/read-only paths | 75 | 72 |
| pivot + read-only root + sysctls + hostname + cwd | 44 | 42 |
| report R + gate release (agent side) | 333 | 9 |
| PATH resolution | 32 | 32 |
| oom, rlimits, credentials, capabilities | 25 | 23 |
| **seccomp assemble + install** | **332** | 332 |
| report fd dup | 3 | 2 |
| **execve + user process start + S report** | **236** | — |
| **sum** | **2672** | |

How much is process + Go runtime and how much is container work:

- **process + Go runtime: 936 µs (35%)** — two execs of a 2.8 MB static Go
  binary: 512 µs for stage A, 424 µs for the stage-B re-exec inside the new
  pid namespace.
- **spec parsing: 376 µs (14%)** — the same `config.json` read and parsed by
  both stages.
- **seccomp: 332 µs (12%)** — ~65 µs assembling the default profile's BPF
  program, ~265 µs in the kernel loading it (the load cost is paid by any
  implementation).
- **container work: ~660 µs** — namespaces 30, mounts 110, devices/masking
  75, pivot/sysctls/hostname 44, cgroup 64, clone 46, credentials/caps 25,
  PATH 32, execve 236.
- the 333 µs gate phase is the agent's own work (supervisor registration, the
  release write, and in this debug build the tmpfs→virtiofs trace copy), not
  the helper.

### Route chosen: a native (C) init helper fed by a precomputed plan

The numbers make the dominant removable terms, in order: the two Go runtime
execs (936 µs), the double JSON parse (376 µs), the seccomp assembly (65 µs
plus the ability to cache it), and the Go work itself.

- **Route A (raw child forked from the agent, no exec)** removes the last
  exec too (~145 µs) but puts the mount/pivot/credentials/seccomp work in a
  forked child of the multithreaded Go agent, where only raw syscalls are
  legal — the risk the `2026-11-16-rust-decision.md` analysis already called
  the highest in the ladder.
- **Route B (a tiny non-Go helper)** removes the Go runtime from both
  stages, replaces both JSON parses with one binary plan read, and lets the
  agent assemble and cache the seccomp program. It keeps the existing fd and
  report protocol unchanged, and the fallback stays trivial.

Route B was chosen: the measured saving is ~1.4 ms of the helper's 2.7 ms,
and the implementation is single-threaded C with no runtime constraints.

### What changed

- The agent builds **one init plan** per native start from the OCI spec
  (`guest-agent/init_plan_linux.go`): namespaces, mount syscalls with their
  precomputed flags/data, device nodes, masked/read-only paths, pivot,
  sysctls, hostname, cwd, argv/env, credentials, capabilities and the
  assembled seccomp BPF program. The plan is written to the bundle (tmpfs)
  and is the helper's only input, so the helper needs no JSON parser, no
  capability/rlimit tables and no seccomp assembler.
- **`/bin/anvil-init-native`** (`guest-agent/guest-shim/native/anvil-init.c`,
  ~65 KB static musl) executes the plan. Stage A clones stage B with
  `CLONE_NEWPID|CLONE_INTO_CGROUP` (the cgroup directory comes in as fd 8)
  and reports `P`/`S`/`D` exactly like the Go helper; stage B is a plain
  `clone(2)` child, **not a re-exec**, so the process that becomes the
  container's PID 1 is the user program. It joins the namespaces, runs the
  op list, reports `R`, waits at the gate, applies the attributes (seccomp
  strictly last) and execve's. The only runtime loop is PATH resolution.
- **Fallback**: `ANVIL_INIT_IMPL=go|native` pins the implementation (the
  guest's stage2 also reads `/mnt/anvil/.anvil-init-impl` from the share, so
  A/B runs need no rebuild). In auto mode (the default) the agent runs a
  **capability probe** once — clone3 with `CLONE_INTO_CGROUP`, a new pid
  namespace, private mount/uts namespaces, tmpfs/proc mounts, pivot_root,
  sethostname, no-new-privs and a seccomp load — and uses the native helper
  only when the probe passes **and** the plan is expressible for that
  container; otherwise the Go helper runs. A native start that fails after
  the probe falls back to the containerd task path like any direct-start
  failure. The Go helper stays in the initramfs and keeps the containerd
  fast-shim path (TTY, custom seccomp profiles, ...).
- The pure OCI helpers (capability/rlimit tables, mount-option mapping,
  safeJoin, standard devices, namespace flags, seccomp assembly) moved to
  `guest-agent/internal/specutil`, and cgroup creation/limit translation to
  `guest-agent/internal/cgroupv2`, so the agent and the helper share one
  copy of each.

### Two bugs the instrumentation found (both fixed)

1. **`dup3` returns the new descriptor, not 0.** The C helper's failure check
   treated success as an error, reported a stale `EINVAL`, and every start
   fell back to containerd. Go's wrapper hides this; raw syscalls do not.
2. **`open(O_CREAT|O_WRONLY)` on an existing lower-layer file triggers an
   overlayfs copy-up.** The Go helper only opens the mountpoint file when
   `os.Stat` says it is missing; the first C version opened it
   unconditionally, copying up every bind-mounted file (measured 0.4–0.7 ms
   each). After the fix the whole op list costs ~0.24 ms.

### After (30 runs, same host, native helper)

| phase | wall | CPU |
|---|---|---|
| process create + execve + libc startup | 147 | — |
| plan read (binary) | 23 | 22 |
| pipes + clone + born handshake + P report | 53 | — |
| stdio + namespaces | 39 | 38 |
| mounts + devices + masked/readonly + pivot + sysctls + hostname + cwd | 237 | 225 |
| report R + gate release | 302 | 6 |
| PATH resolution | 26 | 26 |
| oom, rlimits, credentials, capabilities, seccomp install | 284 | 278 |
| report fd dup | 3 | 2 |
| execve + user process start + S report | 191 | — |
| **sum** | **1306** | |

The agent now also spends **260 µs** building the plan and preparing the
cgroup before the spawn (the Go helper did the cgroup work itself; the plan
build and write are new).

### Before / after (back-to-back cold boots, 40 runs each)

| measurement | Go helper | native | Δ |
|---|---|---|---|
| helper spawn → user exec (`S`) | 2.729 ms | 1.306 ms | **−1.42 ms** |
| guest direct start (agent trace) | 6.13 ms | 4.52 ms | **−1.61 ms** |
| API `/start` step (40 runs) | 12.7 ms | 11.2 ms | **−1.5 ms** |
| API sum of step medians | 30.5 ms | 28.6 ms | −1.9 ms |
| API flow, per-run median / min | 49.4 / 25.9 ms | 55.7 / 25.7 ms | tail-dominated, see note |
| CLI `docker run --rm alpine true`, median / min | 91.3 / 46.1 ms | 90.9 / 45.3 ms | −0.4 / −0.8 ms |

The per-run API total is dominated by unrelated tail events on this host
(p90 250–290 ms; the `attach-eof` step alone swung 0.1→3.4 ms between
samples), so the `/start` step and the guest trace are the meaningful
comparisons: both show the expected 1.4–1.6 ms. The CLI number is dominated
by fixed costs (CLI startup, the `/wait` exit tail at 33–34 ms, attach).

### Verification

- Behaviour, on the dev instance with the native helper in use: `run`/`ls`,
  PATH exec, `echo hi | run -i`, `run -t` (containerd fast-shim path),
  `exec` and `exec -u nobody`, `exec -t`, `--memory 64m --cpus 0.5`
  (`memory.max` 67108864, `cpu.max` 50000 100000 inside), default seccomp
  profile (`Seccomp: 2`, `NoNewPrivs: 1`), `-u nobody` (`CapEff: 0`),
  masked/read-only paths (`/proc/keys`, `/proc/timer_list` are `/dev/null`;
  `/proc/sys` read-only), `--pid host` (containerd path), `--read-only`
  root, restart policy (`on-failure:2` → `RestartCount=2`, exit 7), port
  publishing (HTTP answer through the forwarded port), stop/rm, `inspect`,
  `top`. No stale per-container cgroups after exit.
- `make dev-integration`: **150 passed / 4 skipped / 0 failed**, with the
  native helper selected for the direct-eligible containers and the Go
  helper still serving TTY/non-eligible ones. The only direct-start
  fallbacks in the run are the two pre-existing cases below (a sysctl
  container and a dangling `/etc/localtime` bind), and both fail the same
  way in the Go helper.
- `cd guest-agent && go test ./...` and `go vet ./...` pass; `go vet -tags
  shimserver ./...` passes; `staticcheck` (CI toolchain, linux/arm64, both
  tag sets) is clean. The init-plan serializer has unit tests that decode
  the plan the way the C helper does, run on linux/arm64.
- Known pre-existing quirks, unchanged: a container with `--sysctl` fails the
  direct path with `EROFS` writing `/proc/sys/...` (the spec's read-only
  `/proc/sys` bind is applied before the sysctl writes, in both helpers), and
  a container whose `/etc/localtime` bind source is a dangling symlink fails
  at the mountpoint creation; both fall through containerd's fast shim to
  runc, exactly as the Go helper does.
