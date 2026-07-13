#!/bin/bash
set -euo pipefail

# Build a test initramfs for M1 using Ubuntu's kernel modules for vsock
# and Alpine's initramfs busybox as the userspace base.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DOWNLOAD_DIR="$ROOT/.download"
ALPINE_DIR="$DOWNLOAD_DIR/alpine"
UBUNTU_DIR="$DOWNLOAD_DIR/ubuntu"
WORK_DIR="$UBUNTU_DIR/initrd-work-agent"
OUT="$UBUNTU_DIR/initramfs-agent"

ALPINE_INITRD="$ALPINE_DIR/initramfs-virt"
AGENT_BIN="$ALPINE_DIR/guest-agent"
UBUNTU_DEB="$UBUNTU_DIR/linux-modules.deb"

if [[ ! -f "$ALPINE_INITRD" ]]; then
    echo "Alpine initramfs not found; run 'make download-alpine' first"
    exit 1
fi

if [[ ! -f "$AGENT_BIN" ]]; then
    echo "guest-agent not found; run 'make guest-agent' first"
    exit 1
fi

if [[ ! -f "$UBUNTU_DEB" ]]; then
    echo "Ubuntu modules .deb not found at $UBUNTU_DEB"
    exit 1
fi

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR"

cd "$WORK_DIR"

# Use Alpine initramfs as base userspace.
gunzip -c "$ALPINE_INITRD" | cpio -idm 2>/dev/null || true

# Inject guest agent.
cp "$AGENT_BIN" bin/guest-agent
chmod +x bin/guest-agent

# Symlink busybox applets we need inside the VM.
for applet in mount umount mkdir mknod sleep sh insmod \
              uname id cat ls ps echo printf chmod chown \
              kill killall pwd whoami hostname; do
    ln -sf busybox "bin/$applet"
done

# Extract Ubuntu vsock and virtiofs modules and decompress them.
mkdir -p lib/modules/vsock lib/modules/virtiofs
DEB_WORK="$UBUNTU_DIR/deb-extract"
mkdir -p "$DEB_WORK"
cd "$DEB_WORK"
rm -rf *
ar x "$UBUNTU_DEB"
tar -xf data.tar
cd "$WORK_DIR"

MOD_SRC="$DEB_WORK/lib/modules/*/kernel/net/vmw_vsock"
for mod in vsock vmw_vsock_virtio_transport_common vmw_vsock_virtio_transport; do
    src="$(echo $MOD_SRC/$mod.ko.zst)"
    if [[ -f "$src" ]]; then
        zstd -d -f "$src" -o "lib/modules/vsock/$mod.ko"
    else
        echo "missing module: $mod"
        exit 1
    fi
done

VIRTIOFS_SRC="$DEB_WORK/lib/modules/*/kernel/fs/fuse/virtiofs.ko.zst"
if [[ -f $(echo $VIRTIOFS_SRC) ]]; then
    zstd -d -f $(echo $VIRTIOFS_SRC) -o lib/modules/virtiofs/virtiofs.ko
else
    echo "missing virtiofs module"
    exit 1
fi

# Create a minimal init.
cat > myinit <<'EOF'
#!/bin/sh
export PATH=/bin:/sbin:/usr/bin:/usr/sbin

mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts

# Load virtio-vsock modules (Ubuntu modules extracted from linux-modules deb).
insmod /lib/modules/vsock/vsock.ko
insmod /lib/modules/vsock/vmw_vsock_virtio_transport_common.ko
insmod /lib/modules/vsock/vmw_vsock_virtio_transport.ko

# Load virtiofs module.
insmod /lib/modules/virtiofs/virtiofs.ko

# Wait for /dev/vsock to appear.
for i in 1 2 3 4 5 6 7 8 9 10; do
    if [ -e /dev/vsock ]; then
        break
    fi
    sleep 0.2
done

if [ ! -e /dev/vsock ]; then
    echo "[myinit] /dev/vsock still not found"
    # Keep VM alive for debugging instead of panicking.
    exec /bin/sh
fi

# Mount virtiofs share if the device is present.
mkdir -p /mnt/anvil
mount -t virtiofs anvil /mnt/anvil 2>/dev/null || echo "[myinit] virtiofs mount skipped"

echo "[myinit] starting guest-agent"
exec /bin/guest-agent
EOF
chmod +x myinit

# Repack.
find . -print0 | cpio --null -o --format=newc 2>/dev/null | gzip -9 > "$OUT"

echo "created $OUT"
