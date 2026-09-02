#!/bin/bash
set -euo pipefail

# Build a minimal initramfs with containerd + runc (no nerdctl: the guest-agent talks to containerd directly).
# The whole rootfs tree is assembled inside a Linux container so that
# case-differing filenames (e.g. libxt_MARK.so vs libxt_mark.so) survive
# and the final cpio archive is produced by GNU cpio instead of a custom
# packer. Only the resulting .cpio.gz is written back to the host.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# If we are not already inside a Linux build environment, re-run this script
# in a docker container.
if [[ -z "${IN_CONTAINER:-}" ]]; then
    if [[ "$(uname -s)" == "Linux" ]]; then
        # Already on Linux (e.g. the Lima build VM): all required tools are
        # present. Build directly — no nested docker, no network needed.
        export IN_CONTAINER=1
        export BUILD_DIR="$ROOT/.download"
        # The body runs as a regular user here (not root as in the docker
        # build container), so use a writable scratch area.
        ANVIL_WORK_BASE="$(mktemp -d)"
        export ANVIL_WORK_DIR="$ANVIL_WORK_BASE/rootfs-work"
        export ANVIL_DEB_WORK="$ANVIL_WORK_BASE/deb-extract"
    else
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
            sh -c 'apk add --no-cache bash cpio zstd binutils upx && exec bash /scripts/build_initramfs_containerd.sh'
        exit 0
    fi
fi

# ---------------------------------------------------------------------------
# Everything below runs inside the Linux build container.
# ---------------------------------------------------------------------------

set -euo pipefail

# Install only missing tools; the Lima build VM usually has them already, and
# `apk add` needs network access to the Alpine CDN even when it does.
need_pkgs=""
command -v cpio >/dev/null 2>&1 || need_pkgs="$need_pkgs cpio"
command -v zstd >/dev/null 2>&1 || need_pkgs="$need_pkgs zstd"
command -v objdump >/dev/null 2>&1 || need_pkgs="$need_pkgs binutils"
if [[ -n "$need_pkgs" ]]; then
    apk add --no-cache $need_pkgs
fi

# BUILD_DIR points at the shared download directory: /build/download inside
# the docker build container, the project .download when building directly
# on Linux (Lima VM).
DOWNLOAD_DIR="${BUILD_DIR:-/build/download}"
SCRIPT_DIR="${SCRIPT_DIR:-/scripts}"
ALPINE_DIR="$DOWNLOAD_DIR/alpine"
UBUNTU_DIR="$DOWNLOAD_DIR/ubuntu"
TOOLS_DIR="$DOWNLOAD_DIR/container-tools"
IPTABLES_DIR="$DOWNLOAD_DIR/alpine-iptables"

ALPINE_INITRD="$ALPINE_DIR/initramfs-virt"
AGENT_BIN="$ALPINE_DIR/guest-agent"
VIRT_APK="$ALPINE_DIR/linux-virt.apk"
OUT="${OUT:-$DOWNLOAD_DIR/ubuntu/initramfs-containerd}"

if [[ ! -f "$ALPINE_INITRD" ]]; then
    echo "Alpine initramfs not found; run 'make download-alpine' first"
    exit 1
fi
if [[ ! -f "$AGENT_BIN" ]]; then
    echo "guest-agent not found; run 'make guest-agent' first"
    exit 1
fi
if [[ ! -d "$ALPINE_DIR/linux-virt-pkg/lib/modules" ]]; then
    echo "Alpine linux-virt modules not extracted at $ALPINE_DIR/linux-virt-pkg"
    echo "run 'make alpine-virt-modules' first"
    exit 1
fi
if [[ ! -d "$TOOLS_DIR" ]]; then
    echo "container tools not found at $TOOLS_DIR"
    exit 1
fi

WORK_DIR="${ANVIL_WORK_DIR:-/rootfs-work}"
DEB_WORK="${ANVIL_DEB_WORK:-/deb-extract}"

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR"

cd "$WORK_DIR"

