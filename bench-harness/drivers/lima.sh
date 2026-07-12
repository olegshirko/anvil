#!/usr/bin/env bash
# Драйвер для Lima (instance "anvil"). Требует: brew install lima docker docker-compose

LIMA_INSTANCE="${LIMA_INSTANCE:-anvil}"

backend_name() { echo "Lima ($LIMA_INSTANCE)"; }

backend_is_available() {
    # The VM instance must exist; harness itself will start/stop it.
    command -v limactl >/dev/null 2>&1 && \
        limactl list "$LIMA_INSTANCE" --format '{{.Name}}' 2>/dev/null | grep -qx "$LIMA_INSTANCE"
}

# Внутренняя обёртка для docker-команд внутри Lima VM
_lima_docker() {
    limactl shell "$LIMA_INSTANCE" docker "$@"
}

backend_start() {
    limactl start "$LIMA_INSTANCE" >/dev/null 2>&1 || true
    wait_for "lima docker ready" 120 _lima_docker info
}

backend_stop() {
    limactl stop "$LIMA_INSTANCE" >/dev/null 2>&1 || true
}

# Lima не предоставляет API для мгновенного snapshot/resume состояния VM.
# "Resume" в терминах harness — это обычный warm start после stop.
backend_stop_keep_snapshot() {
    return 1
}

backend_resume() {
    backend_start
}

backend_compose_cmd() {
    echo "limactl shell $LIMA_INSTANCE docker compose"
}

backend_all_healthy() {
    local unhealthy
    unhealthy=$(_lima_docker compose -f "$WORKLOAD" ps --format json 2>/dev/null \
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
    # Сумма RSS процессов Lima + VM backend (vz или qemu) на хосте.
    ps aux | grep -E 'limactl|Virtualization\.VirtualMachine|qemu-system' | grep -v grep \
        | awk '{sum+=$6} END {printf "%.0f", sum/1024}'
}
