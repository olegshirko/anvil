# Anvil vz-runner: architecture and key decisions

This document describes what components the system consists of, why exactly
these technologies were chosen, and how everything behaves on start, restart
and shutdown.

## 1. High-level picture

```
┌─────────────────────────────────────────────────────────────────┐
│                        macOS (host)                             │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────┐  │
│  │   vz-runner  │  │   Docker CLI │  │  docker compose CLI  │  │
│  │   (Swift)    │  │              │  │                      │  │
│  └──────┬───────┘  └──────┬───────┘  └──────────┬───────────┘  │
│         │                 │                      │              │
│         │ unix socket     │ ~/.anvil-vz/docker.sock             │
│         │ ~/.anvil-vz/    │                      │              │
│         │ control.sock    │                      │              │
│         │                 │                      │              │
│  ┌──────▼─────────────────▼──────────────────────▼───────────┐  │
│  │              DockerProxyServer / ControlServer            │  │
│  │                    (inside vz-runner)                     │  │
│  └──────┬─────────────────────────────┬────────────────────┘  │
│         │ vsock:1024                  │ vsock:1025            │
│         │ control channel             │ Docker API channel    │
│         │                             │                       │
│  ┌──────▼─────────────────────────────▼────────────────────┐  │
│  │                     Linux VM (guest)                    │  │
│  │  ┌─────────────────────────────────────────────────┐   │  │
│  │  │  guest-agent (Go)                               │   │  │
│  │  │  • vsock control server                         │   │  │
│  │  │  • Docker API server                            │   │  │
│  │  │  • port scanner + pusher                        │   │  │
│  │  │  • healthcheck runner                           │   │  │
│  │  │  • CNI config generator                         │   │  │
│  │  └────────────────────┬────────────────────────────┘   │  │
│  │                       │                                  │  │
│  │  ┌────────────────────▼────────────────────────────┐   │  │
│  │  │  containerd + nerdctl + CNI plugins             │   │  │
│  │  │  /var/lib (containerd + nerdctl volumes)        │   │  │
│  │  │  on a virtio-blk disk                           │   │  │
│  │  └─────────────────────────────────────────────────┘   │  │
│  └─────────────────────────────────────────────────────────┘  │
│                          │                                     │
│              VZVirtioFileSystemDeviceConfiguration            │
│                          │ /mnt/anvil                          │
│         ┌────────────────┴────────────────┐                   │
│         │   virtiofs share (host dir)     │                   │
│         │   • guest-agent.log (debug)     │                   │
│         │   • networks/<name>.json        │                   │
│         │   • containerd-cache fallback   │                   │
│         └─────────────────────────────────┘                   │
└─────────────────────────────────────────────────────────────────┘
```

`vz-runner` is the single long-lived host process. It:

- creates and manages the Linux VM via `Virtualization.framework`;
- opens unix sockets for the control plane and the Docker API proxy;
- listens on vsock together with the guest-agent;
- opens TCP listeners on `localhost:<hostPort>` to forward container ports;
- saves and restores the VM snapshot.

`guest-agent` is the only process inside the VM after `stage2`. It:

- receives commands from `vz-runner` over vsock (exec, status, sync);
- emulates the Docker API for Docker CLI / docker compose;
- scans containerd and pushes the current port mappings to `vz-runner`;
- runs healthchecks itself, because `nerdctl` does not support the Docker
  `--health-cmd` flags;
- generates CNI configs for per-project bridge networks.

## 2. Why not Lima / Docker Desktop / OrbStack

Originally Anvil used Lima. Lima is good for prototyping, but for a product
it brings a lot of extra weight: an SSH daemon, systemd, cloud-init, Lima's
own guest agent, and the gvisor-tap-vsock layer. All of that slows down the
cold boot and complicates snapshotting.

The decision: a minimal custom guest rootfs and a custom host runner:

- **No systemd.** The init is a busybox shell; stage2 starts containerd and
  the guest-agent directly.
- **No SSH.** Management goes over vsock, not TCP/SSH.
- **No gvisor-tap-vsock.** Networking is Apple's built-in
  `VZNATNetworkDeviceAttachment`.
- **Single shared VM.** All of the user's projects live in one VM, isolated
  via containerd namespaces and separate CNI bridges.

## 3. Host side: vz-runner

### 3.1 VMLifecycleManager

Owns the whole VM lifecycle:

- **cold boot** — creates the `VZVirtualMachineConfiguration`, starts the
  VM, waits for the guest-agent;
- **resume** — `restoreMachineStateFromURL` from
  `~/.anvil-vz/snapshots/default.vzstate`;