# Base userspace from Alpine initramfs.
gunzip -c "$ALPINE_INITRD" | cpio -idm 2>/dev/null || true
# The netboot initramfs bundles its own (older) lib/modules tree for the
# kernel it was built for; ours comes from the linux-virt apk below. Drop
# the stale tree so modprobe can never resolve into it.
rm -rf lib/modules/*

# Busybox applets.
for applet in mount umount mkdir mknod sleep sh insmod modprobe \
              uname id cat ls ps echo printf chmod chown \
              kill killall pwd whoami hostname udhcpc ifconfig \
              ip grep ntpd switch_root cp rm mv head mountpoint \
              tar gzip gunzip dirname sync du date awk touch find fstrim ln; do
    ln -sf busybox "bin/$applet"
done

# zstd for faster cold-boot cache sync/restore than gzip.
cp /usr/bin/zstd bin/zstd

# mkfs.ext4/resize2fs come from the Alpine e2fsprogs apks (musl-linked),
# extracted below together with iptables — a glibc mkfs.ext4 copied from the
# build VM would not run on the musl rootfs.

# Inject guest agent.
cp "$AGENT_BIN" bin/guest-agent
chmod +x bin/guest-agent

# Container tools.
mkdir -p opt/containerd/bin
tar -xzf "$TOOLS_DIR/containerd.tgz" -C opt/containerd/bin --strip-components=1 2>/dev/null || true
cp "$TOOLS_DIR/runc" opt/containerd/bin/runc
# buildkitd + buildctl for `docker build` (the agent calls buildkitd via gRPC). These
# build-only binaries (~150 MB unpacked, the largest part of the initramfs)
# are packed as ONE compressed tarball in /opt instead of loose rootfs
# files: loose files ride through kernel initramfs decompress AND the tmpfs
# root copy on every boot; the tarball is extracted to the persistent
# /var/lib/buildkit once (stage2, background) and symlinked into place.
BUILDKIT_STAGE="$(mktemp -d)"
tar -xzf "$TOOLS_DIR/buildkit.tgz" -C "$BUILDKIT_STAGE" --strip-components=1 2>/dev/null || true
# Drop unused containerd tools (~19 MB): stress tester and rootless helpers.
rm -f opt/containerd/bin/containerd-stress opt/containerd/bin/containerd-rootless*.sh
# qemu emulators are only needed for cross-platform `docker build
# --platform`; keep x86_64 (and 32-bit arm) on an arm64 VM, drop the rest.
# buildkit's bundled runc duplicates the system one — stage2 symlinks it.
for q in "$BUILDKIT_STAGE"/buildkit-qemu-*; do
    case "$(basename "$q")" in
        buildkit-qemu-x86_64|buildkit-qemu-arm) ;;
        *) rm -f "$q" ;;
    esac
done
rm -f "$BUILDKIT_STAGE"/buildkit-runc

# buildkitd runs with the containerd worker (same image store as docker
# pull) so `FROM` resolves local images instead of hitting the registry
# (an unreachable registry means OAuth token requests and a failed build
# even when every base image is present). buildkitd reads this file from
# its default path /etc/buildkit/buildkitd.toml.
mkdir -p etc/buildkit
cat > etc/buildkit/buildkitd.toml <<'BKTEOF'
[worker.oci]
  enabled = false

[worker.containerd]
  enabled = true
  namespace = "default"

# BuildKit resolves registry auth endpoints itself; its embedded resolver
# intermittently times out on auth.docker.io behind the VZ NAT DNS forwarder
# while every other process in the VM resolves fine. Point it at public
# resolvers directly (reachable regardless of which network the host is on,
# same fallback the pre-lease resolv.conf uses).
[dns]
  nameservers = ["8.8.8.8", "8.8.4.4", "1.1.1.1"]
BKTEOF
# UPX lives in PATH in CI (apk add upx); the offline Lima build VM has no
# package network access, so fall back to the prebuilt static binary fetched
# into .download/upx (make download-upx).
UPX_BIN="$(command -v upx || true)"
if [[ -z "$UPX_BIN" && -x "$DOWNLOAD_DIR/upx/upx" ]]; then
    UPX_BIN="$DOWNLOAD_DIR/upx/upx"
fi
if [[ -n "$UPX_BIN" ]]; then
    # -9, not --best: --best selects LZMA, which takes tens of minutes on
    # 70+ MB binaries under the QEMU-emulated CI build container.
    # They run once per build, so exec-time decompression is irrelevant —
    # unlike the hot-path containerd/runc/shim, which stay unpacked.
    "$UPX_BIN" -q -9 "$BUILDKIT_STAGE"/buildkitd "$BUILDKIT_STAGE"/buildctl \
        "$BUILDKIT_STAGE"/buildkit-qemu-* 2>/dev/null || \
        echo "WARNING: upx packing failed, shipping unpacked buildkit binaries" >&2
else
    echo "WARNING: upx not found, shipping unpacked buildkit binaries" >&2
fi
# Single tarball in /opt: stage2 extracts it to /var/lib/buildkit once.
mkdir -p opt
tar -C "$BUILDKIT_STAGE" -cf - buildkitd buildctl buildkit-qemu-x86_64 buildkit-qemu-arm \
    | zstd -q -o opt/buildkit.tar.zst
rm -rf "$BUILDKIT_STAGE"
ln -sf /opt/containerd/bin/ctr bin/ctr
ln -sf /opt/containerd/bin/buildctl bin/buildctl

# CNI plugins for per-project bridge networking.
mkdir -p opt/cni/bin etc/cni/net.d
# The tgz is read from the host share (virtiofs/sshfs in Lima), where a read
# hiccup previously produced zero-byte plugins. Extract, verify, retry once,
# then fail loudly instead of shipping a broken initramfs.
for attempt in 1 2; do
    tar -xzf "$TOOLS_DIR/cni-plugins.tgz" -C opt/cni/bin 2>/dev/null || true
    broken=0
    for plugin in bridge host-local loopback portmap firewall tuning; do
        [[ -s "opt/cni/bin/$plugin" ]] || broken=1
    done
    [[ "$broken" == 0 ]] && break
    echo "WARNING: CNI plugin extraction incomplete (attempt $attempt), retrying" >&2
done
for plugin in bridge host-local loopback portmap firewall tuning; do
    if [[ ! -s "opt/cni/bin/$plugin" ]]; then
        echo "ERROR: CNI plugin $plugin missing or empty after extraction" >&2
        exit 1
    fi
done
chmod +x opt/cni/bin/*
# Keep only the plugins the generated configs actually use; the full set is ~78 MB.
for plugin in opt/cni/bin/*; do
    case "$(basename "$plugin")" in
        bridge|host-local|loopback|portmap|firewall|tuning) ;;
        *) rm -f "$plugin" ;;
    esac
done
# buildkit ships its own copies of the same CNI plugins (buildkit-cni-*);
# replace them with symlinks to the system plugins extracted above.
for p in opt/containerd/bin/buildkit-cni-*; do
    [ -e "$p" ] || continue
    cni_name="$(basename "$p")"
    cni_name="${cni_name#buildkit-cni-}"
    if [[ -s "opt/cni/bin/$cni_name" ]]; then
        rm -f "$p"
        ln -sf "/opt/cni/bin/$cni_name" "$p"
    fi
done

# iptables + libraries required by CNI plugins; GNU tar for docker cp and
# /build context handling; e2fsprogs for the containerd disk (mkfs.ext4 on first boot,
# resize2fs when the disk image has grown).
for apk in iptables libmnl libnftnl libxtables tar libacl libattr \
           e2fsprogs e2fsprogs-extra e2fsprogs-libs libcom_err libblkid libuuid; do
    tar -xf "$IPTABLES_DIR/$apk.apk" -C . 2>/dev/null || true
done

if [[ ! -x sbin/mkfs.ext4 ]]; then
    echo "ERROR: mkfs.ext4 not present in initramfs" >&2
    exit 1
fi
if [[ ! -x usr/sbin/resize2fs ]]; then
    echo "ERROR: resize2fs not present in initramfs" >&2
    exit 1
fi

# GNU tar is required by docker cp and /build; busybox tar is not sufficient.
# The Alpine tar/acl/attr apks (musl-linked) extracted above put GNU tar at
# bin/tar and its libs under usr/lib — no build-VM tar is used (a glibc-linked
# one would not run on the musl rootfs).
if [[ ! -x bin/tar ]]; then
    echo "ERROR: GNU tar not present in initramfs" >&2
    exit 1
fi
cp bin/tar bin/tar-gnu
chmod +x bin/tar-gnu

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
# Alpine linux-virt kernel modules (.ko.gz), pre-extracted by
# `make alpine-virt-modules` into $ALPINE_DIR/linux-virt-pkg. Modules are
# placed at their original paths under lib/modules/<kver>/ and loaded with
# modprobe, which resolves dependency chains (fuse, crc32c, virtio, ...)
# via modules.dep — explicit insmod lists kept breaking on missing deps.
APK_MODDIR="$(echo $ALPINE_DIR/linux-virt-pkg/lib/modules/*/)"
MODKVER="$(basename "$APK_MODDIR")"

