# Anvil improvement plan

Priorities: 1) startup speed, 2) bugs and reliability, 3) functionality,
4) tests and CI, 5) DX and release. Within sections — ordered by impact.

## 1. Startup speed

Baseline measurements (daemon.log / console.log, July 2026):

- Cold start to readiness ≈ 8.3 s: VM start 0.12 s → guest-agent ready
  5.76 s → snapshot save 2.44 s (inline, before declaring readiness).
- Resume from snapshot ≈ 1 s.

**Result after the first iteration (done, bench-harness): cold start to
daemon-ready 1339 ms (a real cold boot; for comparison OrbStack 1560 ms,
Docker Desktop 2175 ms), resume 568 ms (Colima 13301 ms, Lima 9114 ms),
guest-agent ready 1.16 s after VM start (was 5.4–5.8 s), initramfs
109 → 51 MB, idle RSS 2766 → ~700 MB.**

Done:

1. **Honest measurements.** `cmdStatus` always exited with code 0 — now it
   is != 0 until the daemon is up and the control chain (daemon → control
   server → guest-agent health) answers (`main.swift`). bench-harness got a
   `backend_cold_reset` hook (deletes the vz-runner snapshot before the
   cold-start phase) — previously the "cold start" in the benchmark was in
   fact a resume.
2. **Lazy snapshot (−2.4 s on cold start).** `VMLifecycleManager` no longer
   does pause → save → resume before `didBecomeReady`; the snapshot is
   saved on the idle timeout and on a clean shutdown, as before.
3. **Asset sha256 cache.** The hash of kernel+initrd (~172 MB) was computed
   twice per launch; now there is a persistent cache keyed by (path, size,
   mtime) in `~/.anvil-vz/asset-hashes.json` (`SnapshotManager.swift`).
4. **Guest boot.** Removed the blocking `ntpd` (background; the VZ RTC is
   good enough); `udhcpc -n -q` instead of a 0.5 s poll; containerd.sock
   poll 0.5 → 0.1 s; the `sleep 1` after killall — only when processes
   were actually killed.
5. **Initramfs slimming 109 → 51 MB:** dropped `containerd-stress` and the
   rootless scripts (−19 MB), kept only the 6 needed CNI plugins (−56 MB),
   repacked `gzip -9` → `zstd -19` (initrd decompression 0.63 → 0.2 s),
   guest-agent built with `-ldflags="-s -w"`.
6. **Misc:** `random.trust_cpu=on` in the cmdline (crng init used to land
   at ~81 s), fast socket polling on the host (0.5 → 0.1 s),
   `anvil-service.sh` without `python3` in wait loops.
7. **Download size 126 → ~86 MB:** the kernel is packed into the release
   tarball compressed (`vmlinuz-raw.gz`, 18.5 MB instead of 59 MB) and
   unpacked once on first start into `~/.anvil-vz` (re-unpacked when the
   gz from the package is newer — i.e. on upgrade).

Remaining (postponed):

7. **Rootfs copy into tmpfs + switch_root (~0.3–0.5 s).** myinit copies
   ~165 MB into /newroot, because the initramfs rootfs is not a separate
   mount and runc's pivot_root breaks. Left as is for now (risky),
   partially compensated by the image slimming.
8. **(expensive, optional) Alpine linux-virt kernel** instead of the Ubuntu
   generic one: the image is several times smaller and initialization is
   faster. Requires picking the module set (ext4, vsock, virtiofs, bridge,
   netfilter).

## 1a. Migration to Alpine linux-virt (done)

- The kernel and modules now come from a **single apk** `linux-virt-<ver>`
  (vermagic matches by construction; Alpine netboot images lag behind the
  repo, so the netboot kernel cannot be used). The Ubuntu generic kernel
  (59 MB raw, 87 MB modules.deb, version hunting via `strings`) is fully
  removed from the pipeline; the raw kernel went from 59 → 34.4 MB (~10 MB
  gzipped), the build is deterministic.
