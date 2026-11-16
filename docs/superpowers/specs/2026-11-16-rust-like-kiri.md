# "Rust like Kiri" — what it actually means for anvil

Date: 2026-11-16. Companion to `2026-11-16-rust-decision.md` (which covers a
targeted Rust init). This note answers the bigger question: **should anvil be
rewritten in Rust the way Kiri is written?**

## What "like Kiri" is, measured

Their repository, at HEAD:

| crate | files | what it is |
|---|---|---|
| `crates/kiri` | 88 | daemon, Docker API, CLI, GUI, bench |
| `crates/kiri-init` | 24 | guest PID 1: agent + container runtime (clone3, mounts, netlink, seccomp, DNS, relay) |
| `crates/kiri-win` | 20 | Windows/WSL front end |
| `crates/kiri-image` | 9 | registry client, blob store, references, archives (no containerd) |
| `crates/kiri-vmm` | 4 | Virtualization.framework from Rust (objc2) |
| `crates/kiri-proto`, `-share-proto` | 3 | host↔guest wire protocol, no_std |
| — | **149 .rs files**, 3.27 MB of Rust | |

Plus: a custom ~10 MB kernel with five patches, a hand-written
netlink/nf_tables networking stack, their own overlay/layer handling, a GUI
in Rust (HTML/JS for the window: 880 kB HTML + 356 kB JS), and Swift limited
to a 5 KB installer script.

Their headline numbers for exactly our op (`alpine` → `true` → exit → rm,
Docker API): **2.55 ms**, of which the guest does 1.8 ms
(namespaces+mounts+cgroup 0.39, process lifetime 1.41).

## What the same move would cost anvil

anvil today: 28 Swift files (host daemon), 203 Go files (guest agent +
shim), containerd + CNI + BuildKit, a 150-case integration suite, brew
distribution, snapshot resume.

A Kiri-style migration means replacing:

| anvil component | would become | why it is needed for Kiri's numbers |
|---|---|---|
| Go guest agent (203 files) | Rust agent | only the init part (~2 ms); the rest is architecture |
| containerd image store + snapshots | own store (their `kiri-image`) | removes ~8–10 ms of per-create round trips |
| CNI plugins (libcni) | hand-written netlink/nf_tables | removes 10–60 ms |
| containerd shim/helper path | single process | ~2 ms |
| Swift `vz-runner` (28 files) | Rust + objc2 | **nothing on the hot path** — it proxies bytes |
| stock kernel | custom kernel + patches | boot/resume, not per-container start |

Only two of those rows are language-specific, and both are worth **≈2 ms**.
Everything else is *architecture*, and we are already implementing it in Go
(stage 6: 6a netlink, 6b own overlay).

## What the language alone would buy, on our traces

| term | ours | Kiri | Rust gain |
|---|---|---|---|
| init helper exec + Go runtime | 1.14 ms | 0 | ~1.1 ms |
| init's Go work | 1.6 ms | ~0.4–0.8 ms | ~0.9 ms |
| per-request handler (not yet profiled) | ~3 ms of a start | unknown | unknown, likely <1 ms |
| HTTP/vsock per call | 0.32 ms | ~0.19 ms | none (protocol, not language) |

Total measured language-attributable gain today: **≈2 ms of a 16 ms guest
start**, which after stage 6a becomes ≈2 ms of a 4–5 ms guest start.

## Four ways to get Rust into anvil

| option | what it is | effort | gain |
|---|---|---|---|
| **A. Go architecture only** | finish 6a/6b/6d in Go | 1–2 weeks | 141 → ~90–100 ms API |
| **B. Rust init inside the Go agent** | 300–500 lines, same gate/report protocol | ~1 week | ~1.5–2 ms; initramfs payload of the init 13.4 MB → <1 MB |
| **C. Rust runtime module, Go agent** | Rust binary owning overlay + netlink for eligible containers; the agent keeps the Docker API, metadata, images | 3–5 weeks | the architecture gains of 6a/6b without rewriting the agent |
| **D. Full Kiri-style rewrite** | 149-file-scale Rust daemon + agent + image store; drop containerd; own kernel | months | ~2–4 ms over option A once A is done |

## The honest comparison

If the only goal is the number, **D is not justified by the measurements**:
after A the remaining gap is dominated by things D also has to pay (per-call
HTTP, process lifetime) and by ~2 ms of language-attributable init work that
B already takes.

What D *would* buy, and A–C do not:

- one language across host and guest (Kiri's Swift is 5 KB; ours is 28 files);
- a shared codebase for macOS, Windows and Linux hosts (they run the same
  Rust agent on the Linux host with no VM);
- memory: their agent is ~900 KB static and the guest runs one process, ours
  carries containerd (~40 MB RSS in the VM) plus a Go helper per container;
- a GUI and menu bar in the same tree;
- no dependency on containerd/CNI/containerd-shim behaviour, which is also
  where most of our recent bug classes came from (cgroup view, exec plumbing,
  seccomp ordering).

The last point is the strongest argument for D — not speed. Every regression
we fixed after 5a was behaviour containerd used to provide implicitly.

## Recommendation

1. **Do A now** (6a/6b/6d). It is the same architecture Kiri has, in the
   language we already have, and it is where 20–120 ms live.
2. **Then B** — the only place Rust is a direct, measurable lever.
3. **Treat C/D as a product decision, not a performance one.** If anvil should
   eventually be one Rust codebase with its own image store (for Windows/Linux
   hosts, memory footprint, or to stop chasing containerd semantics), then C is
   the natural first step: it moves the hot runtime into Rust without throwing
   away the Docker-API layer, the 150-case suite or the distribution.
4. **Never bundle a full rewrite with a latency goal.** The measurements say
   the latency goal is met by architecture; a rewrite bought with that promise
   would be paid for in months and delivered before the numbers.

## If C is chosen, the order that keeps it honest

1. Rust `runtime` crate: spec → clone3/mounts/pivot/caps/seccomp → exec, plus
   netlink networking and overlay mounting — CLI-testable inside the dev VM
   with `secprobe`-style harnesses, no daemon needed.
2. The Go agent shells out to it for eligible containers (same switch as
   today's `ANVIL_FAST_SHIM`), falling back to the current helper.
3. A/B on the same host under the same load; the integration suite must stay
   150/0 for both paths.
4. Only if that holds: move image/layer bookkeeping across, then retire the
   containerd runtime path — one consumer at a time, exactly like the plan for
   stage 4/5 spelled out.
