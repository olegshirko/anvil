# Anvil bench harness

Compares cold start / resume / compose-up time between vz-runner and
competitors on the same workload.

## Setup

```bash
chmod +x run_bench.sh scripts/*.sh drivers/*.sh

# 1. Make sure vz-runner binary is in PATH, or:
export VZRUNNER_BIN=/path/to/.build/release/vz-runner

# 2. Pull workload images ahead of time on each backend you plan to measure,
#    otherwise the benchmark measures network speed instead of the runtime.
./scripts/prepull.sh docker        # current docker context (Colima, OrbStack, Docker Desktop)
./scripts/prepull.sh lima          # Lima VM instance "anvil"
./scripts/prepull.sh vz-runner     # inside vz-runner VM (namespace bench)
./scripts/prepull.sh apple-containers  # Apple Containers image store (macOS 26+)

# 3. Make sure vz-runner shares the root of this folder into the VM at
#    /mnt/anvil via virtiofs (see M2); otherwise vzc.sh will not find the
#    workload file inside the VM.
```

## Run

```bash
./run_bench.sh vz-runner lima colima orbstack docker-desktop apple-containers
# or
./run_bench.sh all
# or only your runner if competitors are not installed
./run_bench.sh vz-runner
# or compare with the previous Lima-based Anvil
./run_bench.sh vz-runner lima
```

Each backend is run in isolation: full stop → cold start → compose up (first
time) → idle RSS → compose down → snapshot/stop → resume → compose up (second
time) → cleanup.

## What is measured

| Phase | Metric | Meaning |
|---|---|---|
| cold_start | daemon_ready | time from zero to accepting commands |
| cold_start | compose_up_healthy | plus time to bring the whole stack (db+cache+api+web) to healthy |
| resume | daemon_ready | same after snapshot/resume (if supported), otherwise a second cold start |
| resume | compose_up_healthy | compose up on the already warm backend |
| steady_state | idle_rss_mb | host daemon/VM process memory at idle |

Backends without a snapshot/resume API (Colima, OrbStack, Docker Desktop,
Apple Containers today) run an honest second cold start in the "resume"
phase. This makes the difference visible instead of hiding it.

The apple-containers backend (Apple's `container` CLI, macOS 26+) has no
compose: scripts/apple-compose runs the same four services as plain
`container run` calls. Setup:

```bash
# 1. Install the CLI from https://github.com/apple/container/releases
#    (double-click the signed installer pkg), or:
sudo installer -pkg container-*-installer-signed.pkg -target /
# 2. One-time: install the recommended Linux kernel
container system kernel set --recommended
```

A user-space extraction (payload unpacked outside /usr/local) also works:
point CONTAINER_BIN at that `container` binary when running the harness.

## Output

Results are stored in two files:

- `results/latest.csv` — aggregate data (last measurement for each backend ×
  phase × metric).
- `results/latest.md` — a single summary table generated from `latest.csv`,
  with the best value in each row highlighted.

Each new run updates only the backends it includes; results for other backends
are preserved. This lets you remeasure a single competitor without losing
existing data:

```bash
# Remeasure only Docker Desktop, keeping all other results
./run_bench.sh docker-desktop
# or via Makefile
make harness BENCH_BACKENDS=docker-desktop
```

Per-run CSV files are deleted after `latest.csv` is updated so `results/`
does not grow.

## Add a new backend

Copy `drivers/colima.sh` as a template, implement the required functions
(`backend_name`, `backend_start`, `backend_stop`, `backend_stop_keep_snapshot`,
`backend_resume`, `backend_compose_cmd`, `backend_all_healthy`,
`backend_idle_rss`), and add the name to `ALL_BACKENDS` in `run_bench.sh`.
