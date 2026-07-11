#!/bin/bash
set -euo pipefail

# Build a minimal initramfs with containerd + runc + nerdctl for M3 testing.
# The whole rootfs tree is assembled inside a Linux container so that
# case-differing filenames (e.g. libxt_MARK.so vs libxt_mark.so) survive
# and the final cpio archive is produced by GNU cpio instead of a custom
# packer. Only the resulting .cpio.gz is written back to the host.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# If we are not already inside the build container, re-run this script there.
if [[ -z "${IN_CONTAINER:-}" ]]; then
    # Force the default Docker context so a user-selected anvil context does
    # not break the build container step.
    docker --context default run --rm --platform linux/arm64 \
        -v "$ROOT/.download:/build/download" \
        -v "$SCRIPT_DIR:/scripts:ro" \
        -e IN_CONTAINER=1 \
        -e OUT=/build/download/ubuntu/initramfs-containerd \
        -e HOST_UID="$(id -u)" \
        -e HOST_GID="$(id -g)" \
        alpine:3.20 \
        sh -c 'apk add --no-cache bash cpio zstd binutils && exec bash /scripts/build_initramfs_containerd.sh'
    exit 0
fi

# ---------------------------------------------------------------------------
# Everything below runs inside the Linux build container.
# ---------------------------------------------------------------------------

set -euo pipefail

# Tools needed to assemble and pack the initramfs.
apk add --no-cache cpio zstd binutils

ROOT=/build
SCRIPT_DIR=/scripts
DOWNLOAD_DIR=/build/download
ALPINE_DIR="$DOWNLOAD_DIR/alpine"
UBUNTU_DIR="$DOWNLOAD_DIR/ubuntu"
TOOLS_DIR="$DOWNLOAD_DIR/container-tools"
IPTABLES_DIR="$DOWNLOAD_DIR/alpine-iptables"

ALPINE_INITRD="$ALPINE_DIR/initramfs-virt"
AGENT_BIN="$ALPINE_DIR/guest-agent"
UBUNTU_DEB="$UBUNTU_DIR/linux-modules.deb"
OUT="${OUT:-/build/download/ubuntu/initramfs-containerd}"

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

WORK_DIR=/rootfs-work
DEB_WORK=/deb-extract

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR"

cd "$WORK_DIR"

# Base userspace from Alpine initramfs.
gunzip -c "$ALPINE_INITRD" | cpio -idm 2>/dev/null || true

# Busybox applets.
for applet in mount umount mkdir mknod sleep sh insmod \
              uname id cat ls ps echo printf chmod chown \
              kill killall pwd whoami hostname udhcpc ifconfig \
              ip grep ntpd switch_root cp rm mv head mountpoint \
              tar gzip gunzip dirname sync du date awk touch; do
    ln -sf busybox "bin/$applet"
done

# zstd for faster cold-boot cache sync/restore than gzip.
cp /usr/bin/zstd bin/zstd

# Inject guest agent.
cp "$AGENT_BIN" bin/guest-agent
chmod +x bin/guest-agent

