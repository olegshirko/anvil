#!/usr/bin/env bash
# Anvil bench harness: сравнивает cold start / resume / compose-up
# время между разными раннерами (vz-runner, Colima, OrbStack, Docker
# Desktop) на одинаковом workload.
#
# Использование:
#   ./run_bench.sh vz-runner colima orbstack
#   ./run_bench.sh all
#
# Каждый backend — это drivers/<name>.sh, который должен определить:
#   backend_start        - холодный старт демона/VM, вернуть когда готов принимать docker/nerdctl команды
#   backend_stop          - полная остановка (для cold start теста следующего прогона)
#   backend_resume         - если backend поддерживает snapshot/resume; иначе делает то же что backend_start
#   backend_compose_cmd     - печатает команду для docker/nerdctl compose (может отличаться: "docker compose" vs "nerdctl compose")
#   backend_idle_rss        - RSS в МБ демона/VM процесса в состоянии idle (после compose down)
#   backend_name             - человекочитаемое имя для отчёта
#
# Результаты пишутся в results/<timestamp>.csv и в results/latest.md

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DRIVERS_DIR="$SCRIPT_DIR/drivers"
WORKLOAD="$SCRIPT_DIR/workloads/docker-compose.bench.yml"
RESULTS_DIR="$SCRIPT_DIR/results"
mkdir -p "$RESULTS_DIR"
# Старые сырые CSV от предыдущих (прерванных) запусков больше не нужны —
# aggregate results/latest.csv и latest.md уже содержат итоговые данные.
# latest.csv не трогаем, чтобы не сбросить результаты других backend'ов.
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

# --- таймер helper: возвращает миллисекунды ---
now_ms() {
    python3 -c 'import time; print(int(time.time()*1000))'
}

# --- ждать, пока команда не вернёт 0, с таймаутом ---
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

    # --- 1. Полная остановка перед тестом, чтобы cold start был честным ---
    backend_stop || true
    sleep 2

    # --- 2. Cold start ---
    local t0 t1
    t0=$(now_ms)
    if ! backend_start; then
        echo "!! backend '$backend' failed to start, skipping"
        return
    fi
    t1=$(now_ms)
    record "$backend" "cold_start" "daemon_ready" $((t1 - t0))

    # --- 3. Compose up (одинаковый workload на всех) ---
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

    # --- 4. Idle RSS после того как всё поднято ---
    local rss
    rss="$(backend_idle_rss)"
    record "$backend" "steady_state" "idle_rss_mb" "$rss"

    # --- 5. Compose down, потом snapshot/stop для resume-теста ---
    $compose_cmd -f "$WORKLOAD" down
    backend_stop_keep_snapshot || backend_stop

    # --- 6. Resume (если backend не поддерживает snapshot, это будет
    #     просто второй cold start — driver сам это учитывает) ---
    t0=$(now_ms)
    if ! backend_resume; then
        echo "!! backend '$backend' resume failed, skipping"
        backend_stop || true
        return
    fi
    t1=$(now_ms)
    record "$backend" "resume" "daemon_ready" $((t1 - t0))

    # --- 7. Compose up повторно на уже тёплом backend ---
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
# Aggregate latest.csv обновлён, latest.md перегенерирован.
# Сырые CSV-файлы отдельных прогонов больше не нужны — удаляем.
rm -f "$CSV"
cat "$MD"