# modinfo metadata required by modprobe.
for meta in modules.dep modules.dep.bin modules.alias modules.alias.bin \
            modules.builtin modules.builtin.bin modules.symbols modules.symbols.bin; do
    if [[ -f "$APK_MODDIR/$meta" ]]; then
        mkdir -p "lib/modules/$MODKVER"
        cp "$APK_MODDIR/$meta" "lib/modules/$MODKVER/"
    fi
done

# putmod <module> [required] — decompress a module from the apk to its
# original lib/modules/<kver>/... path; exit on missing if "required".
putmod() {
    local mod="$1" required="${2:-}"
    local src dst
    src=$(find "$APK_MODDIR" -name "$mod.ko.gz" -print -quit)
    if [[ -f "$src" ]]; then
        dst="lib/modules/$MODKVER/${src#"$APK_MODDIR"}"
        mkdir -p "$(dirname "$dst")"
        gunzip -c "$src" > "$dst"
    elif [[ -n "$required" ]]; then
        echo "missing module: $mod"
        exit 1
    fi
}

# vsock control channel + virtiofs share (+ fuse) + overlayfs.
putmod vsock required
putmod vmw_vsock_virtio_transport_common required
putmod vmw_vsock_virtio_transport required
putmod virtiofs required
putmod fuse required
putmod overlay required