# Container tools.
mkdir -p opt/containerd/bin
tar -xzf "$TOOLS_DIR/containerd.tgz" -C opt/containerd/bin --strip-components=1 2>/dev/null || true
tar -xzf "$TOOLS_DIR/nerdctl.tgz" -C opt/containerd/bin 2>/dev/null || true
cp "$TOOLS_DIR/runc" opt/containerd/bin/runc
chmod +x opt/containerd/bin/*
cat > bin/nerdctl <<'NERDCTLEOF'
#!/bin/sh
# Thin wrapper that ensures a per-project CNI bridge exists and is used
# automatically for the requested namespace.
NS="default"
NET=""
NEXT_IS_NS=0
NEXT_IS_NET=0
HAS_NET=0
RUN_IDX=0
i=0
for arg in "$@"; do
    i=$((i + 1))
    if [ "$NEXT_IS_NS" -eq 1 ]; then
        NS="$arg"
        NEXT_IS_NS=0
        continue
    fi
    if [ "$NEXT_IS_NET" -eq 1 ]; then
        NET="$arg"
        NEXT_IS_NET=0
        HAS_NET=1
        continue
    fi
    case "$arg" in
        -n|--namespace) NEXT_IS_NS=1 ;;
        -n=*|--namespace=*) NS="${arg#*=}" ;;
        --net|--network) NEXT_IS_NET=1 ;;
        --net=*|--network=*) NET="${arg#*=}"; HAS_NET=1 ;;
        run) RUN_IDX=$i ;;
    esac
done
# Remove the global default bridge so each project gets its own bridge.
rm -f /etc/cni/net.d/nerdctl-bridge.conflist /etc/cni/net.d/bridge.conflist
# Generate the CNI config for the network that will actually be used.
if [ -z "$NET" ]; then
    NET="$NS"
fi
/bin/guest-agent cni-gen "$NET" >/dev/null 2>&1 || true

if [ "$HAS_NET" -eq 0 ] && [ "$RUN_IDX" -ne 0 ]; then
    shift_count=$RUN_IDX
    head=""
    while [ "$shift_count" -gt 0 ]; do
        head="$head '$1'"
        shift
        shift_count=$((shift_count - 1))
    done
    # head now contains the first RUN_IDX arguments (including "run").
    # Insert --net right after the "run" subcommand.
    eval set -- $head --net "$NS" "$@"
fi
exec /opt/containerd/bin/nerdctl "$@"
NERDCTLEOF
chmod +x bin/nerdctl
ln -sf /opt/containerd/bin/ctr bin/ctr

# CNI plugins for per-project bridge networking.
mkdir -p opt/cni/bin etc/cni/net.d
tar -xzf "$TOOLS_DIR/cni-plugins.tgz" -C opt/cni/bin 2>/dev/null || true
chmod +x opt/cni/bin/*

# iptables + libraries required by CNI plugins.
for apk in iptables libmnl libnftnl libxtables; do
    tar -xf "$IPTABLES_DIR/$apk.apk" -C . 2>/dev/null || true
done

# GNU tar is required by nerdctl cp; busybox tar is not sufficient. The build
# container is Alpine, so install the GNU tar package and copy it (along with
# its libacl/libattr dependencies) into the initramfs.
apk add --no-cache tar >/dev/null 2>&1 || true
if /bin/tar --version 2>/dev/null | grep -q GNU; then
    cp /bin/tar bin/tar-gnu
    chmod +x bin/tar-gnu
    rm -f bin/tar
    cp bin/tar-gnu bin/tar
    mkdir -p lib
    for lib in /lib/libacl.so.1 /lib/libattr.so.1 /usr/lib/libacl.so.1 /usr/lib/libattr.so.1; do
        if [ -f "$lib" ]; then
            cp "$lib" lib/
        fi
    done
fi

# Helpers to persist containerd image cache across cold boots.
cat > bin/anvil-sync-containerd <<'EOF'
#!/bin/sh
# Sync /var/lib/containerd to a tarball on the virtiofs share.
# When containerd root is on a persistent block disk this is a no-op.
ARCHIVE="${1:-/mnt/anvil/containerd-cache.tar.zst}"
SIZE_FILE="${ARCHIVE}.size"

if ! mount | grep -q 'on /var/lib/containerd type tmpfs'; then
    echo "[anvil-sync] containerd on persistent disk, skipping tarball sync"
    exit 0
fi

mkdir -p "$(dirname "$ARCHIVE")"

# If the cache size has not changed since the last sync, skip the expensive
# tar pass. This makes repeated save/resume cycles fast while still
# persisting real changes (image pulls, container creates) on shutdown.
set -- $(du -sb /var/lib/containerd)
CURRENT_SIZE=$1
if [ -f "$ARCHIVE" ] && [ -f "$SIZE_FILE" ] && [ "$CURRENT_SIZE" = "$(cat "$SIZE_FILE")" ]; then
    echo "[anvil-sync] containerd cache unchanged ($CURRENT_SIZE bytes), skipping sync"
    exit 0
fi

rm -f "$ARCHIVE.tmp"
# zstd -1 is faster than gzip -1 for both compression and decompression,
# while still reducing the tarball size. The tarball is only a cold-boot
# cache, not long-term storage.
tar -cf - -C /var/lib/containerd . | /bin/zstd -1 -T0 > "$ARCHIVE.tmp"
mv "$ARCHIVE.tmp" "$ARCHIVE"
echo "$CURRENT_SIZE" > "$SIZE_FILE"
# Flush the virtiofs writeback cache so the tarball is visible on the host.
sync
EOF
chmod +x bin/anvil-sync-containerd

cat > bin/anvil-restore-containerd <<'EOF'
#!/bin/sh
# Restore /var/lib/containerd from a tarball on the virtiofs share.
set -e
ARCHIVE="${1:-/mnt/anvil/containerd-cache.tar.zst}"
[ -f "$ARCHIVE" ] || exit 0
mkdir -p /var/lib/containerd
rm -rf /var/lib/containerd/*
START=$(awk '{printf "%d\n", $1*1000}' /proc/uptime)
/bin/zstd -dc "$ARCHIVE" | tar -xf - -C /var/lib/containerd
END=$(awk '{printf "%d\n", $1*1000}' /proc/uptime)
DURATION=$((END - START))
echo "[anvil-restore] restored containerd cache in ${DURATION}ms"
EOF
chmod +x bin/anvil-restore-containerd

# Ubuntu kernel modules: vsock + virtiofs.
mkdir -p lib/modules/vsock lib/modules/virtiofs lib/modules/overlayfs
rm -rf "$DEB_WORK"
mkdir -p "$DEB_WORK"
cd "$DEB_WORK"
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

# ext4 for the optional persistent containerd block disk.
mkdir -p lib/modules/ext4 lib/modules/jbd2 lib/modules/mbcache lib/modules/crc16
EXT4_SRC="$DEB_WORK/lib/modules/*/kernel/fs/ext4/ext4.ko.zst"
if [[ -f $(echo $EXT4_SRC) ]]; then
    zstd -d -f $(echo $EXT4_SRC) -o lib/modules/ext4/ext4.ko
