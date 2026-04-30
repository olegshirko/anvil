#!/usr/bin/env bash

set -ex

alias anvil="$anvil_BINARY"

LIMA_MIN_VERSION="0.18.0"

# Ensure Lima meets the minimum version requirement.
check_lima() {
    local installed
    installed=$(limactl --version | awk '{print $3}')
    if [ "$(printf '%s\n' "$LIMA_MIN_VERSION" "$installed" | sort -V | head -n1)" != "$LIMA_MIN_VERSION" ]; then
        echo "Error: Lima $installed is installed, but $LIMA_MIN_VERSION is required"
        exit 1
    fi
}

DOCKER_CONTEXT="$(docker info -f '{{json .}}' | jq -r '.ClientInfo.Context')"

CROSS_ARCH="amd64"
if [ "$GOARCH" == "amd64" ]; then
    CROSS_ARCH="arm64"
fi

check_lima

# Print a formatted stage header.
banner() {
    set +x
    echo
    echo "######################################"
    echo "$@"
    echo "######################################"
    echo
    set -x
}

# Validate a single container runtime.
run_runtime_test() {
    local arch="$1"
    local runtime="$2"
    banner "runtime: ${runtime}, arch: ${arch}"

    local profile="itest-${runtime}"
    local cmd="docker"
    if [ "$runtime" == "containerd" ]; then
        cmd="$anvil_BINARY -p $profile nerdctl --"
    fi

    anvil="$anvil_BINARY -p $profile"

    $anvil delete -f || true
    $anvil start --arch "$arch" --runtime "$runtime"
    $cmd ps && $cmd info
    $anvil ssh -- nslookup host.docker.internal
    $cmd build integration
    $anvil delete -f
}

# Validate Kubernetes on a given runtime.
run_kubernetes_test() {
    local arch="$1"
    local runtime="$2"
    banner "k8s runtime: ${runtime}, arch: ${arch}"

    local profile="itest-${runtime}-k8s"
    anvil="$anvil_BINARY -p $profile"

    $anvil delete -f || true
    $anvil start --arch "$arch" --runtime "$runtime" --kubernetes
    sleep 5
    kubectl cluster-info && kubectl version && kubectl get nodes -o wide
    $anvil delete -f
}

run_runtime_test "$GOARCH" docker
run_runtime_test "$GOARCH" containerd
run_kubernetes_test "$GOARCH" docker
run_kubernetes_test "$GOARCH" containerd
run_runtime_test "$CROSS_ARCH" docker
run_runtime_test "$CROSS_ARCH" containerd

if [ -n "$DOCKER_CONTEXT" ]; then
    docker context use "$DOCKER_CONTEXT" || true
fi