# virtio devices: network and block disk.
putmod virtio_net required
putmod virtio_blk required
putmod failover
putmod net_failover

# AF_PACKET sockets (CONFIG_PACKET=m in linux-virt) — required by udhcpc.
putmod af_packet required

# CRC32C chain: needed by both ext4 (metadata_csum) and nf_conntrack.
# busybox modprobe does not resolve the libcrc32c->crc32c softdep, so
# crc32c_generic is loaded explicitly in stage2.
putmod crc32c_generic

# ext4 for the optional persistent containerd block disk.
putmod ext4
putmod jbd2
putmod mbcache
putmod crc16

# Networking modules for CNI bridge/veth.
putmod llc required
putmod stp required
putmod bridge required
putmod veth required
putmod br_netfilter

# nftables/iptables modules for CNI bridge/firewall/portmap rules.
putmod nfnetlink
putmod nf_tables
putmod libcrc32c
putmod crc32c
putmod crc32c_intel
putmod x_tables
putmod nf_defrag_ipv4
putmod nf_defrag_ipv6
putmod nf_conntrack required
putmod nf_nat required
for nft_mod in nft_compat nft_ct nft_nat nft_chain_nat nft_masq nft_reject; do
    putmod "$nft_mod"
done
for xt_mod in xt_comment xt_conntrack xt_addrtype xt_MASQUERADE xt_REDIRECT xt_nat xt_tcpudp xt_multiport xt_mark xt_limit; do
    putmod "$xt_mod"
