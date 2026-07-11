#!/usr/bin/env bash
# Затягивает образы workload'а заранее на каждом backend, чтобы бенч
# мерил скорость раннера, а не скорость registry/сети.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGES=(postgres:16-alpine redis:7-alpine node:20-alpine nginx:alpine)

BACKEND="${1:-docker}"
LIMA_INSTANCE="${LIMA_INSTANCE:-anvil}"

case "$BACKEND" in
    lima)
        echo "Prepulling inside Lima VM ($LIMA_INSTANCE)..."
        for img in "${IMAGES[@]}"; do
            echo "  pulling $img"
            limactl shell "$LIMA_INSTANCE" docker pull "$img" >/dev/null
        done
        ;;
    vz-runner)
        VZRUNNER_BIN="${VZRUNNER_BIN:-vz-runner}"
        echo "Prepulling inside vz-runner VM (namespace: bench) via $VZRUNNER_BIN..."
        for img in "${IMAGES[@]}"; do
            echo "  pulling $img"
            "$VZRUNNER_BIN" exec nerdctl -n bench pull "$img" >/dev/null
        done
        ;;
    docker|colima|orbstack|docker-desktop|*)
        echo "Prepulling on current docker context ($(docker context show 2>/dev/null || echo default))..."
        for img in "${IMAGES[@]}"; do
            echo "  pulling $img"
            docker pull "$img" >/dev/null
        done
        ;;
esac

echo "done"
