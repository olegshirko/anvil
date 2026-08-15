#!/usr/bin/env bash
# Driver for Lima (instance "anvil"). Requires: brew install lima docker docker-compose

LIMA_INSTANCE="${LIMA_INSTANCE:-anvil}"

backend_name() { echo "Lima ($LIMA_INSTANCE)"; }

backend_is_available() {
    # The VM instance must exist; harness itself will start/stop it.
    command -v limactl >/dev/null 2>&1 && \
        limactl list "$LIMA_INSTANCE" --format '{{.Name}}' 2>/dev/null | grep -qx "$LIMA_INSTANCE"
}

# Internal wrapper for docker commands inside the Lima VM
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

# Lima has no API for instant VM snapshot/resume.
# "Resume" here means a normal warm start after stop.
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

_start_epoch_of() {
    ps -o lstart= -p "$1" 2>/dev/null \
        | xargs -I{} date -j -f "%a %b %d %H:%M:%S %Y" "{}" +%s 2>/dev/null
}

backend_idle_rss() {
    # Sum RSS of THIS instance's processes: limactl hostagent/usernet mention
    # the instance name, and the VZ VM process is disambiguated by start
    # time (other VZ users — an anvil build VM, colima — may run in
    # parallel; a bare grep would count them too).
    local total=0 pid
    for pid in $(pgrep -f "limactl.*${LIMA_INSTANCE}"); do
        total=$(( total + $(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ' || echo 0) ))
    done
    local start
    start="$(_start_epoch_of "$(pgrep -f "limactl hostagent.*${LIMA_INSTANCE}" | head -1)")"
    if [[ -n "$start" ]]; then
        for pid in $(pgrep -f "com.apple.Virtualization.VirtualMachine"); do
            local st
            st="$(_start_epoch_of "$pid")"
            [[ -n "$st" ]] || continue
            (( st >= start )) && total=$(( total + $(ps -o rss= -p "$pid" | tr -d ' ') ))
        done
    fi
    for pid in $(pgrep -f "qemu-system.*${LIMA_INSTANCE}"); do
        total=$(( total + $(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ' || echo 0) ))
    done
    echo $(( total / 1024 ))
}