done
for nf_mod in iptable_nat iptable_filter ip_tables nf_conntrack_netlink; do
    putmod "$nf_mod"
done

# Init script.
cat > myinit <<'EOF'
#!/bin/sh
export PATH=/bin:/sbin:/usr/bin:/usr/sbin

# Boot-phase markers (seconds since kernel start, from /proc/uptime) —
# land on the hvc0 console -> ~/.anvil-vz/console.log, parsed by
# scripts/time_boot.py. Pure-shell: busybox in the initramfs has no `cut`
# applet, and `read` avoids even the cat fork.
bmark() { read -r up _ < /proc/uptime; echo "[boot] $1 up=$up"; }
bmark myinit_start

mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
mkdir -p /tmp /var/log /sys/fs/cgroup
mount -t cgroup2 cgroup2 /sys/fs/cgroup
bmark mounts

# Load modules (modprobe resolves dependency chains via modules.dep).
for m in vmw_vsock_virtio_transport af_packet virtio_net virtiofs overlay llc stp bridge veth; do
    modprobe $m 2>/dev/null || echo "[myinit] modprobe $m failed"
done
bmark modprobe

for i in 1 2 3 4 5 6 7 8 9 10; do
    if [ -e /dev/vsock ]; then break; fi
    sleep 0.2
done
bmark vsock_dev

mkdir -p /mnt/anvil
mount -t virtiofs anvil /mnt/anvil 2>/dev/null || true
bmark virtiofs

# Set the clock from the host-written time file: VZ does not guarantee a sane
# RTC (boots can start at 1970-01-01) and TLS to registries fails then.
if [ -s /mnt/anvil/.anvil-host-time ]; then
    date -s @"$(cat /mnt/anvil/.anvil-host-time)" 2>/dev/null || true
fi

# Seed the kernel entropy pool from the host: the virt kernel has no
# RANDOM_TRUST_CPU and VZ provides no virtio-rng, so crng init otherwise
# takes ~10 s and stalls containerd/guest-agent start.
/bin/guest-agent seed-entropy /mnt/anvil/.anvil-host-entropy 2>/dev/null || true
bmark entropy

# Bring up the loopback interface; the eth0 DHCP lease is obtained in stage2
# off the boot critical path (nothing before the first container needs it,
# and udhcpc used to block myinit for ~250-300 ms). Pre-seed the resolver
# files so the tmpfs root copy carries them; stage2's udhcpc rewrites
# resolv.conf from the lease when it arrives.
echo "[myinit] configuring network"
ifconfig lo up 2>/dev/null || true
mkdir -p /etc
# Public-DNS fallback for the pre-lease window; after the lease arrives the
# NAT gateway's forwarder takes over (it also works on VPN/restricted
# networks). Serialize A/AAAA lookups: under load the VZ NAT DNS forwarder
# sporadically drops one of the two parallel UDP queries, which Go resolvers
# (image pulls, buildkitd) surface as "lookup ... i/o timeout" after exhausting
# retries.
printf 'nameserver 8.8.8.8\nnameserver 8.8.4.4\noptions single-request timeout:1 attempts:3\n' > /etc/resolv.conf
# A base /etc/hosts is required for --net host containers (the agent bind-mounts its own hosts
# generator opens the host file directly; without it `--network host`
# containers fail with "filesystem error: open /etc/hosts").
printf '127.0.0.1\tlocalhost\n::1\t\tlocalhost\n' > /etc/hosts
# Minimal udhcpc event script: busybox's default.script lives under
# /usr/share, which is NOT copied into the tmpfs root, so stage2's background
# udhcpc would obtain a lease without ever applying it to eth0. /etc is
# copied, so the script lives here.
cat > /etc/udhcpc-script <<'UDHEOF'
#!/bin/sh
case "$1" in
bound|renew)
    ifconfig "$interface" "$ip" netmask "${subnet:-255.255.255.0}" ${broadcast:+broadcast $broadcast} up
    # busybox in this initramfs has no `route` applet; `ip route` is present.
    for r in $router; do ip route replace default via "$r" dev "$interface"; done
    # Public resolvers FIRST, the NAT gateway's forwarder LAST: Go's pure
    # resolver ignores "single-request" and sends A/AAAA in parallel, and the
    # VZ NAT DNS forwarder sporadically drops one of the two — with
    # attempts:2 the query is then retried on the next (public) server
    # instead of exhausting every attempt on the broken forwarder. The lease
    # DNS stays reachable for VPN/restricted networks where public UDP 53 is
    # blocked.
    : > /etc/resolv.conf
    echo "nameserver 8.8.8.8" > /etc/resolv.conf
    echo "nameserver 1.1.1.1" >> /etc/resolv.conf
    for d in $dns; do echo "nameserver $d" >> /etc/resolv.conf; done
    echo "options timeout:1 attempts:2" >> /etc/resolv.conf
    ;;
