#!/usr/bin/env bash
# Драйвер для vz-runner. Требует бинарник vz-runner в PATH и
# скопированный snapshot из предыдущего --fresh запуска для resume-теста.

VZRUNNER_BIN="${VZRUNNER_BIN:-vz-runner}"
SNAPSHOT_PATH="${SNAPSHOT_PATH:-$HOME/.anvil/vz-runner/snapshot}"
GUEST_SHARE_ROOT="/mnt/anvil"

backend_name() { echo "vz-runner"; }

backend_start() {
    "$VZRUNNER_BIN" boot --fresh --daemon >/dev/null 2>&1
    wait_for "vz-runner guest-agent ready" 30 "$VZRUNNER_BIN" status
}

backend_stop() {
    "$VZRUNNER_BIN" stop >/dev/null 2>&1 || true
}

# Ключевое отличие vz-runner: перед stop делаем save, чтобы следующий
# backend_resume мог реально резюмировать состояние, а не грузиться с нуля.
backend_stop_keep_snapshot() {
    "$VZRUNNER_BIN" save >/dev/null 2>&1
    "$VZRUNNER_BIN" stop >/dev/null 2>&1
    return 0
}

backend_resume() {
    "$VZRUNNER_BIN" boot --resume --daemon >/dev/null 2>&1
    wait_for "vz-runner guest-agent ready (resume)" 30 "$VZRUNNER_BIN" status
}

backend_compose_cmd() {
    echo "$SCRIPT_DIR/scripts/vzc.sh"
}

backend_all_healthy() {
    local unhealthy
    unhealthy=$("$VZRUNNER_BIN" exec nerdctl -n bench compose -f "$GUEST_SHARE_ROOT/workloads/docker-compose.bench.yml" ps --format json 2>/dev/null \
        | python3 -c '
import sys, json
n = 0
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        obj = json.loads(line)
    except json.JSONDecodeError:
        continue
    if obj.get("Health") not in ("healthy", ""):
        n += 1
print(n)
' 2>/dev/null || echo 999)
    [[ "$unhealthy" == "0" ]]
}

backend_idle_rss() {
    # Сам vz-runner процесс на хосте (не гостевые процессы внутри VM -
    # их RSS не сравним напрямую с host-side процессами других backend'ов)
    ps aux | grep -i "$VZRUNNER_BIN" | grep -v grep \
        | awk '{sum+=$6} END {printf "%.0f", sum/1024}'
}
