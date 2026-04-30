#!/usr/bin/env sh

# Mounts the Anvil data disk inside the Lima VM.
# The disk is identified by label and mounted at /mnt/lima-<instance>.

LABEL="lima-{{ .InstanceId }}"
TARGET="/mnt/${LABEL}"

# If the mount point already has content, assume the disk is mounted.
if [ -d "${TARGET}" ]; then
    if [ -n "$(find "${TARGET}" -mindepth 1 -print -quit 2>/dev/null)" ]; then
        echo "Data disk already mounted, skipping."
        exit 0
    fi
fi

# Find the block device. If /dev/vdb is used by cidata, use /dev/vdc.
DEVICE="/dev/vdb"
if df -h /mnt/lima-cidata/ | tail -n +2 | grep -q '^/dev/vdb'; then
    DEVICE="/dev/vdc"
fi
PART="${DEVICE}1"

{{ if .Format }}
echo 'type=83' | sudo sfdisk "${DEVICE}"
mkfs.ext4 "${PART}"
e2label "${PART}" "${LABEL}"
{{ end }}

mkdir -p "${TARGET}"
mount "${PART}" "${TARGET}"
