#!/usr/bin/env bash
# Anvil bench harness: compare cold start / resume / compose-up time
# between runners (vz-runner, Colima, OrbStack, Docker Desktop) on the
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
#   backend_compose_cmd    - print the docker/nerdctl compose command
#   backend_idle_rss       - idle RSS of daemon/VM process in MB (after compose down)
#   backend_name           - human-readable name for the report
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

ALL_BACKENDS=(vz-runner lima colima orbstack docker-desktop)

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
now_ms() {
    python3 -c 'import time; print(int(time.time()*1000))'
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

run_one_backend() {
    local backend="$1"
    local driver="$DRIVERS_DIR/$backend.sh"

    if [[ ! -f "$driver" ]]; then
        echo "!! no driver for '$backend' at $driver, skipping"
        return
    fi

    echo "=== $backend ==="
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
