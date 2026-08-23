# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Anvil runs Docker containers on macOS (Apple Silicon only) with **two processes total**: a
Swift host daemon (`vz-runner`) that owns a Virtualization.framework VM, and a static Go
binary (`guest-agent`) that is PID 1 inside it. There is no SSH, no systemd, no
port-forwarding daemon, no userspace network stack. The Docker CLI talks to
`~/.anvil-vz/docker.sock`, which is proxied byte-for-byte into the guest over virtio-vsock.

`ARCHITECTURE.md` holds the full rationale for every decision below — read the relevant
section before touching the host/guest boundary, port forwarding, or the boot path.

## Build & test

```sh
make rebuild-all             # sign Swift binary + build guest-agent + build initramfs
make unit-tests              # Go (go vet + go test) and Swift tests — no VM needed
make test                    # sign + unit-tests
make service-start           # start the background daemon and switch docker context
make integration             # Docker API suite against a live daemon (must be running)
make service-debug-rebuild   # rebuild everything, drop the snapshot, cold boot with debug logs
make validate                # robustness suite (save/resume, kill -9, FD leaks, CNI, isolation)
make doctor                  # diagnose install: hypervisor, signing, assets, API, shares
make prune                   # remove containers/volumes/images inside the VM
make disk-compact            # reclaim host space from the sparse containerd disk
make harness                 # benchmarks vs Lima/Colima/OrbStack/Docker Desktop/Apple Containers
make release VERSION=x.y.z   # tag, CI build + codesign, Homebrew tap update
```

Single tests:

```sh
cd guest-agent && go test -run TestReadPortProxyHeader ./...
```

```sh
swift test --filter ControlProtocolTests
```

`scripts/integration_tests.py` has no filter flag — the suite is the `TESTS` list at the
bottom of the file; to run one case, call its function from a `python3 -c` one-liner or
temporarily trim the list.

The typical inner loop for guest-agent changes is
`make service-debug-rebuild && make integration` — a guest-agent edit is not live until the
initramfs is rebuilt *and* the snapshot is invalidated.

## Layout

- `Sources/vz-runner/` — Swift host daemon, one file ≈ one component.
- `guest-agent/` — Go guest process, one file ≈ one Docker API domain
  (`containers.go`, `images.go`, `networks.go`, `volumes.go`, `exec.go`, `events.go`,
  `build.go`/`buildkit.go`, `restart.go`, `healthcheck.go`, `scanner.go`).
- `scripts/` — initramfs build (`build_initramfs_containerd.sh`), the `stage2.sh` guest
  init, the service wrapper (`anvil-service.sh`), and the Python test harnesses.
- `bench-harness/` — cross-backend benchmarks.

## Architecture: the parts that span files

**Three vsock channels.** 1024 = control (length-prefixed JSON: `exec`, `status`, `sync`),
1025 = Docker API (raw HTTP), 1026 = buildkit bridge. Host side these are
`ControlServer`, `DockerProxyServer` (parameterized by port — it serves both `docker.sock`
and `buildkit.sock`), and the guest side is `main.go`, `dockerapi.go`, `buildkit.go`.

**Port forwarding is push-based, and host ports are never bound in the guest.**
`scanner.go` watches containerd tasks and pushes the *full* mapping state to
`PortForwarder.swift`, which does a replace (open new, close gone). TCP goes host listener
→ guest port proxy (`portproxy.go`, length-prefixed JSON header naming
`containerIP:containerPort`) because the host has no route into `10.10.x`; UDP is relayed
straight to `guestIP:hostPort`. Host ports are deliberately never bound at create time —
that breaks `docker compose up` over live containers. The mappings are persisted in the
guest-agent's own store so ps/inspect/scanner see them, and conflict checks live at start
time.

**Snapshot resume, not reboot.** `VMLifecycleManager` + `SnapshotManager` pause the VM into
`~/.anvil-vz/snapshots/default.vzstate` and restore from it. The snapshot is keyed by a
hash of kernel + initrd + CPU + RAM + disk path/size + shares; any mismatch silently falls
back to a cold boot. `GuestCacheDropper` drops the guest page cache before saving.

**Boot path.** initramfs → `switch_root` → `stage2.sh` (mount virtiofs at `/mnt/anvil`,
mount all of `/var/lib` from `/dev/vda`, load netfilter modules, background `udhcpc`, start
containerd) → `exec guest-agent` as PID 1. Everything slow moved into the agent's
`runBootFinalize` (`bootfinalize.go`): containerd-socket wait, orphan cleanup, DHCP wait.
The Docker API and exec wait on that finalize (bounded); `status`/`health` does not.

**Docker API emulation invariants** (all covered by tests; easy to break by accident):

- Container ID is `sha256(namespace + "/" + containerdID)[:64]` — deterministic across
  sessions.
- `/_ping` must return `Ostype` and `Api-Version` headers; we advertise 1.51 / min 1.24.
- `POST /containers/{id}/wait` must send headers immediately (chunked) *then* block — the
  CLI calls `/wait` before `/start` on the same connection.
- `AutoRemove` and `--restart` are handled by the agent itself; `restart.go` reads
  authoritative task state from containerd and owns the policy.
- Hijacked exec/attach stdin is a raw byte stream (only output is multiplexed), and the
  stdin pipe must close on output EOF or buildx deadlocks.
- Image refs are canonicalized before pull/lookup (`postgres:15.5` →
  `docker.io/library/postgres:15.5`).
- Compose network labels are persisted to `/mnt/anvil/networks/<name>.json`; without them
  compose treats the network as external after a cold boot.
- Each compose project gets its own containerd namespace and CNI bridge; subnet is
  deterministic: `10.10.<hash(project) % 250 + 1>.0/24`.

## Gotchas

- `Sources/vz-runner/version.swift` is **generated by the Makefile** and gitignored. Never
  edit it by hand.
- The Swift binary must be codesigned with `entitlements.plist` before it can use
  Virtualization.framework. `make sign` does it; manual `swift build` does not.
- guest-agent is cross-compiled `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`. It never runs on
  the host — only its pure-Go helpers are unit-testable on darwin.
- `DEBUG=1` only enables *host*-side logs on resume. Guest-agent debug logs need a cold
  boot (`make service-debug`) so `stage2.sh` sees `/mnt/anvil/.anvil-debug`; they land in
  `guest-agent.log` at the share root (the project dir in a source tree).
- The initramfs build needs a Linux container: a Lima VM named `anvil` if running,
  otherwise local Docker.
- `make boot-containerd` deliberately does *not* rebuild the initramfs, so an existing
  snapshot's config hash stays valid. Use `boot-containerd-fresh` to rebuild.
- With the remote buildx driver (`anvil-remote`), plain `docker build` leaves the result in
  the build cache — `--load` is needed to import it into the image store.
- `docker events` has no historical replay (no event log); only live streaming, filters and
  `--until` work.
- CI (`.github/workflows/go.yml`) runs on `macos-15` x86_64 runners: it can build and sign
  but **cannot run any VM-based test**. `make validate` needs a self-hosted Apple Silicon
  runner and is `workflow_dispatch` only.
- English only in code comments and commit messages.

## Environment variables

`ANVIL_MEMORY` (GiB, default 2) · `ANVIL_DISK_GB` (default 64, sparse, grows only) ·
`ANVIL_SHARE_USERS` (default 1, shares host `/Users` at the same absolute path) · `DEBUG` ·
`BENCH_BACKENDS` (default `vz-runner`). Changing memory or disk size changes the snapshot
hash and forces a cold boot.
