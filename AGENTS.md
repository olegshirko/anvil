# AGENTS.md

This file is a brief guide for AI agents working on the Anvil repository.
For a detailed description of the architecture and the rationale behind key
decisions, see `ARCHITECTURE.md` (required reading before non-trivial changes).

## Project overview

Anvil is a minimal alternative to Lima / Docker Desktop / OrbStack for running
Docker containers on macOS (Apple Silicon only). The system consists of two
components:

- **`vz-runner`** (Swift, `Sources/vz-runner/`) — the single long-lived host
  process. It manages the Linux VM via `Virtualization.framework`
  (snapshot/resume, virtiofs, NAT networking), opens the unix sockets
  `~/.anvil-vz/control.sock` (control plane), `~/.anvil-vz/docker.sock`
  (Docker API proxy) and `~/.anvil-vz/buildkit.sock` (buildkit API proxy for
  the buildx remote driver), and forwards container ports to `localhost`.
- **`guest-agent`** (Go, `guest-agent/`) — PID 1 inside the VM. It listens on
  vsock (port 1024 — length-prefixed JSON control channel, port 1025 — Docker
  API emulation for `docker`/`docker compose`, port 1026 — bridge to the
  buildkitd unix socket for the buildx remote driver), scans containerd and
  pushes port mappings to vz-runner, runs healthchecks, generates CNI configs.
  When a container with `-p` starts, it asks vz-runner whether the host ports
  are taken (the guest dials vsock port 1027, listened to by vz-runner —
  `PortCheckServer`): if a port is held by someone else's process on the host
  (Docker Desktop, Lima, a local postgres), start fails with a Docker-style
  "port is already allocated" error instead of silently leaving the container
  unreachable. The check happens on start, not on create: compose
  `--force-recreate` creates the replacement while the old container still
  holds the port. Inside the VM run containerd + nerdctl + runc + CNI plugins;
  there is no systemd and no SSH. buildkitd starts lazily — on the first
  connection to port 1026 or the first `nerdctl build`.

The user works with the regular Docker CLI: `docker context use anvil` points
at `~/.anvil-vz/docker.sock`, and HTTP traffic is proxied into the guest-agent
over vsock.

## Tech stack and requirements

- macOS on Apple Silicon, Xcode/Swift toolchain, `swift-tools-version:5.9`,
  `platforms: [.macOS(.v14)]` (`Package.swift`).
- The binary requires the `com.apple.security.virtualization` entitlement
  (`entitlements.plist`) — an ad-hoc signature is mandatory (`make sign`).
- Go module `guest-agent` (`guest-agent/go.mod`, go 1.26); cross-compiled
  with `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`.
- Python 3 for helper scripts (`scripts/*.py`), no dependencies.
- External build tools: `limactl` (initramfs build inside the Lima VM
  `anvil`; fallback — local Docker), Docker CLI, curl.
- Downloaded artifacts (kernel, initramfs, container-tools) live in
  `.download/` and are not committed.

## Build and common commands

Everything is driven through the `Makefile`:

- `make sign` — build the release binary and sign it with entitlements
  (generates `Sources/vz-runner/version.swift`; do not edit by hand).
- `make guest-agent` — build the guest-agent (linux/arm64) into
  `.download/alpine/guest-agent`.
- `make initramfs-containerd` — build the initramfs with
  containerd/nerdctl/CNI (script `scripts/build_initramfs_containerd.sh`;
  built inside a Linux container — either the Lima VM `anvil` or local
  Docker). buildkit binaries are packed with UPX: CI installs it from apk,
  offline builds in Lima use the static binary from `.download/upx` (target
  `download-upx`, included in the dependencies).
- `make rebuild-all` — binary + guest-agent + initramfs in one command.
- `make service-start` / `service-stop` / `service-restart` / `service-status` —
  daemon management via `scripts/anvil-service.sh` (saves and restores the
  docker context).
- `make service-debug` — stop + delete the snapshot + cold boot with
  `DEBUG=1`; a cold boot is required, because on resume the guest-agent from
  the snapshot is the old process without debug logs (the log is written to
  `<share>/guest-agent.log`).
