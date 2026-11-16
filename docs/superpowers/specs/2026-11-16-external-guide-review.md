# Review: external optimization guide vs what we measured

Source: `~/Downloads/anvil-optimization-guide.md` (external proposal, 4
sections). Reviewed 2026-11-16 against the measurements taken on this branch
(`worktree-bench-start-latency`), not against theory.

Legend: **done** — already in the tree · **planned** — matches a stage-6 rung
· **new** — accepted, added to the plan · **wrong for us** — contradicted by
measurement · **irrelevant to the goal** — true, but not about per-container
start.

## §1 Out of the VM: drop containerd and runc

| Idea | Verdict | Evidence |
|---|---|---|
| containerd/runc is the main cost because of child processes | **partly right, partly wrong** | The 72 ms was containerd spawning the **shim** (three execs + ttrpc), not runc. Removing it (5a) cut start 123 → 46 ms, and the fd/cgroup fixes cut it to ~16 ms. |
| parsing `config.json` is a major cost | **wrong for us** | The helper trace measures spec read + unmarshal at **0.17 ms**. The 40 ms next to it was a `dup3` fd-table expansion, not parsing. |
| the agent itself sets up namespaces, pivot_root, cgroups, io | **done, with one caveat** | 5a does exactly this, via one exec of the init helper. Doing it *in the agent's own fork* is impossible in Go (a forked child of a multithreaded runtime may only run raw syscalls); measured gain of a full rewrite: **2–3 ms**. |
| stdout/stderr through epoll | **no gain** | Measured attach drain after the eof marker: **0 ms**; io is agent-owned pipes. |
| precompiled seccomp BPF instead of rebuilding per container | **new — accepted** | Assembly runs per container today; the whole of stage B (mounts + devices + pivot + attrs + filter) is 1.6 ms, so the filter alone is well under 1 ms. Cache key must be the capability set (the default profile is capability-aware). Cheap, safe. |

## §2 The macOS ↔ VM bridge

| Idea | Verdict | Evidence |
|---|---|---|
| direct vsock without extra proxies, a pool of open streams | **partly new** | `GET /_ping` costs **0.32 ms** per call through the host proxy (host → vsock → guest HTTP → back). Kiri's whole host side is ~0.75 ms for 4 calls (~0.19/call). Our lifecycle makes 6 calls ≈ 2 ms, so there is at most ~1 ms here. Worth a measurement rung, not a redesign. |
| virtiofs for binds, raw ext4/xfs disk for layers and overlay | **done** | The guest already keeps state on `containerd-disk.img` (ext4) and shares `/Users` over virtiofs. |
| warm memory snapshots (`saveMachineStateTo`) | **done** | `SnapshotManager` + `VMLifecycleManager`: resume-to-ready is 611 ms vs 847 ms cold. |

## §3 Networking inside the guest

| Idea | Verdict | Evidence |
|---|---|---|
| netlink instead of `ip`/`iptables` binaries: saves 15–30 ms | **planned (6a), and the diagnosis is right** | We use `libcni`, which **execs plugin binaries** (bridge, portmap, firewall, loopback) and those exec `iptables`. Measured CNI attach: **10 ms median, 60 ms tail** — the single largest unbounded term in start. Kiri's whole veth+bridge-attach is 2.2 ms. |
| pool of pre-created veth pairs, moved into the netns at start | **new — accepted into 6a** | veth create + attach is 2.2 ms even for Kiri; a warm pool can move most of that off the path (refill asynchronously). Constraint: a *created-not-started* container must still hold no IP, so pool entries get their address only at start. |

## §4 A minimal arm64 kernel (~8 MB, boots in 40–60 ms)

**Irrelevant to this goal.** That is *boot* time, and we resume from a
snapshot (611 ms to daemon-ready) instead of booting. Per-container start
never touches the kernel image. Kiri's kernel patches exist for a different
reason (virtio-pci reset waits, GSO) and would matter for resume, not for
`docker run`.

## What this changes in the stage-6 plan

- 6a gains the **veth pool** (pre-created pairs, address assigned at start).
- 6b gains **cached seccomp BPF** (keyed by capability set).
- 6e becomes a measurement first: is our 0.32 ms/call the guest HTTP or the
  host proxy? If the proxy, a connection pool is worth ~1 ms of 141.
- Nothing else moves: the file's §1 and §4 rest on assumptions our own traces
  contradict.

## The one thing the guide gets right that we already did

Its central claim — "the container runtime layer, not the VM, is the cost" —
is exactly what stages 1–5 measured and removed: 556 → 192 ms CLI. The
remaining 141 ms of the API flow is now (in order) the CNI plugin path, the
containerd snapshot/create work, and the agent's per-request handler work —
none of which the guide's §4 addresses.