- **pause + save** — on idle timeout or SIGTERM: pause the VM, drop the
  guest page cache, save the state;
- **snapshot invalidation** — before a restore, a hash of the kernel,
  initrd, CPU, RAM, disk path/size is computed. If the configuration has
  changed — cold boot and snapshot re-creation.

### 3.2 ControlServer

Unix socket `~/.anvil-vz/control.sock`. Accepts length-prefixed JSON from
the CLI and forwards it to the guest-agent over vsock:1024. Used for
`vz-runner exec`, `vz-runner status`, containerd cache sync. With `--debug`
it prints the command and exit code of every request.

`DockerProxyServer` used to bypass the ControlServer and talk to vsock
directly, so on an idle-pause the Docker CLI got an `EOF`. Now the
DockerProxyServer also calls `ensureRunning` to resume the VM before
accepting a docker request.

### 3.3 DockerProxyServer

Unix socket `~/.anvil-vz/docker.sock`. Accepts HTTP from the Docker CLI,
strips the `/v1.XX` prefix, forwards raw bytes to vsock:1025 where the
guest-agent's HTTP server runs. The response is passed back without any
protocol parsing.

The same class (`DockerProxyServer` parameterized by port) also serves
`~/.anvil-vz/buildkit.sock` → vsock:1026: a raw TCP bridge to the buildkitd
unix socket in the guest. The buildx builder `anvil-remote` (remote driver)
points at it; the builder is created/restored at daemon startup
(`anvil-service.sh` + `main.swift`). Through it, `docker buildx build` and
`docker compose build` work without an intermediate buildkit container.
buildkitd in the guest starts lazily — the guest-agent brings it up on the
first connection to port 1026 or the first `nerdctl build`.

### 3.4 PortForwarder

The guest-agent scans running containers and pushes the full list of port
mappings to `vz-runner`. `PortForwarder`:

- opens `listen 0.0.0.0:<hostPort>`;
- forwards TCP to the container through the guest-side port proxy
  (guest-agent listens on a single well-known TCP port; the forwarder
  sends `containerIP:containerPort` as a length-prefixed JSON header and
  the proxy dials the CNI address from inside the guest, where it is
  reachable — the host has no route into 10.10.x). Mappings from older
  guests without a container IP fall back to dialing `guestIP:hostPort`;
- on every push does a full-state replace: new ports are opened, gone ports
  are closed;
- logs a conflict when it fails to open an already taken port.

User host ports are deliberately never bound inside the guest: nerdctl
reserves host ports at CREATE time (an inherited listener fd owned by the
container process), which breaks the Docker flow where the check belongs to
start — `docker compose up` over live containers creates the replacement
before stopping the old one. Ports are therefore not passed to nerdctl at
all; the mappings are persisted in nerdctl's network store
(`network-config.json`) so ps/inspect/scanner see them, and our own
start-time checks enforce conflicts.

POSIX sockets were chosen over `NWListener` because all that is needed is a
plain TCP proxy loop without the TLS/path-monitoring/event-loop overhead of
Network.framework.

### 3.5 SnapshotManager

Saves/restores `default.vzstate`. Before saving, `GuestCacheDropper` runs
`sync; echo 3 > /proc/sys/vm/drop_caches` in the guest so the snapshot does
not drag along the page cache of images.

## 4. Guest side: guest-agent

### 4.1 Startup and the PID 1 role

`stage2.sh` does `exec /bin/guest-agent`. The process becomes PID 1,
therefore:

- a background `SIGCHLD` reaper via `syscall.Wait4(-1, WNOHANG)` must run,
  otherwise finished `containerd-shim`/`runc`/`nerdctl` processes turn into
  zombies and deadlock containerd;
- the guest-agent must not crash unexpectedly — otherwise the VM is left
  without management.

### 4.2 vsock control channel (port 1024)

Simple length-prefixed JSON. Commands: `exec`, `status`, `sync`. The
response contains stdout/stderr/exit_code. Used as the CLI control plane.

### 4.3 Docker API server (port 1025)

An HTTP/1.1 server emulating the subset of the Docker API needed by
`docker` and `docker compose`:

- `/_ping`, `/version`;
- `/containers/*` create/start/stop/wait/rm/attach/logs/exec/inspect/archive;
- `/images/*` create/json/inspect/tag/push/rmi;
- `/networks/*` create/inspect/list/rm/prune;
- `/volumes/*` create/inspect/list/rm/prune.

Notable details:

- The Docker container ID is a deterministic
  `sha256(namespace + "/" + containerdID)[:64]`, so the ID does not change
  between sessions.