- `make service-debug-rebuild` — full rebuild + debug restart.
- `make boot-containerd` — one-off foreground boot (the initramfs is not
  rebuilt, so the snapshot hash stays valid).
- `make prune` — clean containers/volumes/images inside the VM.
- `make disk-compact` — reclaim host space after prune: a sparse copy of
  `~/.anvil-vz/containerd-disk.img` (the daemon is stopped and started again;
  the logical size and contents do not change, the snapshot stays valid).
- `make clean` — remove `.build`, `.download`, `.venv`.

Environment variables: `DEBUG=1` (debug logs), `ANVIL_MEMORY` (VM RAM in GB),
`ANVIL_DISK_GB` (containerd disk size, default 64; an existing image is only
grown, the next launch is a cold boot + online resize2fs), `ANVIL_SHARE_USERS=0`
(disable the virtiofs share of host `/Users` into the guest — by default
`/Users` is mounted in the VM at the same path, so bind mounts
`docker run -v $HOME/...:/path` and compose `volumes:` work without path
rewriting), `VERSION` (substituted into `version.swift`), `VZRUNNER_BIN`
(path to the binary for service scripts and bench-harness).

## Testing

There are no automated unit tests (neither Swift nor Go — no `*_test.go`).
Verification is integration-style, via scripts and manual Docker CLI runs:

- `make test` — smoke: build + `--help`.
- `make time-boot` / `make time-service` — boot time measurements
  (`scripts/time_boot.py`, `scripts/time_service.py`).
- `make validate` — a robustness suite (`scripts/validate_robustness.py`):
  save/resume cycles, resume with a running container, kill -9 without
  orphan processes, FD leaks, CNI cleanup, two-project isolation and port
  conflicts.
- `make harness` / `make bench-all` — bench-harness (`bench-harness/`):
  cold start / resume / compose up compared against Lima, Colima, OrbStack,
  Docker Desktop; results land in `bench-harness/results/`.

Any change to guest-agent or vz-runner is verified with a real run:
`make service-debug-rebuild`, then `docker --context anvil run ...` /
`docker compose up` on a test stack.

## Code layout

- `Sources/vz-runner/` — Swift sources, one file ≈ one component:
  `main.swift` (CLI: start/stop/status/daemon/boot/exec), `DaemonCommand`,
  `VMLifecycleManager`, `VMConfig`, `SnapshotManager`, `ControlServer` +
  `ControlClient` + `ControlProtocol`, `DockerProxyServer`, `PortForwarder`,
  `PortCheckServer` (vsock 1027: answers the guest about host-port
  occupancy), `GuestCacheDropper`, `ContainerdCacheManager`, `BootCommand`.
- `guest-agent/` — Go sources organized by Docker API domain: `main.go`
  (vsock control server, zombie reaper), `dockerapi.go` (HTTP routes),
  `containers.go`, `images.go`, `networks.go`, `volumes.go`, `exec.go`,
  `archive.go`, `healthcheck.go`, `scanner.go` (port scanner/pusher),
  `buildkit.go` (vsock:1026 bridge to buildkitd + lazy start),
  `build.go` (`/build` via nerdctl), `info.go`, `utils.go`.
- `scripts/` — initramfs build and service wrapper
  (`build_initramfs_containerd.sh` is the main one; `stage2.sh` and `myinit`
  are generated inline inside it).
- `bench-harness/` — benchmarks (`run_bench.sh`, backend drivers in
  `drivers/`, workloads in `workloads/`).
- `networks/`, `var-lib/`, `guest-agent.log`, `.download/` — runtime
  artifacts of the virtiofs share / state, not source code.

## Conventions and important invariants

- Documentation language is English (`ARCHITECTURE.md`, planning documents).
  **Code comments are English-only** (`Sources/*.swift`, `guest-agent/*.go`,
  `scripts/*.sh`, `Makefile`, CI workflows): there must be no Russian
  comments in code; translate existing ones when touching them. Write
  documentation (`*.md`) in English.
