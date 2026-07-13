#!/usr/bin/env bash
# Thin wrapper: "vzc compose -f <host-path> up -d" translates the host workload
# path to the guest path under the virtiofs mount and runs
# vz-runner exec nerdctl compose ... inside the VM.
#
# Assumes vz-runner shares SCRIPT_DIR/.. (the bench-harness root) into the VM
# at /mnt/anvil via virtiofs (see M2).
set -euo pipefail

VZRUNNER_BIN="${VZRUNNER_BIN:-vz-runner}"
HOST_SHARE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GUEST_SHARE_ROOT="/mnt/anvil"

args=("$@")
for i in "${!args[@]}"; do
    if [[ "${args[$i]}" == -f ]]; then
        host_path="${args[$((i+1))]}"
        # host_path is usually .../bench-harness/workloads/docker-compose.bench.yml
        rel="${host_path#"$HOST_SHARE_ROOT"/}"
        args[$((i+1))]="$GUEST_SHARE_ROOT/$rel"
    fi
done

"$VZRUNNER_BIN" exec nerdctl -n bench compose "${args[@]}"
