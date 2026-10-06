#!/usr/bin/env bash
# Anvil bench harness: compare cold start / resume / compose-up time
# between runners (vz-runner, Colima, OrbStack, Docker Desktop, Apple
# Containers) on the
# same workload.
#
# Usage:
#   ./run_bench.sh vz-runner colima orbstack
#   ./run_bench.sh all
#
# Each backend is a drivers/<name>.sh that must define:
#   backend_start          - cold start daemon/VM, return when ready for commands
#   backend_stop           - full stop (so the next cold start is honest)
#   backend_resume         - snapshot/resume if supported, else same as backend_start
#   backend_compose_cmd    - print the compose command for this backend
#   backend_idle_rss       - idle RSS of daemon/VM process in MB (after compose down)
#   backend_name           - human-readable name for the report
# Optional:
#   backend_docker_cmd     - print the docker CLI prefix for this backend; enables
#                            the "ops" phase (docker run/stop, compose down)
#
# Results are written to results/<timestamp>.csv and merged into results/latest.md

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DRIVERS_DIR="$SCRIPT_DIR/drivers"
WORKLOAD="$SCRIPT_DIR/workloads/docker-compose.bench.yml"
RESULTS_DIR="$SCRIPT_DIR/results"
mkdir -p "$RESULTS_DIR"
# Drop stale per-run CSVs; aggregate latest.csv/latest.md keep final data.
# Keep latest.csv so other backends are not reset.
for f in "$RESULTS_DIR"/*.csv; do
    [[ -f "$f" ]] || continue
    [[ "$(basename "$f")" == "latest.csv" ]] && continue
    rm -f "$f"
done

TS="$(date +%Y%m%d-%H%M%S)"
CSV="$RESULTS_DIR/$TS.csv"
MD="$RESULTS_DIR/latest.md"

ALL_BACKENDS=(vz-runner lima colima orbstack docker-desktop apple-containers)

if [[ $# -eq 0 ]]; then
    echo "Usage: $0 <backend...> | all"
    echo "Available: ${ALL_BACKENDS[*]}"
    exit 1
fi

if [[ "$1" == "all" ]]; then
    BACKENDS=("${ALL_BACKENDS[@]}")
else
    BACKENDS=("$@")
fi

# --- timer helper: returns milliseconds ---
# bash >= 5 has EPOCHREALTIME (no fork/exec, ~1 ms resolution); the python
# fallback (macOS /bin/bash 3.2) adds a few tens of ms of interpreter startup
# to every interval, which matters for the sub-second ops metrics.
now_ms() {
    if [[ -n "${EPOCHREALTIME:-}" ]]; then
        local us="${EPOCHREALTIME/[.,]/}"
        echo $(( 10#$us / 1000 ))
    else
        python3 -c 'import time; print(int(time.time()*1000))'
    fi
}

# --- median of integer arguments (lower median for an even count) ---
median() {
    printf '%s\n' "$@" | sort -n | sed -n "$(( ($# + 1) / 2 ))p"
}

# --- wait for a command to succeed, with timeout ---
wait_for() {
    local desc="$1"; shift
    local timeout_s="$1"; shift
    local start
    start=$(now_ms)
    until "$@" >/dev/null 2>&1; do
        local elapsed=$(( ($(now_ms) - start) / 1000 ))
        if (( elapsed > timeout_s )); then
            echo "TIMEOUT waiting for: $desc" >&2
            return 1
        fi
        sleep 0.1
    done
}

echo "backend,phase,metric,value_ms" > "$CSV"

record() {
    local backend="$1" phase="$2" metric="$3" value="$4"
    echo "$backend,$phase,$metric,$value" >> "$CSV"
    local unit="ms"
    [[ "$metric" == "idle_rss_mb" ]] && unit="MB"
    printf "  %-12s %-14s %-18s %s%s\n" "$backend" "$phase" "$metric" "$value" "$unit"
}

# Repetitions for the cheap ops metrics (median is recorded).
OPS_REPS="${OPS_REPS:-3}"
OPS_SERVICES=15
OPS_PROJECT="anvil-bench-ops"

# Compose file with $OPS_SERVICES idle alpine services. It is fed to compose
# on stdin (-f -), so the path does not have to be visible inside a VM (lima
# runs the compose CLI in the guest).
write_ops_compose() {
    local file="$1" i
    {
        echo "services:"
        for i in $(seq 1 "$OPS_SERVICES"); do
            echo "  s$i:"
            echo "    image: alpine"
            echo "    command: [\"sleep\", \"300\"]"
        done
    } > "$file"
}

# Phase "ops": everyday CLI latency on the warm backend. Expects the workload
# stack to be up (it is, after the resume phase). A failed command records
# nothing for that metric instead of aborting the whole run.
run_ops_phase() {
    local backend="$1" compose_cmd="$2"
    if ! declare -f backend_docker_cmd >/dev/null; then
        echo "  (ops phase skipped: '$backend' has no docker CLI endpoint)"
        return 0
    fi
    local docker_cmd t0 t1 i
    docker_cmd="$(backend_docker_cmd)"

    # compose down of the benchmark stack (measured once: re-creating the
    # stack to healthy is not cheap).
    t0=$(now_ms)
    if $compose_cmd -f "$WORKLOAD" down -t 0; then
        t1=$(now_ms)
        record "$backend" "ops" "compose_down" $((t1 - t0))
    else
        echo "!! '$backend' ops: compose down failed"
    fi

    # Warm-up (unmeasured): make sure alpine is present so no metric
    # includes a registry pull.
    if ! $docker_cmd run --rm alpine true >/dev/null 2>&1; then
        echo "!! '$backend' ops: 'docker run alpine' failed, skipping the rest of ops"
        return 0
    fi

    # docker run --rm alpine true
    local samples=()
    for i in $(seq 1 "$OPS_REPS"); do
        t0=$(now_ms)
        $docker_cmd run --rm alpine true >/dev/null 2>&1 || { samples=(); break; }
        t1=$(now_ms)
        samples+=($((t1 - t0)))
    done
    if (( ${#samples[@]} > 0 )); then
        record "$backend" "ops" "run_rm" "$(median "${samples[@]}")"
    else
        echo "!! '$backend' ops: docker run --rm failed"
    fi

    # docker stop -t 0 on a running container (create/remove unmeasured)
    local name="anvil-bench-ops-sleep"
    samples=()
    for i in $(seq 1 "$OPS_REPS"); do
        $docker_cmd rm -f "$name" >/dev/null 2>&1 || true
        if ! $docker_cmd run -d --name "$name" alpine sleep 300 >/dev/null 2>&1; then
            samples=(); break
        fi
        t0=$(now_ms)
        $docker_cmd stop -t 0 "$name" >/dev/null 2>&1 || { samples=(); break; }
        t1=$(now_ms)
        samples+=($((t1 - t0)))
    done
    $docker_cmd rm -f "$name" >/dev/null 2>&1 || true
    if (( ${#samples[@]} > 0 )); then
        record "$backend" "ops" "stop_t0" "$(median "${samples[@]}")"
    else
        echo "!! '$backend' ops: docker stop -t 0 failed"
    fi

    # compose up -d / down -t 0 of $OPS_SERVICES idle services
    local tmpdir file ups=() downs=()
    tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/anvil-bench-ops.XXXXXX")"
    file="$tmpdir/docker-compose.ops.yml"
    write_ops_compose "$file"
    local ops_compose="$docker_cmd compose -p $OPS_PROJECT -f -"
    $ops_compose down -t 0 < "$file" >/dev/null 2>&1 || true
    for i in $(seq 1 "$OPS_REPS"); do
        t0=$(now_ms)
        if ! $ops_compose up -d < "$file" >/dev/null 2>&1; then
            ups=(); downs=(); break
        fi
        t1=$(now_ms)
        ups+=($((t1 - t0)))
        t0=$(now_ms)
        if ! $ops_compose down -t 0 < "$file" >/dev/null 2>&1; then
            downs=(); break
        fi
        t1=$(now_ms)
        downs+=($((t1 - t0)))
    done
    $ops_compose down -t 0 < "$file" >/dev/null 2>&1 || true
    rm -rf "$tmpdir"
    if (( ${#ups[@]} > 0 )); then
        record "$backend" "ops" "compose${OPS_SERVICES}_up" "$(median "${ups[@]}")"
    else
        echo "!! '$backend' ops: compose up of $OPS_SERVICES services failed"
    fi
    if (( ${#downs[@]} > 0 )); then
        record "$backend" "ops" "compose${OPS_SERVICES}_down" "$(median "${downs[@]}")"
    else
        echo "!! '$backend' ops: compose down of $OPS_SERVICES services failed"
    fi
}

run_one_backend() {
    local backend="$1"
    local driver="$DRIVERS_DIR/$backend.sh"

    if [[ ! -f "$driver" ]]; then
        echo "!! no driver for '$backend' at $driver, skipping"
        return
    fi

    echo "=== $backend ==="
    # Drivers are sourced into the same shell one after another: drop the
    # optional hooks of the previous driver so they do not leak into this one.
    unset -f backend_is_available backend_cold_reset backend_docker_cmd
    # shellcheck disable=SC1090
    source "$driver"

    # Skip backends that declare they are not available (e.g. app not installed
    # or VM not running). Keeps `make harness-all` usable on machines that have
    # only a subset of competitors installed.
    if declare -f backend_is_available >/dev/null && ! backend_is_available; then
        echo "!! backend '$backend' is not available, skipping"
        return
    fi

    # --- 1. Full stop first so the cold start is honest ---
    backend_stop || true
    sleep 2
    # Give drivers a chance to invalidate any saved state (e.g. vz-runner's
    # snapshot) so "cold start" really is a cold boot, not a resume.
    if declare -f backend_cold_reset >/dev/null; then
        backend_cold_reset
    fi

    # --- 2. Cold start ---
    local t0 t1
    t0=$(now_ms)
    if ! backend_start; then
        echo "!! backend '$backend' failed to start, skipping"
        return
    fi
    t1=$(now_ms)
    record "$backend" "cold_start" "daemon_ready" $((t1 - t0))

    # --- 3. Compose up (same workload on all backends) ---
    local compose_cmd
    compose_cmd="$(backend_compose_cmd)"
    t0=$(now_ms)
    if ! $compose_cmd -f "$WORKLOAD" up -d; then
        echo "!! backend '$backend' compose up failed, skipping"
        backend_stop || true
        return
    fi
    wait_for "all services healthy" 60 backend_all_healthy
    t1=$(now_ms)
    record "$backend" "cold_start" "compose_up_healthy" $((t1 - t0))

    # --- 4. Idle RSS after services are up ---
    local rss
    rss="$(backend_idle_rss)"
    record "$backend" "steady_state" "idle_rss_mb" "$rss"

    # --- 5. Compose down, then snapshot/stop for resume test ---
    $compose_cmd -f "$WORKLOAD" down
    backend_stop_keep_snapshot || backend_stop

    # --- 6. Resume (if snapshot is unsupported this is a second cold start) ---
    t0=$(now_ms)
    if ! backend_resume; then
        echo "!! backend '$backend' resume failed, skipping"
        backend_stop || true
        return
    fi
    t1=$(now_ms)
    record "$backend" "resume" "daemon_ready" $((t1 - t0))

    # --- 7. Compose up again on the warm backend ---
    t0=$(now_ms)
    if ! $compose_cmd -f "$WORKLOAD" up -d; then
        echo "!! backend '$backend' resume compose up failed, skipping"
        backend_stop || true
        return
    fi
    wait_for "all services healthy" 60 backend_all_healthy
    t1=$(now_ms)
    record "$backend" "resume" "compose_up_healthy" $((t1 - t0))

    # --- 8. Ops on the warm backend (takes the workload stack down) ---
    run_ops_phase "$backend" "$compose_cmd"

    # --- cleanup ---
    $compose_cmd -f "$WORKLOAD" down -v
    backend_stop || true
}

for b in "${BACKENDS[@]}"; do
    run_one_backend "$b"
done

echo
echo "Raw results: $CSV"

python3 "$SCRIPT_DIR/report.py" "$CSV"
# Aggregate latest.csv updated, latest.md regenerated.
# Per-run CSVs are no longer needed; remove them.
rm -f "$CSV"
cat "$MD"
