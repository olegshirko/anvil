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
KERNEL = PROJECT_ROOT / ".download" / "ubuntu" / "vmlinuz-raw"
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

    print("\n=== resume from snapshot ===")
    resume = wait_for_lines(
        base_cmd,
        {"restoring VM from snapshot", "VM resumed"},
    )
    for marker in ["restoring VM from snapshot", "VM resumed"]:
        print(f"  {marker}: {resume.get(marker, -1.0):.2f}s")

    if "guest agent ready" in cold and "VM resumed" in resume:
        speedup = cold["guest agent ready"] / resume["VM resumed"]
        print(f"\nresume is ~{speedup:.1f}x faster to guest-agent ready")

    return 0


if __name__ == "__main__":
    sys.exit(main())
