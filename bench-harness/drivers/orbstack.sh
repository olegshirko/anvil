#!/usr/bin/env bash
# Driver for OrbStack. Requires OrbStack.app to be installed.

backend_name() { echo "OrbStack"; }

_ORBSTACK_APP="/Applications/OrbStack.app"
ORBSTACK_SOCK="${ORBSTACK_SOCK:-$HOME/.orbstack/run/docker.sock}"

_backend_orbstack_docker_config() {
    if [[ -z "${ORBSTACK_DOCKER_CONFIG:-}" ]]; then
        local tmp
        tmp="$(mktemp -d)"
        mkdir -p "$tmp"
        # OrbStack overwrites ~/.docker/config.json on start and sets
        # credsStore=osxkeychain, which is unavailable without Docker Desktop.
        # Use a temporary config without credential helpers.
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
# Docker Compose plugin sometimes looks at contexts even when DOCKER_HOST is
# set, so copy them into the temporary config dir.
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
    # `docker info` returns 0 without a server and shows only the Client.
    # Wait for ServerVersion, which appears only when the daemon is ready.
    wait_for "orbstack docker ready" 180 docker info --format '{{.ServerVersion}}'
}

backend_stop() {
    export DOCKER_CONFIG="$(_backend_orbstack_docker_config)"
    export DOCKER_HOST="unix://$ORBSTACK_SOCK"
    osascript -e 'quit app "OrbStack"' 2>/dev/null || true
    sleep 1
}

# OrbStack keeps the machine "warm" in the background and has no public
# snapshot API. For a fair comparison we treat resume as a restart after quit,
# which is the closest thing available to the user.
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