deconfig)
    ifconfig "$interface" 0.0.0.0 up
    ;;
esac
exit 0
UDHEOF
chmod +x /etc/udhcpc-script
bmark dhcp

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
bmark tmpfs_copy
mkdir -p /newroot/proc /newroot/sys /newroot/dev /newroot/run /newroot/tmp /newroot/var /newroot/mnt
mount --make-rprivate /
mount --move /proc /newroot/proc
mount --move /sys  /newroot/sys
mount --move /dev  /newroot/dev
mount --move /mnt/anvil /newroot/mnt/anvil 2>/dev/null || true
mount --move /var/lib/containerd /newroot/var/lib/containerd 2>/dev/null || true

cat > /newroot/stage2.sh <<'STAGE2'
export PATH=/bin:/sbin:/usr/bin:/usr/sbin

bmark() { read -r up _ < /proc/uptime; echo "[boot] $1 up=$up"; }
bmark stage2_start

# Background drift correction only; the clock is already set from the host
# time file in myinit. Must run in stage2: processes started in myinit do
# not survive switch_root.
( ntpd -nq -p pool.ntp.org >/dev/null 2>&1 || true ) &

# Obtain the eth0 DHCP lease off the boot critical path: nothing until the
# first container (CNI) or an outbound connection needs it. The VZ NAT
# DHCP server occasionally drops the first discover, so retry the whole
# client a few times before giving up (and say so on the console).
( ifconfig eth0 up 2>/dev/null || true
  lease=1
  for attempt in 1 2 3 4 5; do
    if udhcpc -i eth0 -n -q -t 3 -T 2 -s /etc/udhcpc-script >>/tmp/udhcpc.log 2>&1; then
      lease=0
      break
    fi
    echo "[stage2] udhcpc attempt $attempt failed, retrying" >>/tmp/udhcpc.log
    sleep 1
  done
  [ "$lease" = 0 ] || echo "[stage2] WARNING: no DHCP lease after 5 attempts" >/dev/console
  ip addr show eth0 >/tmp/network.log 2>&1 || true
) &

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

# Host /Users tree at the same absolute path, so docker -v /Users/...:...
# bind mounts work unchanged (like Docker Desktop / Lima). Absent when the
# host disabled it (ANVIL_SHARE_USERS=0).
mkdir -p /Users
mountpoint -q /Users || mount -t virtiofs macusers /Users 2>/dev/null || true