- `POST /containers/{id}/wait` sends the HTTP headers immediately (chunked
  encoding) and only then blocks until the container exits. This is needed
  because the Docker CLI calls `/wait` before `/start`, and if the response
  is not started right away, the following `/start` queues on the same
  connection and the container never starts.
- `HostConfig.AutoRemove` is not passed to `nerdctl create --rm`. Instead
  the guest-agent removes the container itself after the exit code has been
  saved — otherwise `docker run --rm` could not return a non-zero exit
  code.
- On a hijacked exec/attach connection the client's stdin is always a raw
  byte stream (only the output is multiplexed). It must be forwarded to the
  process as is: the buildx docker-container driver runs gRPC through
  `buildctl dial-stdio` over this channel. `cmd.Wait()` also waits for the
  stdin copy, so the stdin pipe is closed on EOF of the output streams —
  otherwise a client that keeps the connection open (buildx) deadlocks
  exec.

### 4.3a buildkit bridge (port 1026)

`buildkit.go`: a raw TCP bridge vsock:1026 ↔
`/run/buildkit/buildkitd.sock`. The first incoming connection (or
`POST /build`) lazily starts `buildkitd`. The port is forwarded to the host
as `~/.anvil-vz/buildkit.sock` (see §3.3).

### 4.4 Port scanner

Connects to containerd via `/run/containerd/containerd.sock`, periodically
reads tasks and their labels. When ports change:

- on start and after a resume — pushes the full state;
- on regular changes — a 150 ms debounce, then pushes the resulting state.

### 4.5 Healthcheck

The guest-agent saves the healthcheck config from
`POST /containers/create` and runs periodic `nerdctl exec` checks itself.
The `(healthy)`/`(unhealthy)`/`(starting)` status is returned in
`docker ps` and `docker inspect`, which lets `docker compose` use
`depends_on: condition: service_healthy`. This was originally done because
`nerdctl` 2.0.4 did not support `--health-cmd`; the flags appeared in 2.3.5,
but the custom runner stayed — it is battle-tested and does not depend on
nerdctl quirks.

### 4.6 CNI / per-project networking

Every Docker Compose project gets its own namespace and bridge network:

- the subnet is deterministic: `10.10.<hash(project) % 250 + 1>.0/24`;
- the bridge is `br-<sanitized-project>`;
- the CNI conflist
  `/etc/cni/net.d/nerdctl-<name>.conflist` is generated by the guest-agent
  before creating a container or a network.

Compose labels (`com.docker.compose.project`,
`com.docker.compose.network`, etc.) are persisted in
`/mnt/anvil/networks/<name>.json`, because `nerdctl network inspect` does
not return labels for bridge networks. These labels are merged into
`GET /networks` responses, and at startup the guest-agent restores the CNI
conflist from the saved labels — otherwise after a cold boot Compose
considers the network "external" and refuses to use it.

### 4.7 Image canonicalization

The Docker CLI often passes incomplete refs (`postgres:15.5`). `nerdctl`
stores images under the full name
(`docker.io/library/postgres:15.5`). The guest-agent canonicalizes the ref
before pull/lookup so that `docker compose` does not get
`no such image`.

## 5. Initramfs and stage2

### 5.1 Structure

The base is the Alpine initramfs-virt. Inside a Linux container
(`alpine:3.20`) the rootfs is assembled with:

- busybox + basic applets;
- containerd, nerdctl, runc, CNI plugins;
- guest-agent;
- Ubuntu kernel modules: vsock, virtiofs, overlayfs, bridge, veth,
  netfilter/xt/nft modules;
- iptables + libmnl/libnftnl/libxtables;
- GNU tar + libacl/libattr for `nerdctl cp`.

### 5.2 Why switch_root, not bind/pivot

Bind mounts into `/newroot` were used before, but `pivot_root` cannot be
called from the initramfs pseudo-fs `rootfs`. busybox `switch_root` does
`mount --move` + `chroot` + `exec`, which correctly moves init into the
tmpfs root.

### 5.3 stage2

`stage2.sh` after `switch_root`:

1. Checks/remounts proc/sys/dev/cgroup;
2. Mounts the virtiofs share at `/mnt/anvil`;
3. Mounts `/var/lib` as a whole:
   - first priority — the virtio-blk disk `/dev/vda` (ext4);
   - second — a bind mount of `/mnt/anvil/var-lib` (fallback);
   - third — tmpfs (last fallback).

   This matters: previously the disk was mounted only at
   `/var/lib/containerd`, while `/var/lib/nerdctl` (volumes, metadata) and
   `/var/lib/cni` stayed on the tmpfs root. When volumes filled up (e.g.
   PostgreSQL), tmpfs ran out and `docker ps` slowed down because of an
   overflowing/fragmented `nerdctl` state. Now all of `/var/lib` is
   persistent.
