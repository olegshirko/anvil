#!/usr/bin/env bash
# Thin wrapper: "vzc compose -f <host-path> up -d" runs host-side
# `docker compose` against anvil's Docker API socket. The project name is
# pinned to "workloads" (the compose file directory), matching the namespace
# the guest-side flow used to derive.
#
# Assumes vz-runner shares SCRIPT_DIR/.. (the bench-harness root) into the VM;
# the daemon's docker.sock is proxied by vz-runner to ~/.anvil-vz/docker.sock.
set -euo pipefail

DOCKER_SOCKET="${ANVIL_DOCKER_SOCK:-$HOME/.anvil-vz/docker.sock}"
export DOCKER_HOST="unix://${DOCKER_SOCKET}"

exec docker compose --project-name workloads "$@"
