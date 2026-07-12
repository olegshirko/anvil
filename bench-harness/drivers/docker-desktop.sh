#!/usr/bin/env bash
# Драйвер для Docker Desktop. Требует установленный Docker Desktop.app.

backend_name() { echo "Docker Desktop"; }

backend_is_available() {
    [[ -d "/Applications/Docker.app" ]] || \
        mdfind "kMDItemCFBundleIdentifier == 'com.docker.docker'" 2>/dev/null | grep -q .
}

backend_start() {
    open -a Docker
    # `docker info` без сервера возвращает 0 и показывает только Client.
    # Ждём именно ServerVersion — он появляется только когда daemon готов.
    wait_for "docker desktop ready" 180 docker --context desktop-linux info --format '{{.ServerVersion}}'
}

backend_stop() {
    osascript -e 'quit app "Docker"' 2>/dev/null || true
    sleep 1
}

backend_stop_keep_snapshot() {
    return 1  # Docker Desktop не имеет snapshot/resume
}

backend_resume() {
    backend_start
}

backend_compose_cmd() {
    echo "docker --context desktop-linux compose"
}

backend_all_healthy() {
    local unhealthy
    unhealthy=$(docker --context desktop-linux compose -f "$WORKLOAD" ps --format json 2>/dev/null \
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
    ps aux | grep -iE 'Docker Desktop|com.docker' | grep -v grep \
        | awk '{sum+=$6} END {printf "%.0f", sum/1024}'
}