- Pitfalls of linux-virt vs Ubuntu generic (all closed):
  - `CONFIG_VIRTIO_BLK/NET/FS/PACKET=m` (Ubuntu has them built-in): the
    `virtio_blk`, `virtio_net` (+`failover`/`net_failover`), `fuse`,
    `af_packet` modules are required (otherwise no disk, no network, no
    share, no DHCP).
  - `libcrc32c` requires `crc32c` (a softdep that busybox modprobe does not
    resolve) → we load `crc32c_generic` explicitly; without it neither
    nf_conntrack nor ext4 (metadata_csum) work.
  - No `RANDOM_TRUST_CPU` and no virtio-rng in VZ: crng init took ~10 s.
    Solved by seeding from the host: vz-runner writes 64 bytes into
    `.anvil-host-entropy`, the guest-agent credits the pool via
    RNDADDENTROPY.
- Module loading moved from explicit `insmod` lists to `modprobe`
  (modules.dep from the apk) — dependency chains no longer break.
- Result: guest-agent ready in 1.14 s (same as Ubuntu), the whole matrix
  (DHCP, ext4, share, ports, pull, save/load, compose, resume) is green.

## 2. Bugs and reliability

1. **~~virtiofs truncates large writes from the guest into the share~~ —
   investigated, it is not virtiofs.** The root cause is a race in the
   containerd 2.0.0 transfer service during `ctr images export`: the
   containerd log shows
   `error copying stream: write ...: file already closed` — the stream is
   closed before the write finishes, the file is truncated at a random
   point in the tail, while the exit code stays 0 (silent data
   corruption). On tmpfs the race is nearly invisible (0/12); on virtiofs
   it shows up because of slower writes (2/2). Regular writes of any chunk
   size (4 KB–4 MB, with and without fsync) are clean. Conclusion: use
   `nerdctl save` for exports (a different, reliable path — that is what
   `docker save` uses); do not use `ctr images export` into the share.
   Follow-up: ~~upgrade containerd/nerdctl in the image (was
   2.0.0/2.0.4)~~ — done: containerd 2.3.3, nerdctl 2.3.5, runc 1.5.1,
   CNI plugins 1.9.1. The upgrade exposed two regressions, both fixed:
   nerdctl 2.2+ dropped the `nerdctl/ports` label (ports are now read from
   the networkstore —
   `/var/lib/nerdctl/*/containers/<ns>/<id>/network-config.json`,
   fallback in `guest-agent/scanner.go`) and the guest lacked the busybox
   `find` applet (boot-time name-store cleanup silently did nothing —
   added to the initramfs). A third regression surfaced later: nerdctl
   2.3.x `inspect --format json` prints a single object instead of an
   array — `nerdctlContainerStatus`/`isNerdctlContainerRunning`
   silently returned "" / false, and attach before container start spun
   through all 100 wait iterations (every `docker run` with attach was
   +7 s). The parser now accepts both formats
   (`containerStateFromInspect` in `guest-agent/containers.go`).
2. **~~`docker save` not implemented~~ — done.** `GET /images/{name}/get`
   and `/images/get?names=...` stream `nerdctl save` (docker format, the
   reliable path unlike `ctr images export` — see item 1). The save → load
   round-trip is verified. Later, resolving a name without a tag was also
   fixed (`docker save foo` matches `docker.io/library/foo:latest`, and a
   `:` in `host:5000/foo` is no longer mistaken for a tag) —
   `findImageNamespace` in `guest-agent/images.go`.
3. **~~Socket and state-dir permissions~~ — done.** `control.sock` and
   `docker.sock` are 0600, `~/.anvil-vz` is 0700, `containerd-disk.img`
   is 0600 (chmod after bind/creation, applied on every start).
4. ~~**Stale comment in the Makefile** (target `prune`): "docker run --rm
   is not yet implemented"~~ — already cleaned up during one of the
   iterations; AutoRemove has long been implemented
   (`guest-agent/containers.go:489`).
