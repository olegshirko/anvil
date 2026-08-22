#!/usr/bin/env bash
# Driver for vz-runner. Requires the vz-runner binary in PATH (or VZRUNNER_BIN).
#
# Current vz-runner CLI:
#   vz-runner daemon [--share <path>] [--idle <seconds>]
#   vz-runner status
#   vz-runner exec <cmd>...
#
# Harness starts the daemon in the background, waits for status, and stops it
# with SIGTERM (the daemon pauses and saves a snapshot in its shutdown handler).

VZRUNNER_BIN="${VZRUNNER_BIN:-vz-runner}"
# Share the bench-harness root into the VM via virtiofs at /mnt/anvil.
HOST_SHARE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GUEST_SHARE_ROOT="/mnt/anvil"
# Kernel/initrd paths are relative to the project root.
VZRUNNER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KERNEL_PATH="$VZRUNNER_DIR/.download/alpine/vmlinuz-raw"
INITRD_PATH="$VZRUNNER_DIR/.download/ubuntu/initramfs-containerd"
# Persistent block disk for /var/lib/containerd.
CONTAINERD_DISK="${CONTAINERD_DISK:-$HOME/.anvil-vz/containerd-disk.img}"

backend_name() { echo "vz-runner"; }

# --- no pid file; remember the daemon pid ourselves ---
VZ_PID_FILE="${TMPDIR:-/tmp}/vz-runner-bench.pid"
VZ_VM_PID_FILE="${VZ_PID_FILE}.vm"

backend_start() {
    rm -f "$VZ_PID_FILE" "$VZ_VM_PID_FILE"
    # Start daemon detached from terminal but keep its pid.
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
    # Capture the VirtualMachine process spawned by this daemon. Other VZ
    # users may run at the same time (e.g. a Lima VM): picking by highest pid
    # grabbed the wrong process and underreported RSS badly. Pick the VM
    # process that started at/after the daemon itself (epoch start times).
    local daemon_epoch p st_epoch
    daemon_epoch="$(ps -o lstart= -p "$(cat "$VZ_PID_FILE")" 2>/dev/null \
        | xargs -I{} date -j -f "%a %b %d %H:%M:%S %Y" "{}" +%s 2>/dev/null)"
    [[ -n "$daemon_epoch" ]] || daemon_epoch="$(date +%s)"
    for _ in $(seq 1 50); do
        vm_pid=""
        for p in $(pgrep -f "com.apple.Virtualization.VirtualMachine"); do
            st_epoch="$(ps -o lstart= -p "$p" 2>/dev/null \
                | xargs -I{} date -j -f "%a %b %d %H:%M:%S %Y" "{}" +%s 2>/dev/null)"
            [[ -n "$st_epoch" ]] || continue
            if (( st_epoch >= daemon_epoch )); then
                vm_pid="$p"  # pgrep lists ascending pids; keep the last match
            fi
        done
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
    # Fallback: kill any leftover vz-runner daemon from this harness.
    pkill -f 'vz-runner daemon' || true
}

# vz-runner daemon saves the snapshot on SIGTERM, so keep-snapshot is just a
# graceful stop.
backend_stop_keep_snapshot() {
    backend_stop
}

# Remove the saved snapshot so the next backend_start is a true cold boot
# (the daemon saves a snapshot on every graceful stop, including the one the
# harness runs right before the "cold start" phase).
backend_cold_reset() {
    rm -f "$HOME/.anvil-vz/snapshots/default.vzstate" \
          "$HOME/.anvil-vz/snapshots/default.config-hash"
}

backend_resume() {
    # On restart the daemon restores from snapshot if it is valid.
    backend_start
}

backend_compose_cmd() {
    echo "$SCRIPT_DIR/scripts/vzc.sh"
}

backend_all_healthy() {
    local unhealthy
    unhealthy=$("$SCRIPT_DIR/scripts/vzc.sh" -f "$HOST_SHARE_ROOT/workloads/docker-compose.bench.yml" ps --format json 2>/dev/null \
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
    # Sum RSS of the vz-runner daemon and the VM process started by this harness.
    # Guest processes inside the VM are not counted; their RSS is not directly
    # comparable to host-side processes of other backends.
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