4. Loads the netfilter/bridge/veth modules;
5. Adds an iptables MASQUERADE rule for DNATed TCP (fixes asymmetric
   routing under VZ NAT);
6. Starts containerd;
7. Cleans orphaned containers via low-level `ctr` (not `nerdctl rm`, to
   avoid waiting on a hung shim);
8. Starts the guest-agent.

## 6. Restart behavior

### 6.1 Service launch

`scripts/anvil-service.sh`:

- saves the current docker context (if it is not `anvil`);
- starts `vz-runner daemon` with the right kernel/initrd/disk/share paths;
- passes `--debug` when `DEBUG=1` is set;
- waits for `~/.anvil-vz/control.sock` to appear;
- runs `docker context use anvil`;
- warns when the environment has `http_proxy` set but `NO_PROXY` does not
  contain `localhost/127.0.0.1` (otherwise `curl localhost:<port>` and
  integration tests may go through the proxy).

#### Debug mode

`DEBUG=1 make service-start` enables debug logs only on the host side
(`vz-runner`), because `service-start` does a **resume** from the snapshot.
The guest-agent inside the snapshot is the old process that started without
`ANVIL_DEBUG=1` and does not re-read `.anvil-debug` on resume. That is why
`guest-agent.log` is not updated.

To get guest-agent debug logs:

```bash
make service-debug       # stop + delete the snapshot + cold boot with DEBUG=1
```

On a cold boot `stage2.sh` sees `/mnt/anvil/.anvil-debug` and starts the
`guest-agent` with `ANVIL_DEBUG=1`, writing to `<share>/guest-agent.log`.
Subsequent `make service-start`/`service-stop` will resume the VM already
in the debug state, until a new snapshot without debug is created.

On shutdown:

- SIGTERM to the daemon;
- the previous docker context is restored.

### 6.2 Cold boot

1. `vz-runner` finds no snapshot or the hash does not match — cold boot.
2. The VM starts with kernel + initramfs.
3. `myinit` → `switch_root` → `stage2`.
4. Containerd comes up with the persistent disk.
5. The guest-agent starts, restoring CNI conflists from
   `/mnt/anvil/networks/`.
6. The guest-agent pushes the full port state (still empty).
7. `vz-runner` saves the snapshot.

### 6.3 Resume

1. `vz-runner` finds a valid snapshot.
2. `restoreMachineStateFromURL` — usually < 1 s.
3. The VM continues execution from where it was paused.
4. The guest-agent reconnects to vsock and pushes the full port state.
5. `PortForwarder` reopens the needed listeners.

### 6.4 SIGTERM / idle timeout

1. `vz-runner` receives SIGTERM or the idle timer fires.
2. `ContainerdCacheManager.sync()` runs on a background queue (it used to
   block the main queue).
3. `GuestCacheDropper` drops the page cache in the guest.
4. VM pause + `saveMachineStateToURL`.
5. The process exits.

### 6.5 Daemon restart

If `vz-runner` was restarted while the VM was not saved, a cold boot
happens. If the snapshot was saved — a resume. In daemon mode the serial
port is redirected to `~/.anvil-vz/console.log`, so that
`VZFileHandleSerialPortAttachment` on stdio does not break the restore due
to the new FDs of the new process.

## 7. Persistent storage

### 7.1 Containerd root

A virtio-blk disk `~/.anvil-vz/containerd-disk.img` (ext4) is used. Why:

- **tmpfs** — images lived in RAM, the snapshot ballooned, cold boot needed
  a tarball restore;
- **a loop file on virtiofs** — produced `input/output error` on `meta.db`
  and slow writes (a 1.7 GB tarball took ~23 s);
- **virtio-blk** — a real file system, images survive a reboot, resume is
  fast.

#### Writeback cache and synchronization mode

By default `VZDiskImageStorageDeviceAttachment` works in `.full` mode:
every guest fsync triggers a flush on the host. For nerdctl/containerd,
which constantly issue small metadata writes, this resulted in
`docker stop`/`docker compose down` taking the full graceful timeout of 10 s
each, and `docker run --rm alpine` — 4+ s.

The fix — enable the host writeback cache and drop guest-fsync propagation:

```swift
let attachment = try VZDiskImageStorageDeviceAttachment(
    url: diskURL,
    readOnly: false,
    cachingMode: .cached,
    synchronizationMode: .none
)
```

After that:

