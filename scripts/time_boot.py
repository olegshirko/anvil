#!/usr/bin/env python3
"""Time vz-runner cold boot vs resume from snapshot.

Runs from the project root:
    python3 scripts/time_boot.py

It does:
1. cold boot with --fresh and waits for the snapshot to be saved
2. resumes from that snapshot
3. prints phase timings

Note: stdout is redirected to a log file because vz-runner buffers its
console output when stdout is a pipe, which would make the Python reader hang.
"""

import subprocess
import sys
import time
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parent.parent
VZ_RUNNER = PROJECT_ROOT / ".build" / "release" / "vz-runner"
# The Alpine linux-virt kernel is the production pairing for the containerd
# initramfs (same as the bench-harness driver): its modules are the ones
# packed by build_initramfs_containerd.sh. The Ubuntu generic kernel under
# .download/ubuntu has no matching modules — vsock/virtio modprobes fail and
# the guest panics.
KERNEL = PROJECT_ROOT / ".download" / "alpine" / "vmlinuz-raw"
INITRD = PROJECT_ROOT / ".download" / "ubuntu" / "initramfs-containerd"
SHARE_DIR = Path("/tmp/anvil-share")
LOG_FILE = Path("/tmp/time_boot_vz_runner.log")


def wait_for_lines(cmd: list[str], markers: set[str], timeout: float = 120.0) -> dict[str, float]:
    """Run cmd with stdout redirected to a log file and return wall-clock
    times when each marker first appears."""
    t0 = time.time()
    remaining = set(markers)
    times: dict[str, float] = {}

    LOG_FILE.unlink(missing_ok=True)
    with LOG_FILE.open("w") as log_fh:
        proc = subprocess.Popen(
            cmd,
            stdout=log_fh,
            stderr=subprocess.STDOUT,
            cwd=PROJECT_ROOT,
        )

        try:
            while remaining and time.time() - t0 < timeout:
                if not LOG_FILE.exists():
                    time.sleep(0.1)
                    continue
                # Read whatever has been written so far.
                text = LOG_FILE.read_text(errors="replace")
                for marker in list(remaining):
                    if marker in text:
                        # Find the line containing the marker for a more accurate time.
                        for line in text.splitlines():
                            if marker in line:
                                times[marker] = time.time() - t0
                                remaining.remove(marker)
                                break
                if remaining:
                    time.sleep(0.1)
        finally:
            proc.kill()
            proc.wait()

    return times


def print_phase_report(title: str) -> None:
    """Print host phase marks and guest boot marks from the last run log."""
    if not LOG_FILE.exists():
        return
    text = LOG_FILE.read_text(errors="replace")

    host = [ln.strip() for ln in text.splitlines() if "[anvil] phase" in ln]
    if host:
        print(f"  --- host phases ({title}) ---")
        for ln in host:
            print(f"  {ln}")

    # Guest markers: "[boot] <name> up=<seconds>" (kernel-relative).
    boot: list[tuple[str, float]] = []
    for ln in text.splitlines():
        if "[boot] " in ln and " up=" in ln:
            try:
                name = ln.split("[boot] ", 1)[1].rsplit(" up=", 1)[0]
                up = float(ln.rsplit(" up=", 1)[1])
                boot.append((name, up))
            except (IndexError, ValueError):
                pass
    if boot:
        print(f"  --- guest phases ({title}, kernel-relative) ---")
        prev = 0.0
        for name, up in boot:
            print(f"  {name:<28} +{up - prev:6.2f}s  ({up:6.2f}s)")
            prev = up


def main() -> int:
    if not VZ_RUNNER.exists():
        print(f"binary not found: {VZ_RUNNER}; run 'make sign' first", file=sys.stderr)
        return 1
    if not KERNEL.exists() or not INITRD.exists():
        print(f"kernel/initrd missing; run 'make initramfs-containerd' first", file=sys.stderr)
        return 1

    SHARE_DIR.mkdir(parents=True, exist_ok=True)

    base_cmd = [
        str(VZ_RUNNER),
        "boot",
        "--kernel", str(KERNEL),
        "--initrd", str(INITRD),
        "--agent",
        "--share", str(SHARE_DIR),
    ]

    print("=== cold boot (--fresh) ===")
    cold = wait_for_lines(
        base_cmd + ["--fresh"],
        {"VM started", "guest agent ready", "snapshot saved"},
    )
    for marker in ["VM started", "guest agent ready", "snapshot saved"]:
        print(f"  {marker}: {cold.get(marker, -1.0):.2f}s")
    print_phase_report("cold")

    print("\n=== resume from snapshot ===")
    resume = wait_for_lines(
        base_cmd,
        {"restoring VM from snapshot", "VM resumed"},
    )
    for marker in ["restoring VM from snapshot", "VM resumed"]:
        print(f"  {marker}: {resume.get(marker, -1.0):.2f}s")
    print_phase_report("resume")

    if "guest agent ready" in cold and "VM resumed" in resume:
        speedup = cold["guest agent ready"] / resume["VM resumed"]
        print(f"\nresume is ~{speedup:.1f}x faster to guest-agent ready")

    return 0


if __name__ == "__main__":
    sys.exit(main())
