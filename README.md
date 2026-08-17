# Anvil

[![CI](https://github.com/olegshirko/anvil/actions/workflows/go.yml/badge.svg)](https://github.com/olegshirko/anvil/actions/workflows/go.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
![Platform](https://img.shields.io/badge/platform-macOS%20Apple%20Silicon-lightgrey)
![brew](https://img.shields.io/badge/homebrew-olegshirko%2Ftap%2Fanvil-orange)

A minimal, fast alternative to Lima / Docker Desktop / OrbStack for running
Docker containers on macOS (Apple Silicon only). One host process, one tiny
Linux VM, no SSH, no systemd, no background fleet of helpers.

![boot demo](boot-demo.gif)

Real session, timings from `/usr/bin/time -p`: resume + `docker run --rm
alpine` in half a second. (The 1.55 s `anvil start` includes ~1 s of
docker-CLI bootstrap — context switch and buildx builder selection; the VM
itself is restored from a memory snapshot in 0.5 s.)

## Why

| Metric | **anvil** | colima | lima | orbstack | docker-desktop |
|---|---|---|---|---|---|
| Cold start (daemon ready) | **1765 ms** | 11388 ms | 9339 ms | 1811 ms | 6177 ms |
| Cold start: compose up (all healthy) | **963 ms** | 1529 ms | 1541 ms | 4608 ms | 1621 ms |
| Resume (daemon ready) | **579 ms** | 10864 ms | 7162 ms | 1763 ms | 5936 ms |
| Resume: compose up (all healthy) | **873 ms** | 1679 ms | 1604 ms | 4553 ms | 1557 ms |
| Idle RSS | 1247 MB | 2198 MB | 2010 MB | 2208 MB | **1224 MB** |

Full methodology and workloads: [bench-harness/](bench-harness/README.md).

The speed comes from two decisions: the VM is paused into a **memory snapshot**
after boot and restored from it (no re-boot, no re-provisioning), and the whole
data path is **virtio-vsock + virtiofs**, with no SSH, no port-forwarding
daemons and no userspace network stack in between.

## Requirements

- macOS 14+ on Apple Silicon
- Docker CLI (any recent `docker` / `docker compose` client)
- For building from source: Xcode/Swift toolchain, Go, and either a Lima VM
  named `anvil` or a local Docker for the initramfs build

## Install

### Homebrew

```sh
brew install olegshirko/tap/anvil
anvil start        # first run is a cold boot (~1.5 s), then a snapshot is saved
```

### zerobrew

The same tap works with zerobrew:

```sh
zerobrew install olegshirko/tap/anvil
anvil start
```

zerobrew has no `brew services`, so the LaunchAgent for autostart at login is
not installed automatically — run `anvil start` after login instead. `anvil
start` also unpacks the gzipped kernel itself when the service wrapper has
not done it, so no extra steps are needed.

### From source

```sh
git clone https://github.com/olegshirko/anvil.git
cd anvil
make rebuild-all      # vz-runner (signed) + guest-agent + initramfs
make service-start    # background daemon + docker context
```

`make service-install` registers a LaunchAgent so anvil starts at login.

## Usage

Anvil is a Docker **context** — after `anvil start` your normal Docker CLI
talks to it:

```sh
docker context use anvil        # done automatically by `anvil start`
docker run --rm -p 8080:80 nginx
docker compose up               # compose works: networks, volumes, events
docker build -t myimg .         # buildx remote driver against in-VM buildkitd
docker run -v $HOME/proj:/data alpine ls /data   # macOS bind mounts
```

`anvil start` creates a buildx builder named `anvil-remote` (remote driver pointing
at the VM's buildkitd through `~/.anvil-vz/buildkit.sock`) and selects it;
`anvil stop` restores your previous builder. With the remote driver,
`docker build` needs `--load` to import the result into the image store
(compose does this automatically on `compose build`). The classic
`DOCKER_BUILDKIT=0 docker build` path works too.

Host ports of published containers are forwarded to `localhost` automatically.

### CLI

```
anvil start      Launch the daemon, wait for ready, switch docker context
anvil stop       Stop the daemon, restore the previous docker context
anvil restart    Stop + start
anvil status     Daemon + guest readiness (usable as a health gate)
anvil doctor     Diagnose install: hypervisor, signing, assets, API, shares
anvil logs       Tail daemon/console/guest logs
anvil exec ...   Run a command inside the VM (debugging)
```

Housekeeping lives in the Makefile: `make prune` (remove containers/volumes/
images inside the VM), `make disk-compact` (reclaim host space from the
sparse containerd disk after a prune — logical size and snapshot stay
intact).

### Configuration (environment variables)

| Variable | Default | Purpose |
|---|---|---|
| `ANVIL_MEMORY` | `2` | VM RAM in GiB |
| `ANVIL_DISK_GB` | `64` | containerd disk size (sparse; existing disks only grow, guest fs is resized online) |
| `ANVIL_SHARE_USERS` | `1` | Set to `0` to disable sharing the host `/Users` tree into the VM |
| `DEBUG` | — | `1` enables guest-agent debug log (`guest-agent.log` on the share) |

## Architecture

Two components, one process each:

```
┌──────────────────────────── macOS (host) ───────────────────────────┐
│  docker CLI / docker compose                                        │
│      │ unix://~/.anvil-vz/docker.sock                               │
│      ▼                                                              │
│  vz-runner (Swift, single long-lived process)                       │
│    • VMLifecycleManager — boot / save / restore via VZ snapshots    │
│    • DockerProxyServer — unix socket ⇆ vsock:1025 proxy             │
│    • ControlServer     — ~/.anvil-vz/control.sock ⇆ vsock:1024      │
│    • PortForwarder     — localhost:PORT → container IP              │
└───────┼──────────────────────────────────────────┼──────────────────┘
        │ virtio-vsock                             │ virtiofs
┌───────▼──────────────────────────────────────────▼──────────────────┐
│  Linux VM (Alpine kernel 6.6 + custom initramfs)                    │
│    guest-agent (Go, PID 1)                                          │
│    • Docker API emulation on vsock:1025 (subset used by docker/     │
│      compose: containers, images, networks, volumes, exec, logs,    │
│      build, events, archive)                                        │
│    • control channel on vsock:1024 (exec, status, port mappings)    │
│    • port scanner, healthchecks, CNI config generation              │
│    containerd + nerdctl + runc + buildkitd + CNI plugins            │
│    /var/lib on a persistent virtio-blk disk (ext4)                  │
└─────────────────────────────────────────────────────────────────────┘
```

Key design points (the full rationale lives in
[ARCHITECTURE.md](ARCHITECTURE.md)):

- **Snapshot resume, not reboot.** After the first boot the VM state is saved
  (`~/.anvil-vz/snapshots`). Later starts restore memory and device state
  directly; the snapshot is keyed by a hash of kernel/initrd/CPU/RAM/disk/
  shares, so any config change falls back to a cold boot automatically.
- **No SSH, no systemd.** guest-agent is PID 1: it mounts filesystems, starts
  containerd/buildkitd, reaps orphans and serves the API over vsock.
- **POSIX sockets, not Network.framework** — the proxy is a plain
  byte-pump between a unix socket and vsock, with no per-request overhead.
- **Persistent containerd disk.** Images and volumes live on a sparse raw
  disk image (host writeback cache, durability traded for speed — snapshots
  provide the safety net). The disk grows automatically and the guest ext4 is
  resized online (`resize2fs`) after a grow.
- **Bind mounts like Docker Desktop.** The host `/Users` tree is shared via
  virtiofs and mounted at the *same absolute path* in the guest, so
  `-v $HOME/...:/path` and compose relative volumes need no path rewriting.
- **Per-project networking.** Each compose project gets its own CNI bridge;
  the guest-agent generates CNI configs and pushes port mappings to the
  host-side PortForwarder.
- **`docker build` without Docker Desktop.** A classic `POST /build`
  endpoint extracts the context onto the persistent disk and runs
  `nerdctl build` against an in-VM buildkitd.

## Current limitations

- Apple Silicon only; no amd64 emulation yet (Rosetta support is planned).
- With the remote buildx driver, plain `docker build` keeps the result in the
  build cache — add `--load` to import it into the image store (compose does
  this automatically). The buildx `docker-container` driver (which pulls a
  moby/buildkit image) does not work.
- `docker events --since` in the past replays nothing: there is no event
  log, only live events are streamed (`--until` and filters work).
- Docker API is emulated, not complete: it covers what `docker` CLI and
  `docker compose` actually use. Swarm, plugins and some prune endpoints are
  out of scope.
- The control socket is unauthenticated (local-user trust model) — do not
  expose it.

## Development

```sh
make service-debug-rebuild   # rebuild everything, cold boot with debug logs
make validate                # robustness suite (save/resume, kill -9, FD leaks, CNI)
make harness                 # benchmarks against Lima/Colima/OrbStack/Docker Desktop
make test                    # smoke: build + --help
```

Layout: `Sources/vz-runner/` (Swift host daemon, one file ≈ one component),
`guest-agent/` (Go, organized by Docker API domain), `scripts/` (initramfs
build + service wrapper), `bench-harness/` (benchmarks), `IMPROVEMENTS.md`
(roadmap), `ARCHITECTURE.md` (design decisions).

Releases: `make release VERSION=x.y.z` — tag, GitHub Actions build + codesign,
Homebrew tap update.

## License

Apache-2.0 — see [LICENSE](LICENSE).
