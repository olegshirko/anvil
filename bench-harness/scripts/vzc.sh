#!/usr/bin/env bash
# Тонкая обёртка: "vzc compose -f <host-path> up -d" -> транслирует
# host-путь workload-файла в guest-путь под virtiofs mount и делает
# vz-runner exec nerdctl compose ... внутри VM.
#
# Предполагается, что vz-runner делится SCRIPT_DIR/.. (корень проекта
# bench-harness) в VM через virtiofs на /mnt/anvil (как в M2).
set -euo pipefail

VZRUNNER_BIN="${VZRUNNER_BIN:-vz-runner}"
HOST_SHARE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GUEST_SHARE_ROOT="/mnt/anvil"

args=("$@")
for i in "${!args[@]}"; do
    if [[ "${args[$i]}" == -f ]]; then
        host_path="${args[$((i+1))]}"
        # host_path обычно .../bench-harness/workloads/docker-compose.bench.yml
        rel="${host_path#"$HOST_SHARE_ROOT"/}"
        args[$((i+1))]="$GUEST_SHARE_ROOT/$rel"
    fi
done

"$VZRUNNER_BIN" exec nerdctl -n bench compose "${args[@]}"