# Persistent state: mount the virtio-blk disk (or virtiofs share) over /var/lib
# so both containerd root (/var/lib/containerd) and anvil state (/var/lib/anvil)
# survive reboots and resume, instead of filling tmpfs root.
mkdir -p /var/lib
mountpoint -q /var/lib || {
    # Load virtio-blk and ext4 for the optional persistent block disk.
    modprobe virtio_blk 2>/dev/null || true
    modprobe ext4 2>/dev/null || true
    # The first virtio-blk device is exposed as /dev/vda inside the VM.
    for blk in /dev/vda /dev/vdb /dev/vdc; do
        if [ -b "$blk" ]; then
            # noatime + writeback data + infrequent journal commit + no barrier flush
            # for a dev VM where snapshot save/resume provides durability.
            if mount -t ext4 -o noatime,nobarrier,data=writeback,commit=60 "$blk" /var/lib 2>/dev/null; then
                echo "[stage2] mounted $blk as /var/lib"
                # Grow the fs if the host-side disk image was enlarged since
                # the last boot (online resize; a no-op when sizes match).
                /usr/sbin/resize2fs "$blk" >/dev/null 2>&1 || true
                break
            fi
            # First boot: format the raw disk as ext4.
            if command -v mkfs.ext4 >/dev/null 2>&1; then
                echo "[stage2] formatting $blk as ext4"
                mkfs.ext4 -F -q "$blk" 2>/dev/null && \
                mount -t ext4 -o noatime,nobarrier,data=writeback,commit=60 "$blk" /var/lib 2>/dev/null && \
                echo "[stage2] mounted $blk as /var/lib (fresh ext4)" && break
            fi
        fi
    done
}

# If no block disk is available, persist on the virtiofs host share.
if mountpoint -q /var/lib; then
    # Tune the virtio-blk device for low-latency metadata-heavy workload.
    # The host-side APFS/virtio stack already batches requests, so disable
    # guest-level merging/scheduler overhead and keep queues short.
    for dev in vda vdb vdc; do
        if [ -b "/dev/$dev" ]; then
            echo none > /sys/block/$dev/queue/scheduler 2>/dev/null || true
            echo 256 > /sys/block/$dev/queue/read_ahead_kb 2>/dev/null || true
            echo 256 > /sys/block/$dev/queue/nr_requests 2>/dev/null || true
            echo 2 > /sys/block/$dev/queue/nomerges 2>/dev/null || true
            echo "[stage2] tuned /dev/$dev"
        fi
    done
fi