fi
JBD2_SRC="$DEB_WORK/lib/modules/*/kernel/fs/jbd2/jbd2.ko.zst"
if [[ -f $(echo $JBD2_SRC) ]]; then
    zstd -d -f $(echo $JBD2_SRC) -o lib/modules/jbd2/jbd2.ko
fi
MBCACHE_SRC="$DEB_WORK/lib/modules/*/kernel/fs/mbcache.ko.zst"
if [[ -f $(echo $MBCACHE_SRC) ]]; then
    zstd -d -f $(echo $MBCACHE_SRC) -o lib/modules/mbcache/mbcache.ko
fi
CRC16_SRC="$DEB_WORK/lib/modules/*/kernel/lib/crc16.ko.zst"
if [[ -f $(echo $CRC16_SRC) ]]; then
    zstd -d -f $(echo $CRC16_SRC) -o lib/modules/crc16/crc16.ko
fi

# Networking modules for CNI bridge/veth + nftables for iptables-nft.
mkdir -p lib/modules/llc lib/modules/stp lib/modules/bridge lib/modules/veth lib/modules/br_netfilter lib/modules/nfnetlink lib/modules/nf_tables lib/modules/libcrc32c lib/modules/x_tables lib/modules/nf_defrag_ipv4 lib/modules/nf_defrag_ipv6 lib/modules/nft
LLC_SRC="$DEB_WORK/lib/modules/*/kernel/net/llc/llc.ko.zst"
if [[ -f $(echo $LLC_SRC) ]]; then
    zstd -d -f $(echo $LLC_SRC) -o lib/modules/llc/llc.ko
else
    echo "missing llc module"
    exit 1
fi
STP_SRC="$DEB_WORK/lib/modules/*/kernel/net/802/stp.ko.zst"
if [[ -f $(echo $STP_SRC) ]]; then
    zstd -d -f $(echo $STP_SRC) -o lib/modules/stp/stp.ko
else
    echo "missing stp module"
    exit 1
fi
BRIDGE_SRC="$DEB_WORK/lib/modules/*/kernel/net/bridge/bridge.ko.zst"
if [[ -f $(echo $BRIDGE_SRC) ]]; then
    zstd -d -f $(echo $BRIDGE_SRC) -o lib/modules/bridge/bridge.ko
else
    echo "missing bridge module"
    exit 1
