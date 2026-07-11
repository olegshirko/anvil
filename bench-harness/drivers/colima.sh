#!/usr/bin/env bash
# Драйвер для Colima. Требует: brew install colima docker docker-compose

backend_name() { echo "Colima"; }

backend_start() {
    colima start --vm-type=vz --mount-type=virtiofs >/dev/null 2>&1
    wait_for "colima docker ready" 60 docker info
}

backend_stop() {
    colima stop >/dev/null 2>&1 || true
}

# Colima не умеет VM-snapshot/resume в смысле мгновенного восстановления
# состояния — "resume" тут это обычный start после stop (диск тот же,
# но не suspended-state). Это честно отражает реальную разницу с vz-runner.
backend_stop_keep_snapshot() {
    return 1  # сигнализируем harness'у, что снапшота нет, делать обычный stop
}

backend_resume() {
    backend_start
}

backend_compose_cmd() {
    echo "docker compose"
}

backend_all_healthy() {
    local unhealthy
    unhealthy=$(docker compose -f "$WORKLOAD" ps --format json 2>/dev/null \
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
')
    [[ "$unhealthy" == "0" ]]
}

backend_idle_rss() {
    # Сумма RSS всех colima/qemu/vz-related процессов на хосте
    ps aux | grep -E 'colima|qemu-system|vz' | grep -v grep \
        | awk '{sum+=$6} END {printf "%.0f", sum/1024}'
}
