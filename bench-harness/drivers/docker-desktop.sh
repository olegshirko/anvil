#!/usr/bin/env bash
# Driver for Docker Desktop. Requires Docker Desktop.app to be installed.

backend_name() { echo "Docker Desktop"; }

backend_is_available() {
    [[ -d "/Applications/Docker.app" ]] || \
        mdfind "kMDItemCFBundleIdentifier == 'com.docker.docker'" 2>/dev/null | grep -q .
}

backend_cold_reset() {
    # Runs while the backend is stopped, so the actual container cleanup
    # happens in backend_start once the API is reachable (see there).
    return 0
}

backend_start() {
    open -a Docker
    # `docker info` returns 0 without a server and shows only the Client.
    # And a single successful call is not enough: right after ServerVersion
    # appears, /_ping can still answer 500 while the API warms up. Require
    # sustained clean round-trips before declaring readiness.
    if ! wait_for "docker desktop ready" 90 docker_desktop_stable; then
        # Docker Desktop 4.86 on macOS 26 beta: after a quit/open cycle its
        # Linux VM sometimes never boots (apiproxy: "no route to host" to
        # 192.168.65.7 forever). Recover with a hard kill + fresh start; the
        # measured time then still reflects what a user experiences.
        echo "!! docker desktop VM stuck, hard-restarting" >&2
        pkill -9 -f "com.docker.backend" 2>/dev/null || true
        pkill -9 -f "Docker Desktop" 2>/dev/null || true
        pkill -9 -f "com.docker.virtualmachine" 2>/dev/null || true
        sleep 3
        open -a Docker
        wait_for "docker desktop ready (after hard reset)" 180 docker_desktop_stable || return 1
    fi
    # A previously failed run can leave workload containers behind in a
    # broken state ("RWLayer of container ... is unexpectedly nil"), which
    # makes the next compose up fail forever. Force-remove leftovers by the
    # compose project label and the shared network.
    docker --context desktop-linux ps -aq --filter label=com.docker.compose.project=workloads 2>/dev/null \
        | xargs -r docker --context desktop-linux rm -f >/dev/null 2>&1 || true
    docker --context desktop-linux network rm workloads_default >/dev/null 2>&1 || true
}

docker_desktop_stable() {
    # Each probe is time-boxed: on the broken-VM path docker ps hangs
    # forever instead of failing, which would stall wait_for.
    local i
    for i in $(seq 1 10); do
        docker_desktop_probe info >/dev/null 2>&1 || return 1
        docker_desktop_probe ps >/dev/null 2>&1 || return 1
        sleep 0.3
    done
}

docker_desktop_probe() {
    case "$1" in
        info) perl -e 'alarm 5; exec @ARGV' docker --context desktop-linux info --format '{{.ServerVersion}}' ;;
        ps)   perl -e 'alarm 5; exec @ARGV' docker --context desktop-linux ps ;;
    esac
}

backend_stop() {
    # Ask for a graceful quit first (data safety), give it 10s, then ALWAYS
    # hard-kill. On Docker Desktop 4.86 / macOS 26 beta the quit/open cycle
    # is broken both ways: a lingering backend makes the relaunched one skip
    # engine start entirely, and even a fully graceful quit can leave state
    # that keeps the next VM from booting (apiproxy: no route to host
    # forever). Starting from a hard-killed state is the only reliably
    # working path (~2s to API), so normalize on it.
    osascript -e 'quit app "Docker"' 2>/dev/null || true
    local i
    for i in $(seq 1 10); do
        ps aux | grep -q "[c]om.docker.backend" || break
        sleep 1
    done
    pkill -9 -f "com.docker.backend" 2>/dev/null || true
    pkill -9 -f "Docker Desktop" 2>/dev/null || true
    pkill -9 -f "com.docker.virtualmachine" 2>/dev/null || true
    sleep 2
    return 0
}

backend_stop_keep_snapshot() {
    return 1  # Docker Desktop has no snapshot/resume
}

backend_resume() {
    # Same quit/open fragility as backend_start: if the VM did not come
    # back, hard-restart once and measure that (the user experience).
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