fi
VETH_SRC="$DEB_WORK/lib/modules/*/kernel/drivers/net/veth.ko.zst"
if [[ -f $(echo $VETH_SRC) ]]; then
    zstd -d -f $(echo $VETH_SRC) -o lib/modules/veth/veth.ko
else
    echo "missing veth module"
    exit 1
fi
BRNF_SRC="$DEB_WORK/lib/modules/*/kernel/net/bridge/br_netfilter.ko.zst"
if [[ -f $(echo $BRNF_SRC) ]]; then
    zstd -d -f $(echo $BRNF_SRC) -o lib/modules/br_netfilter/br_netfilter.ko
fi
NFNL_SRC="$DEB_WORK/lib/modules/*/kernel/net/netfilter/nfnetlink.ko.zst"
if [[ -f $(echo $NFNL_SRC) ]]; then
    zstd -d -f $(echo $NFNL_SRC) -o lib/modules/nfnetlink/nfnetlink.ko
fi
NFT_SRC="$DEB_WORK/lib/modules/*/kernel/net/netfilter/nf_tables.ko.zst"
if [[ -f $(echo $NFT_SRC) ]]; then
    zstd -d -f $(echo $NFT_SRC) -o lib/modules/nf_tables/nf_tables.ko
fi

CRC32C_SRC="$DEB_WORK/lib/modules/*/kernel/lib/libcrc32c.ko.zst"
if [[ -f $(echo $CRC32C_SRC) ]]; then
    zstd -d -f $(echo $CRC32C_SRC) -o lib/modules/libcrc32c/libcrc32c.ko
fi

XTABLES_SRC="$DEB_WORK/lib/modules/*/kernel/net/netfilter/x_tables.ko.zst"
if [[ -f $(echo $XTABLES_SRC) ]]; then
    zstd -d -f $(echo $XTABLES_SRC) -o lib/modules/x_tables/x_tables.ko
fi

NF_DEFRAG4_SRC="$DEB_WORK/lib/modules/*/kernel/net/ipv4/netfilter/nf_defrag_ipv4.ko.zst"
if [[ -f $(echo $NF_DEFRAG4_SRC) ]]; then
    zstd -d -f $(echo $NF_DEFRAG4_SRC) -o lib/modules/nf_defrag_ipv4/nf_defrag_ipv4.ko
fi

NF_DEFRAG6_SRC="$DEB_WORK/lib/modules/*/kernel/net/ipv6/netfilter/nf_defrag_ipv6.ko.zst"
if [[ -f $(echo $NF_DEFRAG6_SRC) ]]; then
    zstd -d -f $(echo $NF_DEFRAG6_SRC) -o lib/modules/nf_defrag_ipv6/nf_defrag_ipv6.ko
fi

for nft_mod in nft_compat nft_ct nft_nat nft_chain_nat nft_masq nft_reject; do
    NFT_MOD_SRC="$DEB_WORK/lib/modules/*/kernel/net/netfilter/${nft_mod}.ko.zst"
    if [[ -f $(echo $NFT_MOD_SRC) ]]; then
        zstd -d -f $(echo $NFT_MOD_SRC) -o "lib/modules/nft/${nft_mod}.ko"
    fi
done

# x_tables matches/targets used by CNI bridge/firewall/portmap rules.
mkdir -p lib/modules/netfilter
for xt_mod in xt_comment xt_conntrack xt_addrtype xt_MASQUERADE xt_REDIRECT xt_nat xt_tcpudp xt_multiport xt_mark xt_limit; do
    XT_MOD_SRC="$DEB_WORK/lib/modules/*/kernel/net/netfilter/${xt_mod}.ko.zst"
    if [[ -f $(echo $XT_MOD_SRC) ]]; then
        zstd -d -f $(echo $XT_MOD_SRC) -o "lib/modules/netfilter/${xt_mod}.ko"
    fi
done

# Netfilter modules for iptables-nft NAT/filter rules used by CNI portmap/firewall.
mkdir -p lib/modules/netfilter
for mod in nf_conntrack nf_nat iptable_nat iptable_filter ip_tables nf_conntrack_netlink; do
    src=$(find "$DEB_WORK/lib/modules" -name "$mod.ko.zst" -print -quit)
    if [[ -f "$src" ]]; then
        zstd -d -f "$src" -o "lib/modules/netfilter/$mod.ko"
    fi
