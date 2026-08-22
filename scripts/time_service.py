#!/usr/bin/env python3
"""Time vz-runner cold boot vs resume from snapshot for a real service.

Runs from the project root:
    python3 scripts/time_service.py

Measures wall-clock time from process start until `curl localhost:8080`
returns HTTP 200, going through:
1. cold boot (--fresh) + load/pull nginx image + run nginx + curl
2. resume from saved snapshot + run nginx + curl

Containers are driven through anvil's Docker API socket (~/.anvil-vz/docker.sock,
the same endpoint users configure their docker context to). If
/tmp/anvil-share/nginx.tar exists, it is imported with `docker load`, which
avoids network variability and gives stable numbers; otherwise the image is
pulled from the registry.

Output is printed as plain timings; non-zero exit code on failure.
"""

import os
import signal
import subprocess
import sys
import time
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parent.parent
VZ_RUNNER = PROJECT_ROOT / ".build" / "release" / "vz-runner"
SHARE_DIR = Path("/tmp/anvil-share")
DAEMON_LOG = Path("/tmp/time_service_vz_runner.log")
DOCKER_SOCKET = Path.home() / ".anvil-vz" / "docker.sock"
DOCKER_ENV = {**os.environ, "DOCKER_HOST": f"unix://{DOCKER_SOCKET}"}
CURL_CMD = ["curl", "--noproxy", "*", "-s", "-o", "/dev/null", "-w", "%{http_code}", "http://localhost:8080"]


def kill_daemon() -> None:
    """Stop any running vz-runner daemon and clean its pid/snapshot state."""
    subprocess.run(["pkill", "-9", "-f", "vz-runner"], capture_output=True)
    time.sleep(0.5)
    state_dir = Path.home() / ".anvil-vz"
    for name in ("daemon.pid", "run.json"):
        (state_dir / name).unlink(missing_ok=True)
    # Leave snapshots for the resume phase; remove only when explicitly asked.


def remove_snapshots() -> None:
    import shutil
    snapshot_dir = Path.home() / ".anvil-vz" / "snapshots"
    if snapshot_dir.exists():
        shutil.rmtree(snapshot_dir)


def start_daemon(fresh: bool) -> subprocess.Popen:
    """Start the daemon in the background and return the Popen object."""
    DAEMON_LOG.unlink(missing_ok=True)
    SHARE_DIR.mkdir(parents=True, exist_ok=True)
    cmd = [str(VZ_RUNNER), "daemon", "--share", str(SHARE_DIR)]
    if fresh:
        remove_snapshots()
    return subprocess.Popen(
        cmd,
        stdout=open(DAEMON_LOG, "w"),
        stderr=subprocess.STDOUT,
        cwd=PROJECT_ROOT,
    )


def wait_for_marker(marker: str, timeout: float = 120.0) -> float:
    """Return seconds until marker appears in the daemon log."""
    t0 = time.time()
    while time.time() - t0 < timeout:
        if DAEMON_LOG.exists():
            text = DAEMON_LOG.read_text(errors="replace")
            if marker in text:
                return time.time() - t0
        time.sleep(0.1)
    raise TimeoutError(f"marker not found within {timeout}s: {marker}")


def load_or_pull_nginx(timeout: float = 120.0) -> float:
    """Make the nginx image available inside the VM.

    If /tmp/anvil-share/nginx.tar exists on the host, import it with
    `docker load`. This avoids relying on the network and gives stable
    benchmark numbers. Otherwise fall back to `docker pull`.

    The timeout is 120s: loading a local tarball takes ~1-2s, while a slow
    registry pull over a congested link can need up to a minute. Anything
    longer than that is effectively a hang and should fail fast.
    """
    t0 = time.time()
    # Check from the host side whether the tarball is present.
    if (SHARE_DIR / "nginx.tar").exists():
        cmd = ["docker", "--host", f"unix://{DOCKER_SOCKET}", "load", "-i", str(SHARE_DIR / "nginx.tar")]
    else:
        cmd = ["docker", "--host", f"unix://{DOCKER_SOCKET}", "pull", "nginx"]
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    if proc.returncode != 0:
        raise RuntimeError(f"image prep failed: {proc.stderr or proc.stdout}")
    return time.time() - t0


