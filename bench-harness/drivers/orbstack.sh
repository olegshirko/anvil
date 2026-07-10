#!/usr/bin/env bash
# Драйвер для OrbStack. Требует установленный OrbStack.app и CLI `orb`.

backend_name() { echo "OrbStack"; }

backend_start() {
    open -a OrbStack
    wait_for "orbstack docker ready" 60 docker info
}

backend_stop() {
    orbctl stop >/dev/null 2>&1 || osascript -e 'quit app "OrbStack"' 2>/dev/null || true
}

# OrbStack держит машину "тёплой" в фоне и не имеет публичного snapshot
# API — для честного сравнения считаем resume как повторный старт после
# quit, это максимально близко к тому, что реально доступно пользователю.
backend_stop_keep_snapshot() {
    return 1
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
    ps aux | grep -iE 'orbstack|OrbStack' | grep -v grep \
        | awk '{sum+=$6} END {printf "%.0f", sum/1024}'
}