5. **~~Flakiness of the first `docker run` after a cold start~~ —
   fixed.** The root cause was not the "first request" but `--rm`:
   AutoRemove deleted the container (and its json-file logs) right after
   exit while attach was still replaying output → short-lived containers
   (`echo ...`) lost stdout nondeterministically. Now attach is tracked
   (attachBegin/End) and removal waits for the drain up to 30 s; also,
   attach waits for the container to leave `created` (attach arrives
   before start). Verified: 10/10 warm and 2/2 cold produce correct
   output. Separately, a rare transient `nerdctl start failed (1): <id>`
   (~1/17 rapid consecutive runs, shim level) was noticed — not
   reproducible on the current stack (containerd 2.3.3 / nerdctl 2.3.5 /
   runc 1.5.1): ~440 runs (sequential, parallel, burst after resume) are
   clean. Left under observation.
6. **~~Resume compose-up regression in the benchmark (5567 ms)~~ —
   fixed.** The root cause was a deadlock in `runExec` (guest-agent):
   stdout and stderr of the child process were drained sequentially — as
   soon as `nerdctl compose up` wrote more than the pipe buffer (64 KB) to
   stderr with stdout open, the child hung forever in `pipe_write`. Now
   both streams are drained concurrently. As a side effect, `vz-runner
   exec` for "noisy" commands was fixed too. After the fix, resume
   compose-up is ~1.3 s.
7. **Initramfs build in Lima no longer depends on the network.**
   Previously the script inside the Lima VM re-executed itself in nested
   docker and ran `apk add` (requiring access to the Alpine CDN) — with a
   broken or restricted network the build died. Now on a Linux host (Lima)
   the build runs directly: packages are installed only when missing,
   working directories live in mktemp instead of the filesystem root.

Done while investigating `docker load` and during the speed iteration
(July 2026, already in the code):

- `DockerProxyServer.swift`: buffer writes loop in both directions — a
  partial `write()` used to silently drop the tail of a 64-KB chunk.
- `canonicalizeImageRef` (`guest-agent/images.go`): a short name with a
  tag (`myimg:1`) was mistakenly treated as registry-qualified because of
  the `":"` check in the first segment — normalization to
  `docker.io/library/...` did not kick in, `ensureImageInNamespace` did
  not find the local image and fell through to `nerdctl pull`. With a
  reachable registry this was a hidden extra pull on every `docker run`
  (latency); without one — container creation failed. A raw-name fallback
  was added (images from OCI archives with a raw `ref.name` now get
  aliased to the canonical name).
- `/images/load` rewritten onto the containerd Go client
  (`client.Import`): the body is streamed without a temp file — a ~430 MB
  tar used to kill the guest tmpfs ("no space left on device"). gzip/zstd
  are detected automatically. The response contains `Loaded image: <ref>`
  lines like real Docker. Archives without name annotations (typical
  `buildx --output type=oci` without `-t`) used to "load" silently but the
  image was unreachable by name and `docker run` went to a registry pull —
  now such manifests get a digest-based ref
  (`docker.io/imported/anvil-image:<digest12>`). Archives with a raw name
  (`myapp:1` without a registry prefix) were registered as-is, while
  nerdctl canonicalizes short names to `docker.io/library/...` on
  inspect/run → `GET /images/{name}/json` failed with "no such image" and
  the CLI/compose fell back to pull (fatal offline). Now a canonical alias
  is registered at load time too (same target, same namespace).
- `anvil-service.sh`: in the source tree, fresh assets from `.download`
  and the fresh `.build/release/vz-runner` binary now take priority over
  the copies in `~/.anvil-vz` and PATH — otherwise after
  `make rebuild-all` the service kept loading the old initramfs with the
  old guest-agent and the old brew binary.
- DNS in the guest: myinit no longer overwrites `/etc/resolv.conf` with a
  hardcoded 8.8.8.8 — DNS comes from DHCP (the VZ NAT gateway proxies the
  host resolver, works in VPN/restricted networks), 8.8.8.8 only as a
  backup. Previously, on networks with blocked external DNS, pulls hung
  without any symptom.
