#!/usr/bin/env bash
# Driver for Colima. Requires: brew install colima docker docker-compose

backend_name() { echo "Colima"; }

# Colima docker socket + isolated docker config without desktop credential helper,
# so `docker pull` does not fail because docker-credential-desktop is missing.
COLIMA_SOCK="${COLIMA_SOCK:-$HOME/.colima/default/docker.sock}"

_backend_colima_docker_config() {
    if [[ -z "${COLIMA_DOCKER_CONFIG:-}" ]]; then
        local tmp
        tmp="$(mktemp -d)"
        mkdir -p "$tmp"
        # Copy host config but remove desktop credential helper; otherwise
        # `docker pull` tries to call docker-credential-desktop, which is not in PATH.
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
    # Colima DNS forwarder sometimes becomes ready later than the docker daemon.
    # Check that auth.docker.io resolves from inside the VM.
    colima ssh -- sh -c 'curl -sS -o /dev/null --connect-timeout 5 "https://auth.docker.io/token?scope=repository%3Alibrary%2Fnginx%3Apull&service=registry.docker.io"' >/dev/null 2>&1
}

backend_start() {
    export DOCKER_CONFIG="$(_backend_colima_docker_config)"
    export DOCKER_HOST="unix://$COLIMA_SOCK"
    colima start --vm-type=vz --mount-type=virtiofs >/dev/null 2>&1
    # Colima start can take 30-60 s on Apple Silicon.
    # `docker info` returns 0 even without a server, so wait for ServerVersion.
    wait_for "colima docker ready" 180 docker info --format '{{.ServerVersion}}'
    # Also wait for the DNS forwarder inside the VM; without it compose up
    # times out on auth.docker.io.
    wait_for "colima DNS ready" 60 _backend_colima_dns_ready
}

backend_stop() {
    export DOCKER_CONFIG="$(_backend_colima_docker_config)"
    export DOCKER_HOST="unix://$COLIMA_SOCK"
    colima stop >/dev/null 2>&1 || true
}

# Colima has no instant VM snapshot/resume. "Resume" here is a normal start
# after stop (same disk, no suspended state). This honestly shows the gap vs
# vz-runner.
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
    # Sum RSS of all colima/limactl/Virtualization-related host processes.
    # On Apple Silicon Colima uses limactl hostagent + usernet + the
    # com.apple.Virtualization.VirtualMachine process.
    ps aux | grep -E 'colima|limactl|qemu-system|Virtualization' | grep -v grep \
        | awk '{sum+=$6} END {printf "%.0f", sum/1024}'
}