done

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
insmod /lib/modules/llc/llc.ko
insmod /lib/modules/stp/stp.ko
insmod /lib/modules/bridge/bridge.ko
insmod /lib/modules/veth/veth.ko

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

# Re-mount virtiofs share if not already moved.
mkdir -p /mnt/anvil
mountpoint -q /mnt/anvil || mount -t virtiofs anvil /mnt/anvil 2>/dev/null || true

# Persistent state: mount the virtio-blk disk (or virtiofs share) over /var/lib
# so both containerd root (/var/lib/containerd) and nerdctl metadata/volumes
# (/var/lib/nerdctl) survive reboots and resume, instead of filling tmpfs root.
mkdir -p /var/lib
mountpoint -q /var/lib || {
    # Load ext4 modules for the optional persistent block disk.
    insmod /lib/modules/crc16/crc16.ko 2>/dev/null || true
    insmod /lib/modules/mbcache/mbcache.ko 2>/dev/null || true
    insmod /lib/modules/jbd2/jbd2.ko 2>/dev/null || true
    insmod /lib/modules/ext4/ext4.ko 2>/dev/null || true
    # The first virtio-blk device is exposed as /dev/vda inside the VM.
    for blk in /dev/vda /dev/vdb /dev/vdc; do
        if [ -b "$blk" ]; then
            if mount -t ext4 -o defaults "$blk" /var/lib 2>/dev/null; then
                echo "[stage2] mounted $blk as /var/lib"
                break
            fi
        fi
    done
}

# If no block disk is available, persist on the virtiofs host share.
if ! mountpoint -q /var/lib; then
    mkdir -p /mnt/anvil/var-lib

    # One-time migration: if an old tarball cache exists and the persisted root is
    # empty, extract the tarball into the virtiofs directory.
    if [ -f /mnt/anvil/containerd-cache.tar.zst ] && [ -z "$(ls -A /mnt/anvil/var-lib 2>/dev/null)" ]; then
        echo "[stage2] migrating containerd cache from tarball to virtiofs directory"
        mkdir -p /mnt/anvil/var-lib/containerd
        /bin/zstd -dc /mnt/anvil/containerd-cache.tar.zst | tar -xf - -C /mnt/anvil/var-lib/containerd
    fi

    mount --bind /mnt/anvil/var-lib /var/lib
    echo "[stage2] /var/lib bound to virtiofs share"
fi

mkdir -p /var/lib/containerd /var/lib/nerdctl /var/lib/cni

# Load netfilter modules so CNI bridge/portmap/firewall and iptables-nft work.
# Order matters: load dependencies before dependents.
insmod /lib/modules/libcrc32c/libcrc32c.ko
insmod /lib/modules/x_tables/x_tables.ko
insmod /lib/modules/nfnetlink/nfnetlink.ko
insmod /lib/modules/nf_defrag_ipv4/nf_defrag_ipv4.ko
insmod /lib/modules/nf_defrag_ipv6/nf_defrag_ipv6.ko
insmod /lib/modules/netfilter/nf_conntrack.ko
insmod /lib/modules/netfilter/nf_nat.ko
insmod /lib/modules/nf_tables/nf_tables.ko
insmod /lib/modules/nft/nft_compat.ko 2>/dev/null || true
insmod /lib/modules/nft/nft_ct.ko 2>/dev/null || true
insmod /lib/modules/nft/nft_nat.ko 2>/dev/null || true
insmod /lib/modules/nft/nft_chain_nat.ko 2>/dev/null || true
insmod /lib/modules/nft/nft_masq.ko 2>/dev/null || true
insmod /lib/modules/nft/nft_reject.ko 2>/dev/null || true
insmod /lib/modules/netfilter/ip_tables.ko
insmod /lib/modules/netfilter/iptable_filter.ko
insmod /lib/modules/netfilter/iptable_nat.ko
insmod /lib/modules/netfilter/nf_conntrack_netlink.ko
insmod /lib/modules/netfilter/xt_comment.ko 2>/dev/null || true
insmod /lib/modules/netfilter/xt_conntrack.ko 2>/dev/null || true
insmod /lib/modules/netfilter/xt_addrtype.ko 2>/dev/null || true
insmod /lib/modules/netfilter/xt_tcpudp.ko 2>/dev/null || true
insmod /lib/modules/netfilter/xt_multiport.ko 2>/dev/null || true
insmod /lib/modules/netfilter/xt_mark.ko 2>/dev/null || true
insmod /lib/modules/netfilter/xt_limit.ko 2>/dev/null || true
insmod /lib/modules/netfilter/xt_nat.ko 2>/dev/null || true
insmod /lib/modules/netfilter/xt_REDIRECT.ko 2>/dev/null || true
insmod /lib/modules/netfilter/xt_MASQUERADE.ko 2>/dev/null || true
insmod /lib/modules/br_netfilter/br_netfilter.ko 2>/dev/null || true

