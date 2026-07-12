#!/bin/bash
# anvil-service.sh — start/stop/restart/status wrapper for the vz-runner daemon.
#
# On start: saves the current Docker context, launches vz-runner daemon,
# waits for it to be ready, and switches the Docker CLI context to "anvil".
# On stop: stops the daemon and restores the previously saved Docker context.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

VZRUNNER_BIN="${VZRUNNER_BIN:-$PROJECT_ROOT/.build/release/vz-runner}"
KERNEL_PATH="${KERNEL_PATH:-$PROJECT_ROOT/.download/ubuntu/vmlinuz-raw}"
INITRD_PATH="${INITRD_PATH:-$PROJECT_ROOT/.download/ubuntu/initramfs-containerd}"
CONTAINERD_DISK="${CONTAINERD_DISK:-$HOME/.anvil-vz/containerd-disk.img}"
SHARE_ROOT="${SHARE_ROOT:-$PROJECT_ROOT}"
MEMORY_GB="${ANVIL_MEMORY:-2}"

STATE_DIR="$HOME/.anvil-vz"
PID_FILE="$STATE_DIR/daemon.pid"
PREV_CONTEXT_FILE="$STATE_DIR/previous-docker-context"
LOG_FILE="$STATE_DIR/daemon.log"
LAUNCHAGENT_LOG="$STATE_DIR/launchagent.log"

mkdir -p "$STATE_DIR"

now_ms() {
    python3 -c 'import time; print(int(time.time()*1000))'
}

# Warn if the user's shell proxy settings will route localhost traffic away from
# the vz-runner listeners. The service itself is unaffected, but `docker`/`curl`
# commands run in the same shell may break on localhost.
check_proxy() {
    local proxy="${http_proxy:-${HTTP_PROXY:-}}"
    if [[ -z "$proxy" ]]; then
        proxy="${https_proxy:-${HTTPS_PROXY:-}}"
    fi
    if [[ -n "$proxy" ]]; then
        local noproxy="${NO_PROXY:-${no_proxy:-}}"
        if [[ "$noproxy" != *"localhost"* && "$noproxy" != *"127.0.0.1"* && "$noproxy" != *"0.0.0.0"* ]]; then
            echo "[anvil-service] warning: http_proxy=$proxy is set but NO_PROXY does not include localhost/127.0.0.1/0.0.0.0" >&2
            echo "[anvil-service]          localhost port forwards (e.g. curl localhost:8080) and tests using 0.0.0.0 may be sent to the proxy" >&2
            echo "[anvil-service]          fix: export NO_PROXY=localhost,127.0.0.1,0.0.0.0,${NO_PROXY:-}" >&2
        fi
    fi
}

is_running() {
    if [[ -f "$PID_FILE" ]]; then
        local pid
        pid="$(cat "$PID_FILE" 2>/dev/null || true)"
        if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
    fi
    return 1
}

cmd_start() {
    if is_running; then
        echo "[anvil-service] daemon already running (pid $(cat "$PID_FILE"))"
        docker context use anvil >/dev/null 2>&1 || true
        echo "[anvil-service] docker context: $(docker context show 2>/dev/null || echo unknown)"
        return 0
    fi

    if [[ ! -x "$VZRUNNER_BIN" ]]; then
        echo "[anvil-service] error: vz-runner binary not found at $VZRUNNER_BIN" >&2
        echo "[anvil-service] run 'make sign' first" >&2
        return 1
    fi

    local current_context
    current_context="$(docker context show 2>/dev/null || echo default)"
    if [[ "$current_context" != "anvil" ]]; then
        echo "$current_context" > "$PREV_CONTEXT_FILE"
    else
        rm -f "$PREV_CONTEXT_FILE"
    fi

    check_proxy

    echo "[anvil-service] starting vz-runner daemon..."
    rm -f "$PID_FILE"
    local disk_arg=""
    if [[ -f "$CONTAINERD_DISK" ]]; then
        disk_arg="--containerd-disk $CONTAINERD_DISK"
    fi
    local debug_arg=""
    if [[ "${DEBUG:-}" == "1" || "${DEBUG:-}" == "true" ]]; then
        debug_arg="--debug"
        touch "$SHARE_ROOT/.anvil-debug"
    else
        rm -f "$SHARE_ROOT/.anvil-debug"
    fi
    nohup "$VZRUNNER_BIN" daemon \
        --kernel "$KERNEL_PATH" \
        --initrd "$INITRD_PATH" \
        --share "$SHARE_ROOT" \
        --memory "$MEMORY_GB" \
        --idle 600 \
        $disk_arg \
        $debug_arg \
        >>"$LOG_FILE" 2>&1 &
    echo $! > "$PID_FILE"

    local start_ms
    start_ms=$(now_ms)
    local control_sock="$STATE_DIR/control.sock"
    until [[ -S "$control_sock" ]]; do
        local elapsed=$(( ($(now_ms) - start_ms) / 1000 ))
        if (( elapsed > 60 )); then
            echo "[anvil-service] error: daemon did not become ready within 60s" >&2
            return 1
        fi
        sleep 0.5
    done

    echo "[anvil-service] switching docker context to anvil..."
    docker context use anvil >/dev/null

    echo "[anvil-service] ready (pid $(cat "$PID_FILE"))"
}

cmd_stop() {
    if ! is_running; then
        echo "[anvil-service] daemon not running"
    else
        local pid
        pid="$(cat "$PID_FILE" 2>/dev/null || true)"
        echo "[anvil-service] stopping daemon (pid $pid)..."
        kill -TERM "$pid" 2>/dev/null || true
        local start_ms
        start_ms=$(now_ms)
        while kill -0 "$pid" 2>/dev/null; do
            local elapsed=$(( ($(now_ms) - start_ms) / 1000 ))
            if (( elapsed > 90 )); then
                echo "[anvil-service] warning: daemon did not stop gracefully, sending SIGKILL" >&2
                kill -KILL "$pid" 2>/dev/null || true
                break
            fi
            sleep 0.3
        done
        rm -f "$PID_FILE"
    fi

    local current_context
    current_context="$(docker context show 2>/dev/null || echo default)"
    if [[ "$current_context" == "anvil" ]]; then
        local target="default"
        if [[ -f "$PREV_CONTEXT_FILE" ]]; then
            target="$(cat "$PREV_CONTEXT_FILE")"
        fi
        echo "[anvil-service] restoring docker context to $target..."
        docker context use "$target" >/dev/null 2>&1 || true
        rm -f "$PREV_CONTEXT_FILE"
    fi
}

cmd_restart() {
    cmd_stop || true
    cmd_start
}

cmd_status() {
    if is_running; then
        echo "[anvil-service] daemon running (pid $(cat "$PID_FILE"))"
    else
        echo "[anvil-service] daemon not running"
    fi
    echo "[anvil-service] docker context: $(docker context show 2>/dev/null || echo unknown)"
}

case "${1:-}" in
    start)
        cmd_start
        ;;
    stop)
        cmd_stop
        ;;
    restart)
        cmd_restart
        ;;
    status)
        cmd_status
        ;;
    *)
        echo "Usage: $(basename "$0") {start|stop|restart|status}"
        exit 1
        ;;
esac
