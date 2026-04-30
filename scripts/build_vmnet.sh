#!/usr/bin/env sh

set -ex

WORK_DIR="${PWD}/_build/network"
SOURCE_DIR="${WORK_DIR}/socket_vmnet"
ASSET_DIR="${PWD}/internal/embedded/network"

# Clone the socket_vmnet repository if not already present.
fetch_source() {
    if [ ! -d "$2" ]; then
        git clone "$1" "$2"
    fi
}

mkdir -p "${WORK_DIR}"
fetch_source https://github.com/lima-vm/socket_vmnet.git "${SOURCE_DIR}"

# Package built binaries into a gzipped tarball for embedding.
package_binaries() {
    mkdir -p "${ASSET_DIR}/vmnet/bin"
    cp "${SOURCE_DIR}/socket_vmnet" "${SOURCE_DIR}/socket_vmnet_client" "${ASSET_DIR}/vmnet/bin"
    (
        cd "${ASSET_DIR}/vmnet"
        tar cvfz "${ASSET_DIR}/vmnet_${1}.tar.gz" bin/socket_vmnet bin/socket_vmnet_client
    )
    rm -rf "${ASSET_DIR}/vmnet"
}

# Build socket_vmnet for x86_64.
build_for_amd64() {
    cd "${SOURCE_DIR}"
    git checkout v1.1.5
    make ARCH=x86_64
    package_binaries x86_64
    make clean
}

# Build socket_vmnet for arm64.
build_for_arm64() {
    cd "${SOURCE_DIR}"
    git checkout v1.1.5
    make ARCH=arm64
    package_binaries arm64
    make clean
}

# Verify that x86_64 and arm64 archives contain different binaries.
validate_distinct_archives() {
    TEMP_DIR="/tmp/anvil-vmnet-check"
    rm -rf "${TEMP_DIR}"
    mkdir -p "${TEMP_DIR}/x86" "${TEMP_DIR}/arm"

    cp "${ASSET_DIR}/vmnet_x86_64.tar.gz" "${TEMP_DIR}/x86"
    (cd "${TEMP_DIR}/x86" && tar xvfz vmnet_x86_64.tar.gz)

    cp "${ASSET_DIR}/vmnet_arm64.tar.gz" "${TEMP_DIR}/arm"
    (cd "${TEMP_DIR}/arm" && tar xvfz vmnet_arm64.tar.gz)

    assert_different() {
        if diff "${TEMP_DIR}/x86/$1" "${TEMP_DIR}/arm/$1"; then
            echo "Error: $1 is identical across architectures"
            exit 1
        fi
    }

    assert_different bin/socket_vmnet
    assert_different bin/socket_vmnet_client
}

build_for_amd64
build_for_arm64
validate_distinct_archives
