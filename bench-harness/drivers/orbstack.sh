#!/usr/bin/env bash
# Драйвер для OrbStack. Требует установленный OrbStack.app.

backend_name() { echo "OrbStack"; }

_ORBSTACK_APP="/Applications/OrbStack.app"
ORBSTACK_SOCK="${ORBSTACK_SOCK:-$HOME/.orbstack/run/docker.sock}"

_backend_orbstack_docker_config() {
    if [[ -z "${ORBSTACK_DOCKER_CONFIG:-}" ]]; then
        local tmp
        tmp="$(mktemp -d)"
        mkdir -p "$tmp"
        # OrbStack при старте перезаписывает ~/.docker/config.json и выставляет
        # credsStore=osxkeychain, которого нет без Docker Desktop.
        # Используем временный config без credential helpers.
        python3 -c "
import json, os, shutil
src = os.path.expanduser('~/.docker/config.json')
try:
    with open(src) as f:
        cfg = json.load(f)
except (FileNotFoundError, json.JSONDecodeError):
    cfg = {}
cfg.pop('credsStore', None)
cfg.pop('credHelpers', None)
with open('$tmp/config.json', 'w') as f:
    json.dump(cfg, f)
# Docker Compose plugin иногда обращается к contexts даже при использовании
# DOCKER_HOST, поэтому копируем их во временный config dir.
ctx_src = os.path.expanduser('~/.docker/contexts')
if os.path.isdir(ctx_src):
    shutil.copytree(ctx_src, os.path.join('$tmp', 'contexts'))
" 2>/dev/null || echo '{}' > "$tmp/config.json"
        ORBSTACK_DOCKER_CONFIG="$tmp"
    fi
    echo "$ORBSTACK_DOCKER_CONFIG"
}

backend_is_available() {
    # OrbStack.app must be installed; CLI `orb`/`orbctl` may be missing from PATH.
    [[ -d "$_ORBSTACK_APP" ]] || \
        mdfind "kMDItemCFBundleIdentifier == 'com.orbstack.orbstack'" 2>/dev/null | grep -q .
}

backend_start() {
    export DOCKER_CONFIG="$(_backend_orbstack_docker_config)"
    export DOCKER_HOST="unix://$ORBSTACK_SOCK"
    open -a OrbStack
    # `docker info` без сервера возвращает 0 и показывает только Client.
    # Ждём именно ServerVersion — он появляется только когда daemon готов.
    wait_for "orbstack docker ready" 180 docker info --format '{{.ServerVersion}}'
}

backend_stop() {
    export DOCKER_CONFIG="$(_backend_orbstack_docker_config)"
    export DOCKER_HOST="unix://$ORBSTACK_SOCK"
    osascript -e 'quit app "OrbStack"' 2>/dev/null || true
    sleep 1
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
    export DOCKER_CONFIG="$(_backend_orbstack_docker_config)"
    export DOCKER_HOST="unix://$ORBSTACK_SOCK"
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