def run_nginx(timeout: float = 60.0) -> float:
    """Run nginx with a host port mapping and return elapsed wall time."""
    t0 = time.time()
    cmd = [
        "docker", "--host", f"unix://{DOCKER_SOCKET}",
        "run", "-d", "-p", "8080:80", "--name", "nginx", "nginx",
    ]
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    if proc.returncode != 0:
        raise RuntimeError(f"docker run failed: {proc.stderr or proc.stdout}")
    return time.time() - t0


def wait_for_http_200(timeout: float = 60.0) -> float:
    """Poll localhost:8080 until it returns HTTP 200."""
    t0 = time.time()
    while time.time() - t0 < timeout:
        try:
            proc = subprocess.run(
                CURL_CMD,
                capture_output=True,
                text=True,
                timeout=5.0,
            )
            if proc.stdout.strip() == "200":
                return time.time() - t0
        except subprocess.TimeoutExpired:
            pass
        time.sleep(0.2)
    raise TimeoutError(f"localhost:8080 did not return 200 within {timeout}s")


def remove_nginx() -> None:
    """Remove a previous nginx container if it exists."""
    subprocess.run(
        ["docker", "--host", f"unix://{DOCKER_SOCKET}", "rm", "-f", "nginx"],
        capture_output=True,
    )


def measure_phase(fresh: bool, label: str) -> dict[str, float]:
    """Start daemon, pull/run nginx, curl it, stop daemon; return timings."""
    kill_daemon()
    proc = start_daemon(fresh=fresh)
    try:
        # Wait until the control server is accepting commands. After a cold
        # boot this happens after the guest agent is up and the first snapshot
        # is saved; after resume it happens right after VM resumes.
        daemon_ready = wait_for_marker("daemon ready")
        # Give guest-agent a moment to settle before the first containerd call.
        time.sleep(0.5)

        # Make sure no stale container from an earlier run blocks --name nginx.
        remove_nginx()

        image_time = load_or_pull_nginx() if fresh else 0.0
        container_time = run_nginx()
        curl_time = wait_for_http_200()

        # Remove the container before stopping the daemon so the saved snapshot
        # is clean for the resume phase (image cache stays in memory).
        remove_nginx()

        total = daemon_ready + image_time + container_time + curl_time
        return {
            "daemon_ready": daemon_ready,
            "image_load": image_time,
            "container_start": container_time,
            "curl_200": curl_time,
            "total": total,
        }
    finally:
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=30.0)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
        kill_daemon()


def main() -> int:
    if not VZ_RUNNER.exists():
        print(f"binary not found: {VZ_RUNNER}; run 'make sign' first", file=sys.stderr)
        return 1

    print("=== cold boot + nginx service ===")
    try:
        cold = measure_phase(fresh=True, label="cold")
    except Exception as e:
        print(f"cold phase failed: {e}", file=sys.stderr)
        return 1
    image_label = "load nginx.tar" if (SHARE_DIR / "nginx.tar").exists() else "pull nginx"
    print(f"  daemon ready:       {cold['daemon_ready']:.2f}s")
    print(f"  {image_label:<18}   {cold['image_load']:.2f}s")
    print(f"  docker run nginx:   {cold['container_start']:.2f}s")
    print(f"  curl -> 200:        {cold['curl_200']:.2f}s")
    print(f"  total:              {cold['total']:.2f}s")

    print("\n=== resume from snapshot + nginx service ===")
    try:
        resume = measure_phase(fresh=False, label="resume")
    except Exception as e:
        print(f"resume phase failed: {e}", file=sys.stderr)
        return 1
    print(f"  daemon ready:       {resume['daemon_ready']:.2f}s")
    print(f"  docker run nginx:   {resume['container_start']:.2f}s")
    print(f"  curl -> 200:        {resume['curl_200']:.2f}s")
    print(f"  total:              {resume['total']:.2f}s")

    speedup = cold["total"] / resume["total"]
    print(f"\nresume is ~{speedup:.1f}x faster for full service startup")
    return 0


if __name__ == "__main__":
    sys.exit(main())
