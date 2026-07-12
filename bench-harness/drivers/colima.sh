#!/usr/bin/env bash
# Драйвер для Colima. Требует: brew install colima docker docker-compose

backend_name() { echo "Colima"; }

# Colima docker socket + изолированный docker config без desktop credential helper,
# чтобы `docker pull` не ломался на отсутствии docker-credential-desktop.
COLIMA_SOCK="${COLIMA_SOCK:-$HOME/.colima/default/docker.sock}"

_backend_colima_docker_config() {
    if [[ -z "${COLIMA_DOCKER_CONFIG:-}" ]]; then
        local tmp
        tmp="$(mktemp -d)"
        mkdir -p "$tmp"
        # Копируем хостовый config, но убираем desktop credential helper —
        # иначе `docker pull` пытается вызвать docker-credential-desktop, которого нет в PATH.
        python3 -c "
import json, os
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
" 2>/dev/null || echo '{}' > "$tmp/config.json"
        COLIMA_DOCKER_CONFIG="$tmp"
    fi
    echo "$COLIMA_DOCKER_CONFIG"
}

backend_is_available() {
    command -v colima >/dev/null 2>&1
}

_backend_colima_dns_ready() {
    # DNS-форвардер Colima (192.168.5.1) иногда готов позже, чем docker daemon.
    # Проверяем, что изнутри VM резолвится auth.docker.io.
    colima ssh -- sh -c 'curl -sS -o /dev/null --connect-timeout 5 "https://auth.docker.io/token?scope=repository%3Alibrary%2Fnginx%3Apull&service=registry.docker.io"' >/dev/null 2>&1
}

backend_start() {
    export DOCKER_CONFIG="$(_backend_colima_docker_config)"
    export DOCKER_HOST="unix://$COLIMA_SOCK"
    colima start --vm-type=vz --mount-type=virtiofs >/dev/null 2>&1
    # Colima старт может занять 30-60 с на Apple Silicon.
    # `docker info` без сервера всё равно возвращает 0, поэтому ждём именно ServerVersion.
    wait_for "colima docker ready" 180 docker info --format '{{.ServerVersion}}'
    # Дополнительно ждём, пока заработает DNS-форвардер внутри VM —
    # без этого `compose up` падает на timeout auth.docker.io.
    wait_for "colima DNS ready" 60 _backend_colima_dns_ready
}

backend_stop() {
    export DOCKER_CONFIG="$(_backend_colima_docker_config)"
    export DOCKER_HOST="unix://$COLIMA_SOCK"
    colima stop >/dev/null 2>&1 || true
}

# Colima не умеет VM-snapshot/resume в смысле мгновенного восстановления
# состояния — "resume" тут это обычный start после stop (диск тот же,
# но не suspended-state). Это честно отражает реальную разницу с vz-runner.
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
    export DOCKER_CONFIG="$(_backend_colima_docker_config)"
    export DOCKER_HOST="unix://$COLIMA_SOCK"
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
    # Сумма RSS всех colima/limactl/Virtualization-related процессов на хосте.
    # Colima на Apple Silicon использует limactl hostagent + usernet + VM-процесс
    # com.apple.Virtualization.VirtualMachine.
    ps aux | grep -E 'colima|limactl|qemu-system|Virtualization' | grep -v grep \
        | awk '{sum+=$6} END {printf "%.0f", sum/1024}'
}
