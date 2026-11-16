## v1.10.1 (2026-11-16)

### Changed
- 5ee1f99 refactor: share the pure OCI helpers between the helper and the agent

### Docs
- 44fb08a docs: stage 6c measured — native init helper

### Internal
- b2e726d ci: skip the Swift build when the change cannot affect it

### Other
- f4005d8 guest-agent: run the container init from a precomputed plan
- 0f1fe78 guest-agent: share the cgroup v2 helpers with the agent
- 72b5a2a guest-agent: trace the helper's gate, attrs and exec phases
## v1.10.0 (2026-11-16)

### Added
- 1464f40 feat(images): forced mirror pulls via mirror.anvil/ refs and anvil images push

### Docs
- 57d3092 docs: 6b.1 measured — create 9.7 -> 4.6 ms; 6b.2 a no-go
- fbc95e2 docs: 6d green — 150/0 integration, CLI median 92 ms (min 50.6)
- ef86c99 docs: 6d measured — three poll intervals were ~180 ms of the CLI run
- 00afaa3 docs: 6a measured result (cni-attach 12.7 -> 1.26 ms, API 82.8 ms) and the duplicate-IP trap
- b723530 docs: 6a design — native networking for the default bridge hot path
- 1e9cc4a docs: what a kiri-style Rust rewrite would and would not buy
- dad1ec2 docs: rust decision note — targeted init only, after the architectural rungs
- 2fa85be docs: review the external optimization guide against our measurements
- 7fd62fa docs: rewrite stage 6 against kiri's published budget
- 54f10cc docs: stage 6 plan (networking without the CNI plugin, lean create, optional no-exec init)
- b6c8690 docs: stage 5 correctness pass and final measured state
- f9d764e docs: stage 5 measured rungs (CLI 556 -> 192 ms, API 495 -> 128 ms)
- 8eaa3ff docs: stage 5a measured results
- 50dbda7 docs: stage 5 design (direct container creation, no shim in the hot path)
- 1626c89 docs: stage 4 in scope; implementation plans for stages 1-3 and stage 4
- 9c679bd docs: stage 2 shim gating: default-on with fallback (Oleg's call)
- 9f8a823 docs: resolve builder-check questions in latency design
- 76cb349 docs: design spec for container start latency stages

### Internal
- 69f5669 ci: build the shim binaries the initramfs now needs
- 116b316 build: keep server-only shim code behind the shimserver tag
- a7678ac build: retry apk installs in the initramfs build
- cca0eee build: initramfs always builds on the anvil docker context
- 9f3cfc1 style: gofmt image_mirror_test.go

### Other
- 490efef agent: memoize the image facts every create re-derived
- a8a2be2 agent: drain the attach at a fine cadence before removing
- 20229d4 agent: probe the registry finely while /wait polls for a start
- 69501ea agent: poll the removal finely on the --rm wait
- a089922 initramfs: let the host pick the guest CNI mode
- f1634e0 agent: create the default bridge endpoint in-process
- 5be9f90 agent: spawn the helper-only binary for the direct start path
- 9741ff1 shim: build the helper modes without the ttrpc server
- ac16c06 Revert "agent: attach the network alongside the helper setup"
- 5669dbc agent: attach the network alongside the helper setup
- 35a0e3a agent: log a direct container's namespace identity in debug builds
- 5eb4f9e agent: cover the direct cgroup update with a test
- bd89098 shim: enable cgroup controllers top-down
- 8ba70a0 agent: unlock the exec pty before opening its slave
- e1e0ac2 shim: write the swap-only value to memory.swap.max
- 00834e4 agent: apply docker update limits to a direct task's cgroup
- 8e89a78 agent: drop a stale direct record when a start takes the containerd path
- 7341182 agent: allocate an exec pty in the container's devpts
- 98b6728 agent: let an exec's stdin drain before signalling EOF
- cee050a shim: keep capabilities across a non-root user change
- a760959 shim: honor the exec working directory
- 60e2cdd shim: keep --pid host containers off the fast and direct paths
- b9ec423 shim: mount the container's own cgroup subtree in a private cgroup namespace
- a2db6f7 shim: birth stage B into its cgroup (clone3 CLONE_INTO_CGROUP)
- 72476cb shim: keep every fixed fd below NR_OPEN_DEFAULT
- ee0a8da guest-shim: split the helper trace into per-step wall and CPU marks
- 480a172 agent: detach the endpoint off the DELETE response path
- 5419a97 agent: move the helper trace to tmpfs, copied out after setup
- 03f46c7 agent: give a direct TTY exec the pty slave as its stdin
- 0c75e09 agent: keep an --rm container's exit code readable after its removal
- fa054ea agent: answer a removed container's next-exit wait instead of erroring
- 76d6f8c agent: end attach on the deterministic stream end, not a 100 ms linger
- 8d808ee agent: trace the direct init's setup phases; keep healthchecks and cgroup cleanup on fallback
- 8cca9b8 agent: start eligible containers directly, no containerd task or shim
- ca8872b agent: direct-task registry for agent-supervised containers
- 1d7e4ef shim: let the init helper create and remove its own cgroup (-cgroup)
- a37bedd shim: hold TaskExit until TaskStart is published
- 3337257 shim: exec two-stage ns join in a raw window, stage B applies the attrs
- e47c998 shim: report setns failures from the raw exec window
- d759ab6 shim: chroot the exec helper into the init's root
- 89d868f shim: resolve the exec path before the filter in exec helpers
- 44d4b6a shim: drop the create/start timing probes (temporary diagnostics)
- b92b56e shim: finer init-helper probes with absolute mono clocks (temporary)
- 128c84e shim: init helper stage timing prints (temporary)
- 9c6a5f3 shim: create/start phase timing probes (temporary)
- dd72273 logging: follow reads the eof marker promptly, stop condition throttled
- cf852c6 shim: install seccomp via seccomp(2); secprobe diagnostic subcommand
- 4f92196 boot: sweep stray fast-shim processes and dead shim sockets at boot
- edabcda shim: drop the seccomp install probes (temporary diagnostics)
- 237f8ea shim: resolve the exec path before the user attributes (and the filter)
- 708e759 shim: debug probes around the seccomp install (temporary)
- a03684c shim: the default action maps to the bare value, not the EPERM-carrying one
- a9b763b shim: the policy carries the bare default action; the patch adds EPERM
- a823928 shim: assemble the seccomp program in the shim; default deny is EPERM
- c1df7c0 shim: ActErrno defaults to EPERM - a bare RET_ERRNO fakes success
- 8223a4a shim: the exec-not-found diagnostic reports each stat error and mode
- 80ca42b shim: the exec-not-found error names the paths the helper tried
- 31d2353 shim: carry the requested errno on ActErrno (clone3 must answer ENOSYS)
- c81a3bf shim: map SCMP_CMP_MASKED_EQ to bit conditions; unbreak the default profile
- 0f77cf8 shim: file bind mounts bind onto files, like runc
- 442ee59 shim: the next-exit wait polls through the fast-shim fallback gap
- 6905dcc shim: bind the cgroup root like runc; join the private cgroup namespace
- d96fca8 shim: publish TaskOOM from the cgroup oom_kill counter
- 5a7ce28 shim: default-on eligibility with silent runc fallback
- 3b558b9 shim: publish TaskStart on init start; report lines at debug level
- b160c80 shim: queue TaskExecStarted in report order, not start-rpc order
- 5d8ce42 shim: Wait re-reads the exit code once the wait channel closes
- 84eef42 shim: detached io hands /dev/null to the helper's fixed-fd trio
- 2d50650 shim: review fixes: real pids in create/exec, fifo sinks close at init exit, exec fd hygiene
- 6e94120 shim: two-stage init: stage B born in the pid namespace via raw clone
- 81229a6 shim: start confirmation via close-on-exec pipe
- f4acae2 shim: wait/kill/delete/exec/stats with io parity
- 075b1e3 shim: create/start via trampoline, mounts, cgroups
- 6dc7e62 logging: trailing eof marker ends the follow only when the stop condition agrees
- de3c80c shim: anvil-fast-shim skeleton (runtime-v2 ttrpc service)
- ce969cc logging: pin tty opt-out from eof marker
- f36d5c5 logging: follow ends deterministically on eof marker
- 9b3d71d logging: deterministic eof marker from pid watch / stream eof
- 35ff1f2 logging: eof marker record, skipped by replay
- 627d1e3 bench: CLI-shaped API op timing with step breakdown
- 94aeb77 bench: per-phase container start latency tracing (debug builds)
## v1.9.12 (2026-11-13)

### Added
- 376f8a7 feat(host): anvil restart --restore starts the containers that were running

### Docs
- fc7dbb3 docs: the build cache cap is in GiB and excludes layers shared with images
- c84496d docs(changelog): drop the duplicated v1.9.9 section

### Internal
- d60bb69 build: Go 1.26.11 (govulncheck: GO-2026-6859 fixed in 1.26.11)
## v1.9.11 (2026-11-10)

### Changed
- c9019ad perf(boot): keep netfilter setup and the rootfs copy off the guest critical path

### Internal
- a5f54c2 test(validate): report why no cycle resumed, not "division by zero"
- 9cb0217 test(integration): probe egress on reachable targets, not 1.1.1.1
- c12cae7 ci: run the VM suites on the self-hosted runner in an isolated ci instance
- 3e8a8bd build(release): wait for the release archive, not just the release
## v1.9.10 (2026-11-06)

### Fixed
- 6116afd fix(containers): accept the local log driver
- 161ea67 fix(top): honor the ps_args query parameter
- 0d0c9d6 fix: five review findings on the release branch

### Internal
- fd53df6 test(top): drop the unused fixture value staticcheck flags
- b7d2f03 ci: validate stays workflow_dispatch-only while no runner exists
## v1.9.9 (2026-11-06)

### Fixed
- dc781f4 fix(cli): drain the subprocess pipe before waitUntilExit
- 75ac800 fix(daemon): saveSnapshot fails without a config hash; shutdown stops the fs-events forwarder
- 7d8f90c fix(doctor): --json escaping without force unwraps
- 8dc272d fix(control): bounded client I/O and full writes in ControlServer
- 7c89afa fix(port-forwarder): the UDP relay loop owns its client sockets
- 585d794 fix(exec): close the read pipe ends only after the stream readers drain
- b7559c6 fix(images): the GitHub token no longer appears in curl's argv
- bc992f2 fix(images): mirror download bounds, unpack errors, empty mirror tag
- 9ba3180 fix(exec): close the stdio pipes on every failed exec start
- 43f29c4 fix(cp): a failed bind mount on a stopped container fails the cp
- 97f4dc3 fix(daemon): the FSEvents forwarder can no longer outlive its owner, and vm reads are lock-guarded
- c961faf fix(containers): a failed net.json write no longer starts a half-networked container
- e3cfc9c fix(buildx): the /grpc bridge no longer leaks a server per build
- 6669342 fix(stop): docker stop/restart -t -1 waits instead of using the default
- e98dfd5 fix(exec): a second exec start no longer runs the command twice
- 02cb947 fix(idle): ClientTracker reads the suppression flag under its lock
- 27721bd fix(idle): the NAT egress probe no longer keeps the VM from pausing
- 7c6e727 fix(build): a stale-cache prune no longer hangs the build forever
- 185883b fix(cli): anvil found through a symlink finds its source tree
- 7e0d045 fix(guest-agent): a container name over 64 bytes no longer fails the start

### Changed
- a68b784 refactor(archive): request-scoped contexts for the cp paths

### Docs
- 0eb4aff docs(changelog): v1.9.9

### Internal
- f76e83a ci: fix release bundle checksum check; release waits 40 minutes
- f885aef build: ci-checks pins the go.mod toolchain, like CI does
- d12cd11 ci: release does not wait for the absent self-hosted runner
- 9257513 build: make release runs the CI checks job first
- d0456c4 style: gofmt volume_copyup.go
- 46d09de ci: native linux/arm64 tests, VM-validated releases, bundle smoke, https downloads
- b3a3052 build: bump klauspost/compress to 1.20.2 (govulncheck: GO-2026-6865 fixed in 1.20.2)
- 25357e6 build: Go 1.26.10 (govulncheck: GO-2026-6809 fixed in 1.26.10)
- bcc1032 build(deps): bump the go-deps group across 1 directory with 5 updates
- 14d1bc5 build(deps): bump the actions group with 2 updates
- 89989e2 build: update-brew waits for the release asset

## v1.9.8 (2026-10-11)

### Fixed
- a57cc9a fix(cli): anvil restart after brew upgrade stops the running daemon
## v1.9.7 (2026-10-11)

### Fixed
- 5d68bf7 fix(nat): a single failed egress probe no longer reattaches the NAT
- 2a76627 fix(volumes): a named volume is one volume across compose projects
- d352f1e fix(events): --until in the past replays the window, as dockerd does
- 7c164ca fix(net): container /etc/hosts is updated in place, never empty
- fc9fa95 fix(exec): exec -t without -i no longer dies of SIGHUP
- 3fe5169 fix(exec): docker exec -u <name> runs as that user
## v1.9.6 (2026-10-09)

### Fixed
- 6caa3d5 fix: the VM raises inotify limits (8192 instances, 1048576 watches) and vm.max_map_count like Docker Desktop: several k3d clusters ran out of inotify instances
## v1.9.5 (2026-10-09)

### Added
- e8ab5ca feat: ANVIL_INSTANCE — a second, independent anvil (state, VM, context anvil-dev) for development next to the usual service; validate kills only its own instance

### Fixed
- ae6710e fix: the agent recognizes any instance's docker.sock bind; tests and validate stay on their own instance (docker env, buildx builder, orphan count)

### Internal
- c971ddf test: relay tests read with poll (a blocked read slept through the half-close and hung swift test for an hour)
- df237a8 build: Go 1.26.9 and golang.org/x/net 0.60.0 (govulncheck: stdlib html/template, net/http; x/net)
## v1.9.4 (2026-10-08)

### Added
- 84bb965 feat(doctor): vm route — the Mac must route the VM's address to the NAT bridge
- 8fd8489 feat: the VM's NAT is reconnected when the Mac's network changes (VPN on/off) and the guest lost its internet

### Fixed
- 052116f fix(nat): the egress check probes IPv4 (IPv6 through the NAT hid a dead IPv4 path); the NAT watcher also checks after wake and every minute, one reattach per incident

### Internal
- 8f70a14 test: network disconnect live serves lo with nc (alpine's busybox has no httpd)
## v1.9.3 (2026-10-06)

### Fixed
- 2395a24 fix: network disconnect of a running container's primary or last network; a secondary's DEL no longer takes lo down

### Internal
- 8d25f28 test: the running primary network may be disconnected now (covered by network disconnect live)

### Other
- e3578fa bench: honest summary lines, ops phase (run --rm, stop -t 0, compose down, 15 services)
## v1.9.2 (2026-10-06)

### Fixed
- 200a51d fix: host relays poll before reading and drain the guest side eagerly

### Docs
- 20e1e45 docs: host relays (poll, guest-side drain), network rules k3d relies on, k3d/exec -i support
## v1.9.1 (2026-10-06)

### Fixed
- ddf4577 fix(partial): hijacked stdin is drained eagerly; save | exec -i into a not-yet-reading process locks the guest's vsock less often (not fixed: Linux 6.6 vsock rx lockup)
- 949dce3 fix: ship nft expression modules (limit, log, ...) and xt_pkttype: k3s NetworkPolicy failed iptables-restore with ENOENT
- 8c28b76 fix: a restarted container gets its previous IP back when free (k3s nodes kept their node IP)
- 751ddeb fix: a stopped proxy's deinit deleted the restarted daemon's docker.sock; status fails without the socket
- 2bfec8c fix: network rm deleted no bridge (read the loopback plugin); a reused subnet lost egress to the leaked bridge's route; orphan bridges swept on network create
- 6fed12d fix: mounts are applied parents first (a tmpfs on /run covered the Docker socket bound inside; k3d image import)
- 2484101 fix: /etc/hosts, resolv.conf and hostname are writable unless --read-only (k3d rewrites /etc/hosts)
- bbb6b37 fix: docker exec -i passes stdin EOF to the process (CloseIO on client half-close)
- d282097 fix(k3d): id list filters are regular expressions; NetworkMode joins only without EndpointsConfig

### Internal
- e465809 test: port-follows-new-IP forces the IP change (a restart now keeps its address)
- 7c3a085 test: exec -i stdin EOF, k3d-style create (id regex filters, EndpointsConfig-only network)
## v1.9.0 (2026-10-03)

### Added
- ae5b32e feat: IPv6 on user-defined networks (dual-stack, ULA by default, NAT egress, internal isolation)
- 9994a02 feat: freed disk space goes back to macOS (FITRIM after deletes; virtio-blk passes discards)
- 1e658ef feat: build cache capped at ANVIL_BUILD_CACHE_GB (default 20) through buildkit GC
- ece93ac feat: docker events --since survives cold boots (event log kept on the VM disk)

### Fixed
- 81fb0e3 fix: a short-lived container saw no peers in /etc/hosts (own hosts block written before the task starts)

### Changed
- 057af57 refactor: the daily trim uses the same FITRIM path as the post-delete trim

### Internal
- c00db59 test(validate): host ports picked free instead of fixed 8080/8081
- 46c0901 test: IPv6 network, hosts populated before start
- 2e4ac9b build(release): bottle uploads retry; make bottle-resume finishes a failed run
## v1.8.1 (2026-10-03)

### Fixed
- 0cb1acd fix: snapshot config hash is taken when the VM is configured, not at save

### Internal
- ff41d58 build(deps): Dependabot skips the VM-coupled modules and grpc 1.84
- 85d525e build(deps): GitHub Actions and Go dependency updates
## v1.8.0 (2026-10-02)

### Added
- 2b1db8b feat: container domains, http://<name>.anvil.localhost (ANVIL_DOMAINS=1)

### Fixed
- a16d2fc fix: review of the 1.7 features
## v1.7.0 (2026-10-02)

### Added
- 59d83ca feat: GET /containers/{id}/attach/ws
- a9f0a5d feat: launchd supervises the daemon (anvil-service.sh run)
- 1fa07c2 feat: privileged containers get the VM's devices; k3s runs in a container
- b8d1a5b feat: file change events in bind mounts, idle memory release, host-network ports
## v1.6.0 (2026-10-02)

### Added
- f1ee160 feat: containers survive cold boots; exec env, healthchecks, static IPs and API fixes

### Fixed
- 3eae886 fix: subpath mounts pinned at start, cp -a, static IPs outside ip_range; review fixes
- 4ead2c7 fix: idle pause froze running containers; Testcontainers, stdin, exec -it and Docker API parity
## v1.5.0 (2026-09-26)

### Added
- 03b3852 feat: real docker system df; build context through the chrooted Go tar; make smoke; runner setup docs

### Changed
- 245d5a9 perf: compose up/down on many services, docker stop -t 0
## v1.4.1 (2026-09-25)

### Fixed
- bcdabcc fix: docker volume prune removed volumes in use; log rotation; health_status events

### Changed
- 330dd77 perf: docker run returns ~4x faster (2.5 s -> 0.6 s)

### Internal
- a5628b7 test: event tests select their own container/network across runs
- c65ef96 test: event tests tolerate a guest clock that trails the Mac's
## v1.4.0 (2026-09-25)

### Added
- 5c0f7f8 feat: volume copy-up, image VOLUME, inspect Mounts, real docker stats, network events, Rosetta AOT; host relay hardening

### Docs
- 0d6a06e docs: Rosetta AOT cache effect

### Internal
- c046ccd test(validate): restart via a failing workload (docker kill disables the policy); recycle the UDP echo server
- 1ed8d5f test(validate): --network goes after the docker subcommand
## v1.3.3 (2026-09-25)

### Fixed
- dd6ce27 fix(security): docker cp/export cannot escape the container root; review fixes
## v1.3.2 (2026-09-25)

### Fixed
- 00a3b15 fix: docker cp without a shell in the image, --network none, network rm in use, save under GC
## v1.3.1 (2026-09-25)

### Fixed
- d07e3cc fix: multi-network containers, live network connect/disconnect, restart policy re-armed by start

### Internal
- 5d15350 build(guest-agent): go.mod lists libcni as a direct dependency
## v1.3.0 (2026-09-25)

### Added
- bc8b6d3 feat: linux/amd64 containers through Rosetta (opt-in, ANVIL_ROSETTA=1)
- c89a02d feat: docker update, docker commit, SSH agent forwarding, Mac localhost via host.docker.internal
## v1.2.0 (2026-09-25)

### Added
- 1c53a66 feat(guest-agent): Docker Desktop parity — host.docker.internal, seccomp, --init, volumes-from, history, search, export, diff
- 7f89775 feat: registry traffic falls back to the Mac when the VM has no internet
- 338e771 feat: diagnose a VM without internet access (full-tunnel VPN on the Mac)

### Fixed
- af9659b fix(host): published ports broke when the Mac routed the vmnet subnet elsewhere
- f1a298e fix(guest-agent): port forwarding dialed a stale guest IP after a new DHCP lease
- 1149875 fix(guest-agent): output without a trailing newline was lost
- dffe271 fix(guest-agent): docker wait after docker run -d returned 0 immediately
- 547d5c3 fix(host): anvil doctor failed on healthy setups and tested the wrong engine
- fe4f111 fix(guest-agent): staticcheck findings — network connect answered 500, dead namespace listing
- aafe8cb fix(guest-agent): compose ipam.config subnet was reported but never applied
- 40f0b1b fix(guest-agent): networks could share a subnet or a bridge
- ab3ac7b fix(host): SIGINT/SIGTERM handlers called non-async-signal-safe code
- f59f3d9 fix(host): partial snapshot saves and a failed restore forced repeated cold boots
- 1d920c9 fix(host): listener fds closed twice; accept loops spun at 100% on EMFILE
- 97c354e fix(host): late vsock connect completions leaked connections and raced the waiter
- fa3d349 fix(host): a liveness restart started a second VM on the disk the wedged one still used
- 48e0138 fix(guest-agent): per-container state was never dropped on removal
- 7500ed2 fix(guest-agent): a failed create leaked the container's netns and files
- be2db0e fix(guest-agent): a container name made of hex characters collided with ID prefixes
- 5921c3a fix(make): release without VERSION tagged vdev; disk-compact and update-brew could corrupt state
- 2b3e4ef fix(guest-agent): event recorder spun at 100% CPU after the containerd stream dropped
- 6aa9748 fix(guest-agent): healthcheck lost and stale /wait code after stop/start or restart
- 589189d fix(host): idle pause stalled the daemon for ~40 s and never dropped guest caches
- 238667c fix(host): a crash after resume restored stale VM memory over a newer disk
- 1b4c33f fix(host): PortForwarder.stop() never ran, a stale forwarder kept the ports after a VM crash
- eb7c19d fix: -p 127.0.0.1:port published the port on every host interface

### Changed
- ef4b7dd refactor(guest-agent): split images.go (1586 lines) by concern
- ea03c0f refactor(guest-agent): split containers.go (1460 lines) by concern
- d212988 refactor(guest-agent): remove unused functions and the never-incremented retries map
- f4d9b3b perf(guest-agent): docker ps made about seven containerd calls per container
- 277b05b perf(host): UDP replies waited up to 250 ms for the relay loop to wake
- fabed84 perf(guest-agent): restart monitor resolved every container each second per policy
- 5c44230 perf(guest-agent): port scanner re-fetched every container twice per 500 ms tick

### Docs
- 254b75b docs: drop an internal project name from ARCHITECTURE.md

### Internal
- dbdb38a test: --link probe lost its output to the missing-newline limitation
- bd269c0 build: ANVIL_BUILD_PROXY passes an HTTPS proxy to the initramfs build container
- 5460136 chore(host): silence the Swift 6.x build warnings
- 1cd784c ci: build guest assets through the Makefile instead of a copy of its URLs
- 9462c1d chore(guest-agent): go mod tidy after removing pullImageAllPlatforms
- 41f4f45 build: build_initramfs.sh had no shebang
- 86ee1d0 build: release notes listed feat! twice and missed feat(scope)!
- 7226a31 test: an integration filter that matched no test reported a green run
- 365e683 build: make doctor runs anvil doctor
- bd21e7a ci: run the unit tests, go vet and staticcheck
- 6bc0a32 test(guest-agent): three assertions compared a value with itself
- bb7b263 build: ANVIL_BUILD_CONTEXT selects the docker context for the initramfs build
## v1.1.2 (2026-09-09)

### Fixed
- cb7d672 fix(host): every second daemon stop/start cycle cold-booted and wiped containers
## v1.1.1 (2026-09-09)

### Fixed
- 37052ec fix(host): port forwarder kept dialing a dead container IP after restart
- a2aab42 fix(demo): expand tabs into aligned columns; portable font discovery
- a690a65 fix(demo): typing animation rendered every prefix as its own line

### Docs
- fec9a26 docs: diagram title said 'one unix socket' — vz-runner serves three
- 0f2f5b3 docs: architecture diagram v3 — registry auth, buildkit bridge, route table
- 1ae11e4 docs: refresh anvil benchmark numbers; fix harness JSONL parsing
- 293aeb5 docs: bump demo gif cache-bust to ?v=4
- e7fe7aa docs: new boot-demo.gif — old-style synthetic renderer with current timings
- 1a03d8d docs: re-record boot-demo.gif; docker ps COMMAND filled from the OCI spec

### Other
- 1bbfc10 revert: restore the original boot-demo.gif and README caption
## v1.1.0 (2026-09-03)

### Added
- f2b162f feat(guest-agent): HostConfig spec fields — ulimits/shm/pids/cpuset/swap/group-add/uts/ipc/cgroupns/security-opt per HOSTCONFIG_SPEC; log-driver none discards; wave4 integration tests
- 3431a09 feat(guest-agent): HostConfig rejection layer — 400 naming the flag for oom-kill-disable/blkio-weight/storage-opt/isolation/runtime, log drivers, and non-arm64 platforms
- 10d5663 feat(guest-agent): HostConfig coverage — cpu shares/quota/cpuset, swap, pids, shm, ulimits, no-new-privileges, group-add, ipc/cgroup ns, annotations; snapshot-backed inspect and create warnings
- d34b505 feat(guest-agent): registry authentication (docker login, private pull/push/build) and TTY resize endpoints
- 2768e25 feat: docker events --since replay, doctor --json, generated changelog, Swift unit tests

### Fixed
- f0e9819 fix(guest-agent): error responses were hand-concatenated JSON — quotes in validator messages broke the wire; writeJSONError marshals properly (101 sites)
- 12b0c91 fix(guest-agent): --cpus set a cpuset pin instead of a CFS quota; cpuset now via WithCPUs/WithCPUsMems; hostname skipped under shared UTS
- 2d86af3 fix(guest-agent): logs -f follow diagnostics; runtime artifacts moved under .anvil-run/
- 385c76f fix(host): auto-restart crashed VM with backoff, vsock liveness strikes, snapshot invalidation after 2 failed restarts
- 4eafef5 fix(guest-agent): docker logs -f ended after 30s; ANVIL_CPUS passthrough

### Changed
- 7704b21 refactor(guest-agent): replace the 566-line routing switch with a declarative route table

### Docs
- dcbac72 docs(changelog): drop Unreleased section — covered by the upcoming v1.1.0 release notes
- 21ceeb1 docs: events replay exists — CLAUDE.md and ARCHITECTURE.md still claimed no event log
- d940a7e docs: changelog — events/networks/rename tests
- 9d390f2 docs: changelog — fake containerd tests
- 3694913 docs: changelog — HTTP-layer tests
- b376cf2 docs: changelog — routing refactor
- 733d717 docs(changelog): writeJSONError entry
- a9052d2 docs(changelog): HostConfig spec implementation entries
- 6da1d84 docs: disclose the missing seccomp profile (README limitations, SECURITY.md); HostConfig support matrix
- 05a88f6 docs(changelog): HostConfig coverage entry
- 257f866 docs(changelog): unreleased section
- 8fe51d7 docs: honest Current limitations section in README

### Internal
- 3c5b437 chore: remove CLAUDE.md (AGENTS.md is the single source of agent instructions)
- 7310e07 test(guest-agent): events replay, network lifecycle and rename behind the fake containerd
- bdf1fe9 test(guest-agent): fake containerd gRPC server behind the read-path handlers
- 83d4eb8 test(guest-agent): HTTP-layer tests via httptest over the real route table
- 698f249 chore: LaunchAgent plist as sed template; gofmt gate in unit-tests
## v1.0.55 (2026-08-16)

### Added
- 4ccaf38 feat: rmi across namespaces, guest clock sync, logs --since; test waves

### Docs
- 81b5b79 docs: sync all docs with the current implementation

### Internal
- 88d2703 test: skip buildx tests when host DNS for auth.docker.io is broken

## v1.0.56 (2026-08-18)

### Changed
- 8eeb9c3 perf: build-only binaries move to the persistent disk; cold boot 769->613 ms
- 4a76ab1 perf: cut cold boot 1765->769 ms, compose-up-to-healthy 963->825 ms

### Docs
- 2400e9a docs: update architecture for the post-boot-tail world (agent-side finalize, background DHCP, buildkit tarball on disk)
- 3b0eb6e docs: refresh benchmarks after buildkit-on-disk (cold 613 ms, compose 792 ms, RSS 1139 MB); re-record boot demo timings
- 85794cb docs: refresh benchmark table after boot-perf work (cold 769 ms, RSS best-in-table 1206 MB)
- 3f18819 docs: architecture diagram v2
- e4caf86 docs: rendered architecture diagram in the README
- 411ac85 docs: drop the IMPROVEMENTS.md pointer from the README layout section
- 5d91628 docs: rework the README architecture section
- de01b86 docs: animated boot demo in the README

### Internal
- a62d460 chore: keep post drafts and chart generators local-only
- 34b9089 test: validate_robustness pulls with retries and runs with a containerd disk
- 3963dfa ci: trigger on main instead of test (test branch is gone)
- ad412ba chore: community files, README badges
- cfa1f04 chore: untrack internal docs (AGENTS.md, IMPROVEMENTS.md)

### Other
- a64e748 bench: add Apple Containers backend to the harness

## v1.0.57 (2026-08-24)

### Added
- 129f1d3 feat(guest-agent): serve buildx docker-driver gRPC on the Docker API
- 9db84f5 feat(guest-agent): images and build without nerdctl
- cbb7d08 feat(guest-agent): native networks and volumes
- be463a0 feat(guest-agent): native exec, archive and healthcheck paths
- 5fe15c8 feat(guest-agent): native container lifecycle via containerd client + go-cni
- ef75f6a feat(guest-agent): anvil/* store skeleton

### Fixed
- 38b3541 fix(guest-agent): docker cp out of stopped containers; new suite tests
- fb5529a fix(guest-agent): save-multiple, restart budget, CNI self-heal; new BUGS_AUDIT
- 407d471 fix(guest-agent): cp regression, cgroup/host-net flags, cross-ns GC race
- e39bf01 fix(guest-agent): restart veth race, TTY tasks, late log flush, build prune hang
- a34267b fix(guest-agent): robust docker save across namespaces and platforms
- a137436 fix(guest-agent): cross-namespace image copy and network delete by ID
- 65b6662 fix(guest-agent): live-debug fixes for native runtime

### Changed
- 84be70a refactor: persistent containerd client for all direct client calls

### Docs
- b285c0d docs: purge stale nerdctl references from docs and code comments
- 8ccb1c1 docs: cache-bust the boot demo gif reference (?v=2 — camo served the pre-1.0.56 recording after the file was replaced)

### Internal
- 94ffa2f chore: port host-side harnesses off nerdctl, clean stale comments
- c0bd289 chore: drop nerdctl from initramfs, Makefile and CI

## v1.0.58 (2026-08-24)

### Fixed
- 9fa0544 fix(guest-agent): bring lo up in containers (CNI loopback plugin); doctor check

### Internal
- c24cff9 chore: remove BUGS_AUDIT.md from the repo (keep locally, gitignored)

## v1.0.59 (2026-08-31)

### Fixed
- 7d3ff37 fix(guest-agent): tag resilience against racing GC; index health check
- 6231ad2 fix(guest-agent): cross-container DNS mesh, /run masking, pull verification

