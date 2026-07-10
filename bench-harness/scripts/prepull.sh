#!/usr/bin/env bash
# Затягивает образы workload'а заранее на каждом backend, чтобы бенч
# мерил скорость раннера, а не скорость registry/сети.
set -euo pipefail

IMAGES=(postgres:16-alpine redis:7-alpine node:20-alpine nginx:alpine)

echo "Prepulling on current docker context ($(docker context show 2>/dev/null || echo default))..."
for img in "${IMAGES[@]}"; do
    echo "  pulling $img"
    docker pull "$img" >/dev/null
done
echo "done"
