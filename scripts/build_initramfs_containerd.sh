#!/bin/bash
set -euo pipefail

# Build a minimal initramfs with containerd + runc + nerdctl for M3 testing.
# This is intentionally simple: everything runs from RAM.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DOWNLOAD_DIR="$ROOT/.download"
ALPINE_DIR="$DOWNLOAD_DIR/alpine"
UBUNTU_DIR="$DOWNLOAD_DIR/ubuntu"
TOOLS_DIR="$DOWNLOAD_DIR/container-tools"
WORK_DIR="$UBUNTU_DIR/initrd-work-containerd"
OUT="$UBUNTU_DIR/initramfs-containerd"

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
if [[ ! -d "$TOOLS_DIR" ]]; then
    echo "container tools not found at $TOOLS_DIR"
    exit 1
fi

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR"

cd "$WORK_DIR"

# Base userspace from Alpine initramfs.
gunzip -c "$ALPINE_INITRD" | cpio -idm 2>/dev/null || true

# Busybox applets.
for applet in mount umount mkdir mknod sleep sh insmod \
              uname id cat ls ps echo printf chmod chown \
              kill killall pwd whoami hostname udhcpc ifconfig \
              ip grep ntpd switch_root cp rm mv head mountpoint; do
    ln -sf busybox "bin/$applet"
done

# Inject guest agent.
cp "$AGENT_BIN" bin/guest-agent
chmod +x bin/guest-agent

# Container tools.
mkdir -p opt/containerd/bin
tar -xzf "$TOOLS_DIR/containerd.tgz" -C opt/containerd/bin --strip-components=1 2>/dev/null || true
tar -xzf "$TOOLS_DIR/nerdctl.tgz" -C opt/containerd/bin 2>/dev/null || true
cp "$TOOLS_DIR/runc" opt/containerd/bin/runc
chmod +x opt/containerd/bin/*
ln -sf /opt/containerd/bin/nerdctl bin/nerdctl
ln -sf /opt/containerd/bin/ctr bin/ctr

# Ubuntu kernel modules: vsock + virtiofs.
mkdir -p lib/modules/vsock lib/modules/virtiofs lib/modules/overlayfs
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

OVERLAY_SRC="$DEB_WORK/lib/modules/*/kernel/fs/overlayfs/overlay.ko.zst"
if [[ -f $(echo $OVERLAY_SRC) ]]; then
    zstd -d -f $(echo $OVERLAY_SRC) -o lib/modules/overlayfs/overlay.ko
else
    echo "missing overlayfs module"
    exit 1
fi

# Init script.
cat > myinit <<'EOF'
#!/bin/sh
export PATH=/bin:/sbin:/usr/bin:/usr/sbin

mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
mkdir -p /tmp /var/log /sys/fs/cgroup
mount -t cgroup2 cgroup2 /sys/fs/cgroup

# Load modules.
insmod /lib/modules/vsock/vsock.ko
insmod /lib/modules/vsock/vmw_vsock_virtio_transport_common.ko
insmod /lib/modules/vsock/vmw_vsock_virtio_transport.ko
insmod /lib/modules/virtiofs/virtiofs.ko
insmod /lib/modules/overlayfs/overlay.ko

for i in 1 2 3 4 5 6 7 8 9 10; do
    if [ -e /dev/vsock ]; then break; fi
    sleep 0.2
done

mkdir -p /mnt/anvil
mount -t virtiofs anvil /mnt/anvil 2>/dev/null || true

# Bring up NAT network via virtio-net.
echo "[myinit] configuring network"
ifconfig lo up 2>/dev/null || true
ifconfig eth0 up 2>/dev/null || true
udhcpc -i eth0 -s /usr/share/udhcpc/default.script >/tmp/udhcpc.log 2>&1 &
for i in 1 2 3 4 5 6 7 8 9 10; do
    if ip addr show eth0 2>/dev/null | grep -q 'inet '; then break; fi
    sleep 0.5
