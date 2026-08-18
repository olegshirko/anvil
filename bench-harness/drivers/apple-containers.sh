#!/usr/bin/env bash
# Driver for Apple Containers (the `container` CLI from apple/container,
# macOS 26+). Install from https://github.com/apple/container/releases
# (`container system kernel set --recommended` once after install), or point
# CONTAINER_BIN at a non-PATH location.
#
# Differences from docker backends, kept honest on purpose:
# - No compose: scripts/apple-compose runs the same workload as plain
#   `container run` calls.
# - No snapshot/resume API: "resume" is a second cold start (apiserver
#   restart), like Colima/Lima.

CONTAINER_BIN="${CONTAINER_BIN:-container}"
export CONTAINER_BIN

backend_name() { echo "Apple Containers"; }

backend_is_available() {
    command -v "$CONTAINER_BIN" >/dev/null 2>&1
}

# Epoch of the last backend_start; used by backend_idle_rss to pick only the
# VirtualMachine XPC processes spawned by this harness (other VZ users —
# Lima, Colima, anvil — may run in parallel).
APPLE_START_EPOCH=0

backend_start() {
    APPLE_START_EPOCH=$(( $(date +%s) - 1 ))
    # The apiserver and its plugins are per-user launchd services; start is
    # idempotent. If no default kernel is configured yet, start still brings
    # the apiserver up (kernel setup fails separately, with a clear error
    # from container run).
    "$CONTAINER_BIN" system start >/dev/null 2>&1 || true
    wait_for "apple containers apiserver ready" 60 "$CONTAINER_BIN" system status
}

backend_stop() {
    "$CONTAINER_BIN" stop --all >/dev/null 2>&1 || true
    "$CONTAINER_BIN" delete --all --force >/dev/null 2>&1 || true
    "$CONTAINER_BIN" system stop >/dev/null 2>&1 || true
}

# No snapshot/resume API: the harness falls back to backend_stop + a second
# cold start, same as Colima.
backend_stop_keep_snapshot() {
    return 1
}

backend_resume() {
    backend_start
}

backend_compose_cmd() {
    echo "$SCRIPT_DIR/scripts/apple-compose"
}

backend_all_healthy() {
    # All four services must be running and answer the same probes as the
    # compose healthchecks in the workload file.
    local running
    running="$("$CONTAINER_BIN" ls --format json 2>/dev/null | python3 -c '
import sys, json
try:
    arr = json.load(sys.stdin)
except json.JSONDecodeError:
    sys.exit(0)
print(" ".join(c.get("id", "") for c in arr
               if c.get("status", {}).get("state") == "running"))
' 2>/dev/null)"
    local svc
    for svc in bench-db bench-cache bench-api bench-web; do
        [[ " $running " == *" $svc "* ]] || return 1
    done
    "$CONTAINER_BIN" exec bench-db pg_isready -U postgres >/dev/null 2>&1 || return 1
    "$CONTAINER_BIN" exec bench-cache redis-cli ping 2>/dev/null | grep -q PONG || return 1
    "$CONTAINER_BIN" exec bench-api wget -qO- http://localhost:3000 >/dev/null 2>&1 || return 1
    "$CONTAINER_BIN" exec bench-web wget -qO- http://localhost:80 >/dev/null 2>&1 || return 1
    return 0
}

_start_epoch_of() {
    ps -o lstart= -p "$1" 2>/dev/null \
        | xargs -I{} date -j -f "%a %b %d %H:%M:%S %Y" "{}" +%s 2>/dev/null
}

backend_idle_rss() {
    # Sum RSS of the apiserver, the plugin processes (runtime, images,
    # network) and the VirtualMachine XPC processes spawned after
    # backend_start — i.e. everything Apple Containers adds on the host.
    local total=0 pid st
    for pid in $(pgrep -f "bin/container-apiserver"); do
        total=$(( total + $(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ' || echo 0) ))
    done
    for pid in $(pgrep -f "libexec/container/plugins"); do
        total=$(( total + $(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ' || echo 0) ))
    done
    for pid in $(pgrep -f "com.apple.Virtualization.VirtualMachine"); do
        st="$(_start_epoch_of "$pid")"
        [[ -n "$st" ]] || continue
        (( st >= APPLE_START_EPOCH )) && total=$(( total + $(ps -o rss= -p "$pid" | tr -d ' ') ))
    done
    echo $(( total / 1024 ))
}
