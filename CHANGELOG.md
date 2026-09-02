## Unreleased

### Added
- f2b162f feat(guest-agent): HostConfig spec fields — ulimits/shm/pids/cpuset/swap/group-add/uts/ipc/cgroupns/security-opt per HOSTCONFIG_SPEC; log-driver none discards; wave4 integration tests
- 6da1d84 feat(guest-agent): HostConfig rejection layer — 400 naming the flag for oom-kill-disable/blkio-weight/storage-opt/isolation/runtime, log drivers, and non-arm64 platforms
- 10d5663 feat(guest-agent): HostConfig coverage — cpu shares/quota/cpuset, swap, pids, shm, ulimits, no-new-privileges, group-add, ipc/cgroup ns, annotations; snapshot-backed inspect and create warnings
- d34b505 feat(guest-agent): registry authentication (docker login, private pull/push/build) and TTY resize endpoints
- 2768e25 feat: docker events --since replay, doctor --json, generated changelog, Swift unit tests

- f0e9819 fix(guest-agent): error responses were hand-concatenated JSON — quotes in validator messages broke the wire; writeJSONError marshals properly (101 sites)
### Internal
- 7704b21 refactor(guest-agent): the 566-line routing switch replaced with a declarative route table (router.go) and per-domain handler files api_{system,containers,images,networks,volumes}.go; matcher unit tests; events recorder debug instrumentation

### Fixed
- 12b0c91 fix(guest-agent): --cpus set a cpuset pin instead of a CFS quota; cpuset now via WithCPUs/WithCPUsMems; hostname skipped under shared UTS
- 2d86af3 fix(guest-agent): logs -f follow diagnostics; runtime artifacts moved under .anvil-run/
- 385c76f fix(host): auto-restart crashed VM with backoff, vsock liveness strikes, snapshot invalidation after 2 failed restarts
- 4eafef5 fix(guest-agent): docker logs -f ended after 30s; ANVIL_CPUS passthrough

### Docs
- 6da1d84 docs: disclose the missing seccomp profile (README limitations, SECURITY.md); HostConfig support matrix
- 8fe51d7 docs: honest Current limitations section in README

### Internal
- 698f249 chore: LaunchAgent plist as sed template; gofmt gate in unit-tests

# Changelog

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