if ! mountpoint -q /var/lib; then
    mkdir -p /mnt/anvil/.anvil-run/var-lib

    # One-time migration: if an old tarball cache exists and the persisted root is
    # empty, extract the tarball into the virtiofs directory.
    if [ -f /mnt/anvil/containerd-cache.tar.zst ] && [ -z "$(ls -A /mnt/anvil/.anvil-run/var-lib 2>/dev/null)" ]; then
        echo "[stage2] migrating containerd cache from tarball to virtiofs directory"
        mkdir -p /mnt/anvil/.anvil-run/var-lib/containerd
        /bin/zstd -dc /mnt/anvil/containerd-cache.tar.zst | tar -xf - -C /mnt/anvil/.anvil-run/var-lib/containerd
    fi

    # One-time migration from the pre-.anvil-run layout: adopt an existing
    # persisted root instead of stranding it on the share.
    if [ -z "$(ls -A /mnt/anvil/.anvil-run/var-lib 2>/dev/null)" ] && [ -n "$(ls -A /mnt/anvil/var-lib 2>/dev/null)" ]; then
        echo "[stage2] migrating /mnt/anvil/var-lib into /mnt/anvil/.anvil-run/var-lib"
        mv /mnt/anvil/var-lib/* /mnt/anvil/.anvil-run/var-lib/ 2>/dev/null || true
    fi

    mount --bind /mnt/anvil/.anvil-run/var-lib /var/lib
    echo "[stage2] /var/lib bound to virtiofs share"
fi
bmark varlib_mount

mkdir -p /var/lib/containerd /var/lib/anvil /var/lib/cni

# Build-only binaries (buildkitd/buildctl/qemu) ship as /opt/buildkit.tar.zst
# and live on the persistent disk: unpack once in the background (they are
# needed only on the first `docker build`) and symlink into the PATH location
# guest-agent starts buildkitd from.
( if [ ! -x /var/lib/buildkit/bin/buildkitd ] && [ -f /opt/buildkit.tar.zst ]; then
      mkdir -p /var/lib/buildkit/bin
      /bin/zstd -dc /opt/buildkit.tar.zst | tar -x -C /var/lib/buildkit/bin
  fi
  ln -sf /var/lib/buildkit/bin/buildkitd            /opt/containerd/bin/buildkitd
  ln -sf /var/lib/buildkit/bin/buildctl             /opt/containerd/bin/buildctl
  ln -sf /var/lib/buildkit/bin/buildkit-qemu-x86_64 /opt/containerd/bin/buildkit-qemu-x86_64
  ln -sf /var/lib/buildkit/bin/buildkit-qemu-arm    /opt/containerd/bin/buildkit-qemu-arm
  ln -sf /opt/containerd/bin/runc                   /opt/containerd/bin/buildkit-runc
) &

# Load netfilter modules so CNI bridge/portmap/firewall and iptables-nft work.
# modprobe resolves dependency order itself; crc32c_generic must come first
# (busybox modprobe misses the libcrc32c->crc32c softdep).
for m in crc32c_generic nf_conntrack nf_nat nf_tables nft_compat nft_ct nft_nat nft_chain_nat \
         nft_masq nft_reject ip_tables iptable_filter iptable_nat nf_conntrack_netlink \
         xt_comment xt_conntrack xt_addrtype xt_tcpudp xt_multiport xt_mark xt_limit \
         xt_nat xt_REDIRECT xt_MASQUERADE br_netfilter; do
    modprobe $m 2>/dev/null || echo "[stage2] modprobe $m failed"
done

# Ensure mount propagation is private so runc can pivot_root into containers.
mount --make-rprivate /

# CNI's portmap plugin DNATs host ports to container IPs. When the original
# source is on the same L3 segment as the guest (VZ NAT on the macOS host),
# reply packets from the container bypass the DNAT host and the host drops
# them. Masquerade DNATed TCP traffic so replies always return through the
# guest, making localhost:PORT forwarding from the host work.
iptables -t nat -C POSTROUTING -p tcp -m conntrack --ctstate DNAT -j MASQUERADE -m comment --comment "anvil-dnat-masq" 2>/dev/null || \
iptables -t nat -A POSTROUTING -p tcp -m conntrack --ctstate DNAT -j MASQUERADE -m comment --comment "anvil-dnat-masq"
bmark netfilter

# Start containerd in background.
echo "[stage2] starting containerd"
/opt/containerd/bin/containerd > /tmp/containerd.log 2>&1 &
bmark containerd_started

# buildkitd is started lazily by guest-agent on the first build request
# (classic /build or the vsock:1026 buildx bridge) — it idles at ~50 MB RSS,
# so booting it here would tax every user, including those who never build.

# The containerd-socket wait and the cold-boot stale-container cleanup used
# to run here and gated the guest-agent. Both now run inside guest-agent
# (runBootFinalize) after the control channel is up; the Docker API server
# there waits for them, so container operations cannot race the cleanup.

# Start guest agent in foreground.
echo "[stage2] starting guest-agent"
bmark agent_start
if [ -f /mnt/anvil/.anvil-debug ]; then
    echo "[stage2] enabling guest-agent debug mode"
    export ANVIL_DEBUG=1
    # Stream guest-agent stdout/stderr to the virtiofs share so debug logs are
    # inspectable on the host without entering the VM.
    mkdir -p /mnt/anvil/.anvil-run
    exec /bin/guest-agent >>/mnt/anvil/.anvil-run/guest-agent.log 2>&1
fi
exec /bin/guest-agent
STAGE2
chmod +x /newroot/stage2.sh
exec switch_root /newroot /bin/sh /stage2.sh
EOF
chmod +x myinit

# Pack the rootfs with GNU cpio inside the Linux container. zstd decompresses
# much faster than gzip at boot (the kernel supports CONFIG_RD_ZSTD).
mkdir -p "$(dirname "$OUT")"
find . -mindepth 1 -print0 | cpio -o -0 -H newc | zstd -q -19 -T0 > "$OUT"

if [[ -n "${HOST_UID:-}" && -n "${HOST_GID:-}" ]]; then
    chown "$HOST_UID:$HOST_GID" "$OUT" 2>/dev/null || true
fi

echo "created $OUT"