- **Loud failure on a taken host port.** Previously, when a localhost port
  was already held by someone else's process (Docker Desktop with the same
  compose project, Lima with auto-forwarded guest ports, a local brew
  postgres), PortForwarder wrote "bind/listen failed: Address already in
  use" only into daemon.log while the container started "successfully" —
  unreachable from the host, with confusing application failures
  (connection-pool timeouts). Now the guest-agent, when starting a
  container with `-p`, asks vz-runner whether the ports are taken (the
  guest dials vsock port 1027, `PortCheckServer`; a port counts as taken
  when it is not held by our forwarder and cannot be probe-bound — the
  probe runs in both families, since an IPv4-only squatter does not
  prevent a dual-stack bind on macOS), plus conflicts with already
  running anvil containers are checked — start fails with the Docker-style
  error `Bind for 0.0.0.0:<port> failed: port is already allocated`. The
  check is deliberately on start, not on create: Docker checks ports at
  start, and compose `--force-recreate` creates the replacement under a
  temporary name `<id>_<name>` while the old container still holds the
  port. Ports held by our own forwarder do not count as taken (the old
  container's listener may still be coming down when the new one starts).
- **nerdctl network-store pruning.** The cold-boot cleanup in stage2
  removes containers bypassing `nerdctl rm`, and `nerdctl rm` itself does
  not always remove the record — the files
  `/var/lib/nerdctl/<hash>/containers/<ns>/<id>/` (network-config.json
  etc.) accumulated on the persistent disk forever. Now the record is
  deleted on `docker rm`, and at startup the guest-agent cleans out all
  records without a live container in containerd.
- ~~Known limitation: nerdctl runs its own host-port check on create, so
  `compose up` over LIVE containers (compose creates the replacement before
  stopping the old one) failed with "bind for :<port> failed: port is
  already allocated"~~ — fixed: `-p` flags are no longer passed to nerdctl
  at all. nerdctl reserves host ports at CREATE time with an inherited
  listener fd, which is incompatible with Docker's check-at-start
  semantics. Port mappings are persisted in nerdctl's network store
  (metadata for ps/inspect/scanner), publishing goes through the guest-side
  port proxy (single TCP port, header describes the CNI target), and
  conflicts are enforced by our own start-time checks. Same fix removed the
  compose-run CLI panic: `docker inspect` now returns HostConfig
  (AutoRemove/PortBindings) and Config Tty/Env/Cmd — the CLI nil-derefs
  HostConfig.AutoRemove in `container.RunStart` otherwise.
- buildkitd now runs with the containerd worker (namespace `default`) instead
  of the default OCI worker: `FROM` resolves from the local image store, so
  builds no longer request OAuth tokens from Docker Hub when every base
  image is already local (a broken/restricted network used to fail the
  build even with local images).
- `/images/load`: added `client.WithSkipMissing()` — single-platform
  exports with a multi-arch index (without foreign blobs) no longer fail
  with "content digest ... not found".
- **The root bug of "docker run/compose pulls from the registry after
  docker load"**: the containerd content store is namespaced.
  `ensureImageInNamespace` copied only the image metadata into the
  compose project's namespace, and the record pointed at invisible blobs
  → nerdctl went to pull (HEAD against the registry) → failure without
  registry access (and with a network — a hidden pull on every compose
  up). Now the image is streamed between namespaces
  (`nerdctl save | ctr images import -`) — works fully offline (verified
  with an empty resolv.conf).
- **Guest clock**: VZ does not guarantee the RTC — the boot started at
  1970-01-01 and TLS failed ("certificate is not yet valid"). myinit
  processes do not survive switch_root, so ntpd from myinit did not help.
  Now vz-runner writes the epoch into `<share>/.anvil-host-time` at VM
  start, myinit sets the clock from the file, ntpd in stage2 is only a
  background drift correction, and the guest-agent re-reads the file at
  startup and on every subscribe (covering resume with "frozen" clocks).
  Additionally: vz-runner rewrites the file in `resume()` as well —
  otherwise after an idle-pause of the daemon the file stayed from
  yesterday and the guest-agent returned the clock as of the pause moment
  on every subscribe (the clock "froze" for the whole idle period).
- `anvil-service.sh`: for brew installations, package assets take
  priority over the copies in `~/.anvil-vz` (otherwise an old initramfs
  silently shadowed every upgrade), plus a warning about ignored shadow
  files.

## 3. Functionality

1. **~~`docker build`~~ — done (classic path + buildx remote driver).**
   buildkitd (v0.32.2) + buildctl were added to the initramfs (buildctl is
   needed: nerdctl build invokes it as a subprocess); the guest-agent
   implements the classic `POST /build`: the context is unpacked onto the
   persistent disk (`/var/lib/anvil-build`), the build runs via
   `nerdctl build`, progress is streamed as a JSON stream like Docker's.
   buildkitd starts lazily on the first build (saving ~50 MB RSS).
   Additionally the buildkitd socket is forwarded to the host:
   `~/.anvil-vz/buildkit.sock` → vsock:1026 →
   `/run/buildkit/buildkitd.sock`; `anvil start` creates the buildx
   builder `anvil-remote` (remote driver) and makes it active (the
   previous builder is saved and restored on stop) — the default
   `docker build` (buildx) works out of the box; importing into the image
   store needs `--load` (compose does that itself). Both paths work: the
   remote driver (`docker buildx build`) and the docker-container driver
   (bare `docker build` on a desktop CLI pulls moby/buildkit as a
   container — for that, `PUT /containers/{id}/archive` on a stopped
   container was fixed, plus two stdin bugs in exec: a `cmd.Wait()`
   deadlock and dropped stdin that broke `buildctl dial-stdio`).
2. **Rosetta for x86_64** (`VZRosettaDirectoryShare`, macOS 13+) — run
   amd64 images almost natively, like Lima.
3. **~~Bind mounts of arbitrary host paths~~ — done.** vz-runner shares
   the host directory `/Users` via a second virtiofs device (tag
   `macusers`); stage2 mounts it in the guest at the same absolute path
   `/Users` — `docker run -v $HOME/proj:/data` and compose `volumes:`
   with relative paths work without path rewriting, like in Docker
   Desktop. Disabled with `ANVIL_SHARE_USERS=0`. The set of shares is part
   of the snapshot hash (adding a device breaks restore) → one cold boot
   after the upgrade. Along the way: the guest-agent used to silently
   ignore `HostConfig.Binds`/`Mounts` — now they are passed to `nerdctl
   -v` (including named volumes); `GET /events` is implemented (streams
   containerd task events in Docker format: create/start/die with
   exitCode/destroy), without it `docker compose up` got a 404.
4. ~~**Fill in the stubs**: dangling-image prune and build-cache
   prune~~ — done. `/images/prune` removes dangling images by default and
   all unused ones with `dangling=false` (`docker system prune -a`),
   computing SpaceReclaimed from the sizes in `listDockerImages`.
   `/build/prune` runs `buildctl prune` over the buildkit socket
   (buildkitd is NOT started for it — no daemon, no cache) and parses the
   reclaimed value from the `Total:` line.
5. ~~**Docker CLI compatibility waves**~~ — done (v1.0.51–v1.0.54).
   Progressive hardening driven by the integration suite, in waves:
   entrypoint/workdir/add-host/memory/caps; read-only/stop-signal/tmpfs/
   pid:host/net:host; dns/sysctls/devices; `--link name:alias` (legacy
   links are plain /etc/hosts records — applied at start via the same
   mechanism as compose service aliases, since nerdctl has none);
   `--restart` policies (guest-agent restart monitor over the containerd
   task state — nerdctl's own supervisor races Docker semantics, so the
   flag is never passed to nerdctl); UDP port publishing (`-p .../udp` via
   a host-side datagram relay to `guestIP:hostPort`, where nerdctl arms
   the persisted mapping with nft DNAT); `docker events` filters and
   `--until`; `pause`/`top`/`stats`/`system df`. Along the way the API
   handshake was fixed: `/_ping` now sends `Ostype`/`Api-Version` headers
   (the CLI resolves the server OS from the ping header and downgrades to
   the advertised version — must be >= 1.40 for compose).

## 4. Tests and CI

1. **CI run of the robustness suite.** The `validate` job was added, but
   it turned out that GitHub-hosted macos-15 runners provide no
   hypervisor (`VZErrorDomain Code=2 "Virtualization is not available on
   this hardware"`) — boot tests cannot work there in principle. So
   validate is wired to a self-hosted Apple Silicon runner triggered via
   workflow_dispatch; the release does not depend on it. Locally —
   `make validate`.
2. **Go unit tests for guest-agent** covering the invariants from
   ARCHITECTURE.md §4.3: deterministic container ID,
   `/containers/{id}/wait` (headers before blocking), the AutoRemove
   lifecycle. `httptest` against the handlers.
3. **Swift tests**: `ControlProtocol` parsing, the snapshot hash.

## 5. DX and release

1. ~~**`anvil doctor` / `anvil logs`**~~ — done: `doctor` checks the
   hypervisor, the entitlement, the presence of kernel/initramfs/disk,
   free space, the daemon, the docker context and the API answer on
   `/_ping`, the `/Users` share (exit code ≠ 0 on errors);
   `logs [daemon|console|guest]` shows log tails with one command.
2. ~~**`guest-agent.log` rotation**~~ — done: in debug mode the
   guest-agent manages the file on the share itself (`logrotate.go`):
   dup3 of stdout/stderr onto its own descriptor and size-based rotation —
   above 50 MiB the current log moves to `guest-agent.log.1` (one backup),
   checked once a minute. At most ~100 MiB on the host regardless of the
   debug-session length.
3. **Developer ID + notarization** — for now `spctl` rejects the binary;
   irrelevant for brew, but manual tarball downloads hit Gatekeeper.
   Requires a paid Apple Developer account.
4. ~~**Homebrew bottle** instead of a source formula~~ — done: the
   formula carries a bottle block (`root_url` pointing at the GitHub
   release, `cellar: :any_skip_relocation`, tag `arm64_tahoe`), the
   bottle tarball is published as a release asset. Automation:
   `make bottle VERSION=x.y.z` (`scripts/make_bottle.sh`) — reinstalls the
   formula with `--build-bottle`, runs `brew bottle`, renames the tarball
   (brew looks for an asset with a single dash `anvil-1.0.x...` while
   `brew bottle` creates a double dash), uploads it to the release via
   `gh`, updates the bottle block and pushes the tap. On a version bump
   `make update-brew` cleans out the stale bottle block (otherwise rebuild
   gets incremented from the old one). On macOS older than tahoe the
   bottle tag will not match — brew silently falls back to the source
   formula, which works identically (same file unpacking).
5. ~~**Containerd disk compaction**~~ — done (§5.5): the default size was
   raised 16→64 GiB (sparse, configurable via `ANVIL_DISK_GB`), an
   existing image is grown automatically at startup (the snapshot hash
   includes the file size → the next launch is a cold boot, stage2 catches
   ext4 up via online `resize2fs`; mkfs.ext4/resize2fs are now musl
   binaries from the Alpine apks e2fsprogs/e2fsprogs-extra, not glibc
   copies from the build VM). To return host space after `make prune`,
   `make disk-compact` was added (a sparse copy via `dd conv=sparse`,
   contents and logical size unchanged — the snapshot stays valid). Plus
   automation: virtio-blk in VZ supports discard, so the guest-agent runs
   `fstrim /var/lib/containerd` once a day (`periodicFstrim` in `main.go`,
   the busybox applet added to the initramfs) — holes in the sparse image
   get punched on their own, without stopping the daemon.