done
ip addr show eth0 >/tmp/network.log 2>&1 || true
mkdir -p /etc
printf 'nameserver 8.8.8.8\nnameserver 8.8.4.4\n' > /etc/resolv.conf

# Sync clock; TLS to registries fails with 1970-01-01.
echo "[myinit] syncing clock"
ntpd -nq -p pool.ntp.org >/tmp/ntpd.log 2>&1 || true

# containerd needs /etc/containerd and a state dir.
mkdir -p /etc/containerd /run/containerd /var/lib/containerd
# Separate tmpfs so runc pivot_root can jail the container rootfs.
mount -t tmpfs tmpfs /var/lib/containerd
cat > /etc/containerd/config.toml <<'CTREOF'
version = 2
root = "/var/lib/containerd"
state = "/run/containerd"

[plugins."io.containerd.grpc.v1.cri"]
  snapshotter = "native"
CTREOF

# Switch to a real tmpfs root so runc can pivot_root container rootfs.
# The initramfs rootfs is not a separate mount, which breaks runc.
echo "[myinit] preparing tmpfs root"
mkdir -p /newroot
mount -t tmpfs tmpfs /newroot
# Copy the rootfs contents; bind mounts keep us on rootfs, which breaks pivot_root.
for d in bin sbin lib etc opt; do
    rm -rf /newroot/$d
    if [ -d "/$d" ]; then
        cp -a /$d /newroot/ 2>/dev/null || true
    fi
done
for d in usr/bin usr/sbin usr/lib; do
    rm -rf /newroot/$d
    mkdir -p /newroot/usr
    if [ -d "/$d" ]; then
        cp -a /$d /newroot/usr/ 2>/dev/null || true
    fi
done
mkdir -p /newroot/proc /newroot/sys /newroot/dev /newroot/run /newroot/tmp /newroot/var /newroot/mnt
mount --make-rprivate /
mount --move /proc /newroot/proc
mount --move /sys  /newroot/sys
mount --move /dev  /newroot/dev
mount --move /mnt/anvil /newroot/mnt/anvil 2>/dev/null || true
mount --move /var/lib/containerd /newroot/var/lib/containerd 2>/dev/null || true

cat > /newroot/stage2.sh <<'STAGE2'
export PATH=/bin:/sbin:/usr/bin:/usr/sbin

# Remount virtual filesystems if switch_root did not move them.
mountpoint -q /proc || mount -t proc proc /proc
mountpoint -q /sys  || mount -t sysfs sys /sys
mountpoint -q /dev  || mount -t devtmpfs dev /dev
mkdir -p /dev/pts
mountpoint -q /dev/pts || mount -t devpts devpts /dev/pts
mkdir -p /sys/fs/cgroup
mountpoint -q /sys/fs/cgroup || mount -t cgroup2 cgroup2 /sys/fs/cgroup

# Re-mount virtiofs share and containerd tmpfs if not already moved.
mkdir -p /mnt/anvil
mountpoint -q /mnt/anvil || mount -t virtiofs anvil /mnt/anvil 2>/dev/null || true

# Ensure mount propagation is private so runc can pivot_root into containers.
mount --make-rprivate /

# Start containerd in background.
echo "[stage2] starting containerd"
/opt/containerd/bin/containerd > /tmp/containerd.log 2>&1 &

# Wait for containerd socket.
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    if [ -S /run/containerd/containerd.sock ]; then break; fi
    sleep 0.5
done

# Start guest agent in foreground.
echo "[stage2] starting guest-agent"
exec /bin/guest-agent
STAGE2
chmod +x /newroot/stage2.sh
exec switch_root /newroot /bin/sh /stage2.sh
EOF
chmod +x myinit

find . -print0 | cpio --null -o --format=newc 2>/dev/null | gzip -9 > "$OUT"

echo "created $OUT"
