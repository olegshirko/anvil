#!/usr/bin/env bash
# Драйвер для vz-runner, использующий host `docker` CLI через anvil context.
# Отличается от vz-runner.sh только командой compose: вместо `vzc.sh`
# (nerdctl compose внутри VM) здесь `docker compose`, который ходит в
# guest-agent через Docker API proxy (/Users/oleg/.anvil-vz/docker.sock).
#
# Требования: активный docker context `anvil` (default) и запущенный/запускаемый
# vz-runner daemon.

VZRUNNER_BIN="${VZRUNNER_BIN:-vz-runner}"
# Корень bench-harness расшариваем в VM через virtiofs на /mnt/anvil.
# Для docker compose host-путь workload передаётся как есть, но share нужен
# для синхронизации containerd cache между cold boot'ами.
HOST_SHARE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Пути к kernel/initrd относительно корня проекта.
VZRUNNER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KERNEL_PATH="$VZRUNNER_DIR/.download/ubuntu/vmlinuz-raw"
INITRD_PATH="$VZRUNNER_DIR/.download/ubuntu/initramfs-containerd"
# Persistent block disk for /var/lib/containerd. Without it stage2 falls back
# to a virtiofs bind-mount, which does not satisfy containerd native snapshotter
# semantics (read-only rootfs errors on container start).
CONTAINERD_DISK="${CONTAINERD_DISK:-$HOME/.anvil-vz/containerd-disk.img}"

backend_name() { echo "vz-runner-docker"; }

VZ_PID_FILE="${TMPDIR:-/tmp}/vz-runner-bench.pid"
VZ_VM_PID_FILE="${VZ_PID_FILE}.vm"

backend_start() {
    rm -f "$VZ_PID_FILE" "$VZ_VM_PID_FILE"
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
        for _ in $(seq 1 300); do
            kill -0 "$pid" 2>/dev/null || break
            sleep 0.3
        done
        rm -f "$VZ_PID_FILE" "$VZ_VM_PID_FILE"
    fi
    pkill -f 'vz-runner daemon' || true
}

backend_stop_keep_snapshot() {
    backend_stop
}

backend_resume() {
    backend_start
}

backend_compose_cmd() {
    # anvil context сделан default; явно указываем для надёжности.
    echo "docker --context anvil compose"
}

backend_all_healthy() {
    local unhealthy
    unhealthy=$(docker --context anvil compose -f "$HOST_SHARE_ROOT/workloads/docker-compose.bench.yml" ps --format json 2>/dev/null \
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
    if not isinstance(obj, dict):
        continue
    state = obj.get("State", obj.get("Status", ""))
    if state != "running":
        n += 1
    elif obj.get("Health") not in ("healthy", ""):
        n += 1
print(n)
' 2>/dev/null || echo 999)
    [[ "$unhealthy" == "0" ]]
}

backend_idle_rss() {
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
