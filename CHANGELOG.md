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

