#!/usr/bin/env bash
# Драйвер для vz-runner. Требует бинарник vz-runner в PATH (или VZRUNNER_BIN).
#
# Текущий CLI vz-runner:
#   vz-runner daemon [--share <path>] [--idle <seconds>]
#   vz-runner status
#   vz-runner exec <cmd>...
#
# Harness запускает демон в фоне, ждёт status, останавливает через SIGTERM
# (daemon сам делает pause + save snapshot в shutdown handler).

VZRUNNER_BIN="${VZRUNNER_BIN:-vz-runner}"
# Корень bench-harness расшариваем в VM через virtiofs на /mnt/anvil
HOST_SHARE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GUEST_SHARE_ROOT="/mnt/anvil"
# Пути к kernel/initrd относительно корня проекта (этот драйвер лежит в bench-harness/drivers).
VZRUNNER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KERNEL_PATH="$VZRUNNER_DIR/.download/ubuntu/vmlinuz-raw"
INITRD_PATH="$VZRUNNER_DIR/.download/ubuntu/initramfs-containerd"
# Persistent block disk for /var/lib/containerd.
CONTAINERD_DISK="${CONTAINERD_DISK:-$HOME/.anvil-vz/containerd-disk.img}"

backend_name() { echo "vz-runner"; }

# --- pid файла нет; запоминаем pid демона ourselves ---
VZ_PID_FILE="${TMPDIR:-/tmp}/vz-runner-bench.pid"
VZ_VM_PID_FILE="${VZ_PID_FILE}.vm"

backend_start() {
    rm -f "$VZ_PID_FILE" "$VZ_VM_PID_FILE"
    # Запускаем демон, detached от терминала, но с сохранением pid.
    # 2 GB is enough because images and snapshots live on the persistent
    # containerd block disk, not in guest RAM/tmpfs.
    nohup "$VZRUNNER_BIN" daemon \
        --kernel "$KERNEL_PATH" \
        --initrd "$INITRD_PATH" \
        --share "$HOST_SHARE_ROOT" \
        --memory 2 \
        --idle 300 \
        --containerd-disk "$CONTAINERD_DISK" \
        >/tmp/vz-runner-bench.log 2>&1 &
    echo $! > "$VZ_PID_FILE"
    wait_for "vz-runner daemon ready" 60 "$VZRUNNER_BIN" status
    # Capture the VirtualMachine process spawned by this daemon. There may be
    # leftover orphan VM processes from previous runs; pick the newest one.
    for _ in $(seq 1 50); do
        local vm_pid
        vm_pid="$(pgrep -f "com.apple.Virtualization.VirtualMachine" | tail -1)"
        if [[ -n "$vm_pid" ]]; then
            echo "$vm_pid" > "$VZ_VM_PID_FILE"
            break
        fi
        sleep 0.1
    done
}

backend_stop() {
    if [[ -f "$VZ_PID_FILE" ]]; then
        local pid
        pid="$(cat "$VZ_PID_FILE")"
        kill -TERM "$pid" 2>/dev/null || true
        # Graceful shutdown includes VM snapshot save + containerd cache sync;
        # on a loaded VM this can take 30-40 s, so wait up to 90 s.
        for _ in $(seq 1 300); do
            kill -0 "$pid" 2>/dev/null || break
            sleep 0.3
        done
        rm -f "$VZ_PID_FILE" "$VZ_VM_PID_FILE"
    fi
    # Fallback: убиваем любой оставшийся vz-runner daemon от этого harness'а
    pkill -f 'vz-runner daemon' || true
}

# vz-runner daemon сам сохраняет snapshot при SIGTERM, так что keep snapshot
# — это просто graceful stop.
backend_stop_keep_snapshot() {
    backend_stop
}

backend_resume() {
    # При повторном запуске daemon восстановится из snapshot, если он валиден.
    backend_start
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
data = sys.stdin.read().strip()
if not data:
    print(999)
    sys.exit(0)
try:
    parsed = json.loads(data)
except json.JSONDecodeError:
    print(999)
    sys.exit(0)
items = parsed if isinstance(parsed, list) else [parsed]
for obj in items:
    if not isinstance(obj, dict):
        continue
    if obj.get("State") != "running":
        n += 1
    elif obj.get("Health") not in ("healthy", ""):
        n += 1
print(n)
' 2>/dev/null || echo 999)
    [[ "$unhealthy" == "0" ]]
}

backend_idle_rss() {
    # Сумма RSS vz-runner daemon + VM процесса, запущенного этим harness'ом.
    # Гостевые процессы внутри VM не считаем — их RSS несравним напрямую
    # с host-side процессами других backend'ов.
    local total_rss=0
    local pid
    if [[ -f "$VZ_PID_FILE" ]]; then
        pid="$(cat "$VZ_PID_FILE" 2>/dev/null)"
        if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
            total_rss=$((total_rss + $(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ')))
        fi
    fi
    if [[ -f "$VZ_VM_PID_FILE" ]]; then
        pid="$(cat "$VZ_VM_PID_FILE" 2>/dev/null)"
        if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
            total_rss=$((total_rss + $(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ')))
        fi
    fi
    echo $((total_rss / 1024))
}