# Ensure mount propagation is private so runc can pivot_root into containers.
mount --make-rprivate /

# CNI's portmap plugin DNATs host ports to container IPs. When the original
# source is on the same L3 segment as the guest (VZ NAT on the macOS host),
# reply packets from the container bypass the DNAT host and the host drops
# them. Masquerade DNATed TCP traffic so replies always return through the
# guest, making localhost:PORT forwarding from the host work.
iptables -t nat -C POSTROUTING -p tcp -m conntrack --ctstate DNAT -j MASQUERADE -m comment --comment "anvil-dnat-masq" 2>/dev/null || \
iptables -t nat -A POSTROUTING -p tcp -m conntrack --ctstate DNAT -j MASQUERADE -m comment --comment "anvil-dnat-masq"

# Start containerd in background.
echo "[stage2] starting containerd"
/opt/containerd/bin/containerd > /tmp/containerd.log 2>&1 &

# Wait for containerd socket.
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    if [ -S /run/containerd/containerd.sock ]; then break; fi
    sleep 0.5
done

# Cold boot only: per-container nerdctl state (/var/lib/nerdctl) is on tmpfs and
# is not persisted across boots. Restored containerd metadata would reference
# missing state files (resolv.conf, hostname, etc.), causing those containers to
# fail on start. Drop all containers on cold boot so the VM starts with a clean
# slate; images and snapshots survive in the persisted cache.
# Use the low-level `ctr` client instead of `nerdctl rm` so a stuck OCI hook or
# shim cannot block the whole boot process.
echo "[stage2] cleaning up containers from restored metadata"

# Old shims/hooks from a crashed/hung previous session cannot be talked to;
# trying to delete their tasks through containerd would hang. Kill them first.
killall -9 containerd-shim-runc-v2 2>/dev/null || true
killall -9 runc 2>/dev/null || true
killall -9 nerdctl 2>/dev/null || true
sleep 1

for ns in $(/opt/containerd/bin/ctr namespace ls -q 2>/dev/null); do
    for id in $(/opt/containerd/bin/ctr -n "$ns" c ls -q 2>/dev/null); do
        /opt/containerd/bin/ctr -n "$ns" t rm -f "$id" >/dev/null 2>&1 || true
        /opt/containerd/bin/ctr -n "$ns" c rm "$id" >/dev/null 2>&1 || true
    done
done

# Start guest agent in foreground.
echo "[stage2] starting guest-agent"
if [ -f /mnt/anvil/.anvil-debug ]; then
    echo "[stage2] enabling guest-agent debug mode"
    export ANVIL_DEBUG=1
    # Stream guest-agent stdout/stderr to the virtiofs share so debug logs are
    # inspectable on the host without entering the VM.
    exec /bin/guest-agent >>/mnt/anvil/guest-agent.log 2>&1
fi
exec /bin/guest-agent
STAGE2
chmod +x /newroot/stage2.sh
exec switch_root /newroot /bin/sh /stage2.sh
EOF
chmod +x myinit

# Pack the rootfs with GNU cpio inside the Linux container.
find . -mindepth 1 -print0 | cpio -o -0 -H newc | gzip -9 > "$OUT"

if [[ -n "${HOST_UID:-}" && -n "${HOST_GID:-}" ]]; then
    chown "$HOST_UID:$HOST_GID" "$OUT" 2>/dev/null || true
fi

echo "created $OUT"