- `docker ps` — ~0.06 s;
- `docker images` — ~0.04 s;
- `docker run --rm alpine echo hi` — ~2.0 s;
- `make run_tests_locally` in `pprb_uzp_efficiency` — ~50 s.

The risk: on a kill -9 of the daemon or a host panic the last metadata
transaction can be lost. Acceptable for a dev VM; this mode does not fit
production workloads. Durability in the normal scenario is provided by
snapshot save/resume.

#### Sparse raw disk + ext4 / virtio-blk tuning

Initially a `.dmg` (UDIF sparse image) was used. On random writes APFS adds
block-allocation overhead beyond EOF. We moved to a raw image — first
preallocated (zero-filled with `dd`), then sparse: the image is created
empty and immediately truncated to the target size (`main.swift`, default
64 GiB, `ANVIL_DISK_GB`). Sparse is not faster than preallocated on APFS
(blocks are allocated on first write either way), but it does not consume
host space until data is actually written, and it allows returning space
via discard (see §7.3).

Stage2 mounts it with options that minimize metadata latency:

```bash
mount -t ext4 -o noatime,nobarrier,data=writeback,commit=60 /dev/vda /var/lib
```

And tunes the block device:

```bash
echo none > /sys/block/vda/queue/scheduler
echo 256 > /sys/block/vda/queue/read_ahead_kb
echo 256 > /sys/block/vda/queue/nr_requests
echo 2 > /sys/block/vda/queue/nomerges
```

The result compared to `.none` without tuning:

- `docker ps` — ~0.01 s (was ~0.06 s);
- `docker images` — ~0.03 s (was ~0.04 s);
- `docker run --rm alpine echo hi` — ~1.2 s (was ~2.0 s);
- `make run_tests_locally` — ~40 s (was ~50 s).

> **Multiqueue virtio-blk** — in the current Virtualization.framework SDK
> (`VZVirtioBlockDeviceConfiguration.h`, macOS 14/15) there is no public
> property for the queue count, so this item is not applicable without
> upgrading Xcode/SDK.

### 7.2 Virtiofs share

`/mnt/anvil` on the host project directory. Used for:

- the guest-agent debug log;
- persisted network labels (`/mnt/anvil/networks/`);
- the `/var/lib` fallback (`/mnt/anvil/var-lib`) when no virtio-blk disk is
  configured;
- a one-time migration of the old tarball cache into
  `/mnt/anvil/var-lib/containerd`.

### 7.3 How to change memory or disk size

**Memory.**

1. Stop the service: `make service-stop`.
2. Set the desired value (GB) and start again:
   ```bash
   export ANVIL_MEMORY=4
   make service-start
   ```
   The snapshot hash includes the memory size, so changing it triggers a
   cold boot and the snapshot is re-created automatically.

**Containerd disk.**

The disk is a sparse raw image (`containerd-disk.img`), ext4 without a
partition table. It is created automatically on first start (default 64
GiB, configurable via `ANVIL_DISK_GB`) and occupies only the blocks
actually written on the host.

Growing is automatic and upward-only: at startup vz-runner compares the
target size (`ANVIL_DISK_GB`) with the current one and truncates upward
(`main.swift`). The file size is part of the snapshot hash, so the next
launch is a cold boot, and stage2 catches the ext4 up with an online
`resize2fs` right after mounting. Shrinking is not supported (the image is
never compacted — otherwise ext4 data could be lost).

Returning host space after deleting images (`make prune`):

- **Automatically**: virtio-blk in VZ supports discard, and the guest-agent
  runs `fstrim /var/lib/containerd` once a day (`periodicFstrim` in
  `guest-agent/main.go`) — freed blocks punch holes in the sparse image
  without stopping the daemon.
- **Manually and immediately**: `make disk-compact` — a sparse copy of the
  image via `dd conv=sparse` (the daemon is stopped and started again; the
  logical size and contents do not change, the snapshot stays valid).

If no disk is configured, stage2 uses the `/mnt/anvil/var-lib` fallback on
the virtiofs share — slower, but requires no manual image creation.

## 8. Known trade-offs

- **Snapshot size floor.** Even with the block disk and `drop_caches`, the
  snapshot after pulling images is ~1 GB because of containerd's active
  memory. Reducing it further is only possible with more aggressive
  containerd cache management or less RAM.
- **Partial Docker API.** Only the subset of the Docker API sufficient for
  day-to-day `docker`/`docker compose` usage is implemented. Some rare
  endpoints return `404`.
- **Single VM.** All projects live in one VM. If the VM dies, all projects
  stop. Isolation is logical (containerd namespace + CNI bridge), not
  hardware.
- **macOS only.** `Virtualization.framework` is available only on Apple
  Silicon macOS.
