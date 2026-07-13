cat > myinit <<'EOF'
#!/bin/sh
export PATH=/bin:/sbin:/usr/bin:/usr/sbin

mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts

echo "[myinit] preparing tmpfs root"
mkdir -p /newroot
mount -t tmpfs tmpfs /newroot

# Copy everything from the initramfs root except pseudo-FSes and newroot
# itself. This matters because of usrmerge: /bin, /sbin, /lib are symlinks
# to /usr/*, so "cp -a /bin ..." without /usr copies broken links.
cd /
for d in *; do
    case "$d" in
        proc|sys|dev|newroot) continue ;;
    esac
    cp -a "$d" /newroot/ 2>/dev/null || true
done

mkdir -p /newroot/proc /newroot/sys /newroot/dev /newroot/run /newroot/tmp /newroot/var

mount --make-rprivate /
mount --move /proc /newroot/proc
mount --move /sys  /newroot/sys
mount --move /dev  /newroot/dev

for i in 1 2 3 4 5 6 7 8 9 10; do
    [ -e /newroot/dev/vsock ] && break
    sleep 0.2
done
[ -e /newroot/dev/vsock ] || mknod /newroot/dev/vsock c 10 241 2>/dev/null || true

# Sanity check before switch_root — if this is empty, fail with a clear
# message instead of getting a kernel panic from a dead PID 1.
if [ ! -e /newroot/bin/sh ] && [ ! -L /newroot/bin ]; then
    echo "[myinit] FATAL: /newroot has no shell, aborting before switch_root"
    exec sh
fi

echo "[myinit] switch_root -> tmpfs, starting guest-agent"
exec switch_root /newroot /bin/guest-agent
EOF
chmod +x myinit