- Swift: Foundation + Virtualization.framework, no third-party dependencies
  (`Package.swift` has no dependencies — do not add any without a good
  reason). POSIX sockets instead of Network.framework is a deliberate
  decision (see `ARCHITECTURE.md` §3.4).
- Go: static binary without CGO; guest-agent is PID 1, so it **must not
  crash** and must reap zombies (`reapZombies`).
- The Docker API is only partially emulated — just what `docker` and
  `docker compose` need. There are non-trivial invariants that must not be
  broken (details in `ARCHITECTURE.md` §4.3): deterministic container ID
  (`sha256(namespace + "/" + containerdID)[:64]`); `/containers/{id}/wait`
  sends headers immediately (chunked) before blocking; `AutoRemove` is
  implemented by the guest-agent itself, not by `nerdctl --rm`.
- Compose-network labels are persisted in `/mnt/anvil/networks/<name>.json`
  (nerdctl does not return them) — when changing `networks.go`, do not lose
  the CNI conflist restoration on cold boot.
- The snapshot hash includes kernel/initrd/CPU/RAM/disk — changing
  `VMConfig` or the initramfs invalidates the snapshot and causes a cold
  boot; this is expected, but keep it in mind while debugging.
- The containerd disk uses the host writeback cache (`cachingMode: .cached`,
  `synchronizationMode: .none`) — a deliberate durability-vs-speed trade-off
  for a dev VM; do not "fix" it without a discussion (`ARCHITECTURE.md`
  §7.1).
- `version.swift` is generated by the Makefile; do not edit by hand.

## Security

- The daemon keeps its state in `~/.anvil-vz/` (snapshots, the containerd
  disk, pid/logs, the saved docker context). Do not delete these files
  silently — the snapshot and the disk contain user data.
- The control socket accepts `exec` commands that run inside the VM as
  root; the socket is unauthenticated (designed for the local user). Do not
  expose it and do not extend the protocol thoughtlessly.
- `anvil-service.sh` warns about `http_proxy` without `localhost` in
  `NO_PROXY` — do not remove this check.
- Releases: `make release VERSION=x.y.z` (tag + GitHub Actions from
  `.github/workflows/go.yml` + Homebrew tap update). Git mutations
  (tag/push) are performed only on the user's explicit request.

## Deploy / release process

CI is `.github/workflows/go.yml` (macos-15): build check on PRs into `test`;
on pushing a `v*` tag — build, codesign, publish
`anvil-darwin-arm64.tar.gz` to GitHub Releases. Then
`make update-brew VERSION=x.y.z` updates the formula in the neighboring
`homebrew-tap` repository (also cleaning out the stale bottle block), and
`make bottle VERSION=x.y.z` (`scripts/make_bottle.sh`) builds the bottle
(`brew install --build-bottle` + `brew bottle --no-rebuild`), uploads the
tarball to the same GitHub release (file name with a single dash — `brew
bottle` creates a double dash, while brew looks for a single one), uploads
the same tarball under the other supported macOS tags and writes the bottle
block with all tags into the formula. Two invariants that must not be
broken:

- **`rebuild` must be 0.** `brew bottle` without `--no-rebuild` sets
  rebuild = (formula's rebuild on origin/HEAD) + 1, and since update-brew
  has already pushed the cleaned formula, upstream matches and rebuild
  becomes 1. With rebuild > 0 zerobrew builds the URL using its own
  incompatible scheme
  `<name>-<version>.<rebuild>.<tag>.bottle.tar.gz` (Homebrew puts rebuild
  after the tag) and gets a 404 — it has no fallback to source.
- **Tags for all supported macOS versions.** The bottle is
  `cellar :any_skip_relocation` and contains no platform-specific
  artifacts, so one tarball serves all tags; they are listed in the
  `EXTRA_TAGS` loop in `make_bottle.sh` — when a new macOS version ships,
  add its tag there.

Users install via Homebrew (`vz-runner` in PATH + assets in
`share/anvil`); the LaunchAgent is
`scripts/com.olegshirko.anvil.plist` (`make service-install`).
