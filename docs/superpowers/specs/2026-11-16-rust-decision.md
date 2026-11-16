# Should anvil move to Rust? — a decision note

Date: 2026-11-16. Written against measurements taken on this branch, not
against the usual language arguments.

## The evidence we already have

Our wins so far came from removing **system calls and processes**, not from
changing languages:

| change | gain | language-related? |
|---|---|---|
| deterministic log EOF (stage 1) | 450 → 0 ms attach drain | no |
| agent creates the container, no containerd task (5a) | 123 → 46 ms start | no |
| fixed fds below `NR_OPEN_DEFAULT` | −24 ms | no |
| `clone3(CLONE_INTO_CGROUP)` instead of a cgroup move | −21 ms | no |
| CNI attach parallel to helper setup | small, tail-bound | no |

What Rust would actually buy, measured on the same traces:

| term | now (Go helper) | Kiri (Rust, single process) | Rust gain |
|---|---|---|---|
| exec of the init helper | 1.14 ms | 0 (fork in-process) | ~1.1 ms |
| init's own work (mounts, devices, pivot, attrs, seccomp) | 1.6 ms | ~0.4–0.8 ms | ~0.8–1.2 ms |
| container init RSS | 896 kB | ~900 kB (their whole agent) | none |
| binary in the initramfs | 13.4 MB | 0.9 MB | boot-time only |

So a Rust init is worth **~2 ms of a 16 ms guest start** — and after 6a
(networking without the CNI plugin) the guest start is expected to be ~4–5 ms,
where 2 ms is a third of it. That is the one place where the language is a
real, measurable lever: **Go cannot run the mount/pivot work in a forked child
of its multithreaded runtime**, so the helper must be a separate exec; Rust
can do it in-process.

## Where Rust would not help

- **Networking (6a).** The cost is plugin *process spawns* and `iptables`
  execs, not Go. Go's netlink/nftables libraries issue the same syscalls a
  Rust one would; the plan is already in place.
- **Create (6b).** The cost is containerd round trips and unpack checks.
  Removing them is architectural, language-agnostic.
- **HTTP/vsock per call (0.32 ms).** Measured; not a language artifact.
- **The host daemon (Swift).** It is not on the measured path.
- **Full rewrite of the guest agent.** Would cost months (Kiri's agent is a
  set of crates with their own wire protocol, image store, and DNS/DHCP
  servers) and buys nothing measurable until the architectural terms above
  are gone.

## Options

| option | scope | cost | gain | risk |
|---|---|---|---|---|
| **A. No Rust** — finish 6a/6b/6d in Go | ~1–2 weeks of rungs | low | 141 → ~90–100 ms API | low |
| **B. Rust container init only** | ~300–500 lines: spec → mounts/pivot/caps/seccomp → exec, speaking the existing gate/report protocol | ~1 week incl. cross-compile in the initramfs build (cargo + musl/zig) | ~1.5–2 ms of start; removes the Go runtime from every container init | low–medium: it must reproduce the helper's proven semantics exactly |
| **C. Rust networking module** | netlink/nftables | ~1 week | ~0 (same syscalls) | not worth it |
| **D. Full guest agent in Rust** | rewrite of 100+ files | months | nothing until A/B are done | very high |

## Recommendation

1. **Finish A first.** 6a is the largest measured term (10–60 ms), and 6b is
   ~8 ms; both are Go work. Any Rust decision taken before them is blind.
2. **Then B, if the numbers still matter.** After 6a/6b, the guest start is
   dominated by the helper exec + its Go stage B; a Rust init removes both and
   is contained enough to review in one sitting. It also shrinks the initramfs
   payload for the container init from 13.4 MB to under 1 MB, which shortens
   `make dev-rebuild` and boot.
3. **Do not do C or D now.** Neither is where the milliseconds are.

## What to verify before committing to B

- Cross-compilation of a static `aarch64-unknown-linux-musl` (or zig-linked)
  binary inside the existing initramfs build container, with the same
  reproducibility we have for Go.
- That the Rust init passes the same integration suite (150/0), especially the
  TTY/console, caps/`-u`, masked/readonly paths and seccomp ordering.
- A measured A/B on the same host under the same load: guest start with the
  Go helper vs the Rust init, p50 and p90.
