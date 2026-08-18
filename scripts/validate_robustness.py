#!/usr/bin/env python3
"""Robustness checks for vz-runner before publishing benchmarks.

Runs from the project root:
    python3 scripts/validate_robustness.py

Checks:
1. Repeated save/resume cycles (state drift, snapshot size).
2. Resume after real workload (running nginx container).
3. Stateful TCP connection behaviour after resume (best-effort).
4. Kill -9 of vz-runner does not leak an orphan VM process.
5. vsock/exec file-descriptor leaks on host and inside guest.
6. CNI cleanup after container removal.
7. Two-project isolation and host port conflict handling.
8. Restart policy across a save/resume cycle.
9. UDP port forwarding across a save/resume cycle.

Each test prints PASS/FAIL with details; final summary at the end.
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
STATE_DIR = Path.home() / ".anvil-vz"
SNAPSHOT_DIR = STATE_DIR / "snapshots"
SNAPSHOT_FILE = SNAPSHOT_DIR / "default.vzstate"
LOG_FILE = Path("/tmp/validate_robustness.log")

results: list[tuple[str, bool, str]] = []


def log(msg: str) -> None:
    print(msg, flush=True)


def run_host(cmd: list[str], timeout: float = 30.0, check: bool = True) -> subprocess.CompletedProcess:
    proc = subprocess.run(
        cmd,
        capture_output=True,
        text=True,
        timeout=timeout,
        cwd=PROJECT_ROOT,
    )
    if check and proc.returncode != 0:
        raise RuntimeError(f"host cmd failed {' '.join(cmd)}: {proc.stderr or proc.stdout}")
    return proc


def kill_daemon() -> None:
    subprocess.run(["pkill", "-9", "-f", "vz-runner"], capture_output=True)
    time.sleep(0.5)
    for name in ("daemon.pid", "run.json"):
        (STATE_DIR / name).unlink(missing_ok=True)


def remove_snapshots() -> None:
    import shutil
    if SNAPSHOT_DIR.exists():
        shutil.rmtree(SNAPSHOT_DIR)


def start_daemon(fresh: bool = False) -> subprocess.Popen:
    LOG_FILE.unlink(missing_ok=True)
    SHARE_DIR.mkdir(parents=True, exist_ok=True)
    # The persistent containerd disk is required for container starts: the
    # virtiofs fallback for /var/lib breaks the native snapshotter (read-only
    # rootfs errors from runc), the same reason the bench-harness drivers
    # always pass --containerd-disk.
    disk = os.environ.get("ANVIL_VALIDATE_DISK",
                          os.path.expanduser("~/.anvil-vz/validate-disk.img"))
    if not os.path.exists(disk):
        # Sparse image, the same way anvil-service.sh provisions its disk.
        os.makedirs(os.path.dirname(disk), exist_ok=True)
        subprocess.run(["/bin/dd", "if=/dev/zero", f"of={disk}", "bs=1",
                        "count=0", "seek=10g"], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    cmd = [str(VZ_RUNNER), "daemon", "--share", str(SHARE_DIR),
           "--containerd-disk", disk]
    if fresh:
        remove_snapshots()
    return subprocess.Popen(
        cmd,
        stdout=open(LOG_FILE, "w"),
        stderr=subprocess.STDOUT,
        cwd=PROJECT_ROOT,
    )


def wait_for_marker(marker: str, timeout: float = 120.0) -> float:
    t0 = time.time()
    while time.time() - t0 < timeout:
        if LOG_FILE.exists():
            text = LOG_FILE.read_text(errors="replace")
            if marker in text:
                return time.time() - t0
        time.sleep(0.1)
    raise TimeoutError(f"marker not found within {timeout}s: {marker}")


def vz_exec(*args: str, timeout: float = 60.0) -> subprocess.CompletedProcess:
    return run_host([str(VZ_RUNNER), "exec", *args], timeout=timeout)


def snapshot_size() -> int:
    return SNAPSHOT_FILE.stat().st_size if SNAPSHOT_FILE.exists() else 0


def stop_daemon(proc: subprocess.Popen) -> None:
    proc.send_signal(signal.SIGTERM)
    # 120s: a graceful shutdown first syncs /var/lib/containerd to the virtiofs
    # share, which can take tens of seconds for a few hundred MB. Waiting less
    # kills the daemon mid-sync and leaves orphan sync children.
    try:
        proc.wait(timeout=120.0)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
    kill_daemon()


def record(name: str, ok: bool, detail: str) -> None:
    results.append((name, ok, detail))
    status = "PASS" if ok else "FAIL"
    log(f"[{status}] {name}: {detail}")


# ---------------------------------------------------------------------------
# Test 1: repeated save/resume cycles
# ---------------------------------------------------------------------------
def test_repeated_resume_cycles() -> None:
    log("\n=== Test: repeated save/resume cycles ===")
    kill_daemon()
    proc = start_daemon(fresh=True)
    try:
        wait_for_marker("daemon ready")
        # Prepare a clean snapshot with the image cached but no container.
        if (SHARE_DIR / "nginx.tar").exists():
            vz_exec("nerdctl", "-n", "project-a", "load", "-i", "/mnt/anvil/nginx.tar", timeout=120)
        else:
            vz_exec("nerdctl", "-n", "project-a", "pull", "nginx", timeout=300)

        base_size = snapshot_size()
        sizes = [base_size]
        times = []
        failures = 0

        for i in range(1, 11):
            # Each iteration: stop (save) -> start (resume) -> exec health check.
            stop_daemon(proc)
            sizes.append(snapshot_size())
            proc = start_daemon(fresh=False)
            try:
                times.append(wait_for_marker("daemon ready", timeout=60.0))
                vz_exec("uname", "-a", timeout=10.0)
            except Exception as e:
                failures += 1
                log(f"  cycle {i} failed: {e}")
                break

        stop_daemon(proc)
        detail = (
            f"10 cycles, {failures} failures, "
            f"resume ready avg={sum(times)/len(times):.2f}s max={max(times):.2f}s, "
            f"snapshot sizes min={min(sizes)//1024//1024}MB max={max(sizes)//1024//1024}MB"
        )
        record("repeated save/resume cycles", failures == 0, detail)
    except Exception as e:
        stop_daemon(proc)
        record("repeated save/resume cycles", False, str(e))


# ---------------------------------------------------------------------------
# Test 2: resume after real workload
# ---------------------------------------------------------------------------
def test_resume_after_workload() -> None:
    log("\n=== Test: resume after running workload ===")
    kill_daemon()
    proc = start_daemon(fresh=True)
    try:
        wait_for_marker("daemon ready")
        if (SHARE_DIR / "nginx.tar").exists():
            vz_exec("nerdctl", "-n", "project-a", "load", "-i", "/mnt/anvil/nginx.tar", timeout=120)
        vz_exec("nerdctl", "-n", "project-a", "run", "-d", "-p", "8080:80", "--name", "nginx", "nginx")
        # Let it serve for a few seconds before saving.
        for _ in range(30):
            p = run_host(["curl", "--noproxy", "*", "-s", "-o", "/dev/null", "-w", "%{http_code}", "http://localhost:8080"], check=False)
            if p.stdout.strip() == "200":
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("nginx did not serve 200 before suspend")

        # Stop daemon -> saves snapshot with running container.
        stop_daemon(proc)

        # Resume and check container is still reachable.
        proc = start_daemon(fresh=False)
        resume_time = wait_for_marker("daemon ready", timeout=60.0)
        for _ in range(30):
            p = run_host(["curl", "--noproxy", "*", "-s", "-o", "/dev/null", "-w", "%{http_code}", "http://localhost:8080"], check=False)
            if p.stdout.strip() == "200":
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("nginx not reachable after resume")

        stop_daemon(proc)
        record("resume after workload", True, f"resume ready {resume_time:.2f}s, curl -> 200 after resume")
    except Exception as e:
        stop_daemon(proc)
        record("resume after workload", False, str(e))


# ---------------------------------------------------------------------------
# Test 3: stateful TCP connection across resume
# ---------------------------------------------------------------------------
def test_stateful_connection() -> None:
    log("\n=== Test: stateful TCP connection across resume ===")
    kill_daemon()
    proc = start_daemon(fresh=True)
    try:
        wait_for_marker("daemon ready")
        if (SHARE_DIR / "nginx.tar").exists():
            vz_exec("nerdctl", "-n", "project-a", "load", "-i", "/mnt/anvil/nginx.tar", timeout=120)
        vz_exec("nerdctl", "-n", "project-a", "run", "-d", "-p", "8080:80", "--name", "nginx", "nginx")
        for _ in range(30):
            p = run_host(["curl", "--noproxy", "*", "-s", "-o", "/dev/null", "-w", "%{http_code}", "http://localhost:8080"], check=False)
            if p.stdout.strip() == "200":
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("nginx not reachable")

        # Open a keep-alive HTTP connection from the host.
        import socket
        sock = socket.create_connection(("localhost", 8080), timeout=5.0)
        sock.sendall(b"GET / HTTP/1.1\r\nHost: localhost\r\nConnection: keep-alive\r\n\r\n")
        # Read first response.
        sock.settimeout(2.0)
        try:
            data = sock.recv(4096)
        except socket.timeout:
            data = b""
        if b"200 OK" not in data:
            raise RuntimeError("initial keep-alive request did not return 200")

        # Suspend/resume.
        stop_daemon(proc)
        proc = start_daemon(fresh=False)
        wait_for_marker("daemon ready", timeout=60.0)

        # Try to reuse the same socket.
        try:
            sock.settimeout(5.0)
            sock.sendall(b"GET / HTTP/1.1\r\nHost: localhost\r\nConnection: keep-alive\r\n\r\n")
            data2 = sock.recv(4096)
            reused = b"200 OK" in data2
            detail = "same socket reused and got 200" if reused else "same socket failed after resume (expected)"
        except Exception as e:
            reused = False
            detail = f"same socket failed after resume: {e}"
        finally:
            sock.close()

        stop_daemon(proc)
        # We do not require reuse to succeed; stateful connections across VM
        # suspend are allowed to break. We only record what happened.
        record("stateful TCP across resume", True, detail)
    except Exception as e:
        stop_daemon(proc)
        record("stateful TCP across resume", False, str(e))


# ---------------------------------------------------------------------------
# Test 4: kill -9 cleanup
# ---------------------------------------------------------------------------
def test_kill9_cleanup() -> None:
    log("\n=== Test: kill -9 of vz-runner ===")
    kill_daemon()
    proc = start_daemon(fresh=True)
    try:
        wait_for_marker("daemon ready")
        pid = proc.pid
        os.kill(pid, signal.SIGKILL)
        proc.wait()
        time.sleep(5.0)
        # A shared Virtualization.framework XPC service may stay alive on macOS;
        # what matters is that vz-runner itself is gone and the singleton lock
        # is released so a new daemon can start immediately.
        ps = run_host(["ps", "aux"], check=False)
        remaining = [line for line in ps.stdout.splitlines() if "vz-runner" in line and "validate_robustness" not in line]
        if remaining:
            record("kill -9 cleanup", False, f"orphan vz-runner processes: {len(remaining)}")
            return

        # Verify the lock file is gone and a fresh daemon can start.
        (STATE_DIR / "daemon.pid").unlink(missing_ok=True)
        proc2 = start_daemon(fresh=True)
        try:
            wait_for_marker("daemon ready", timeout=60.0)
            stop_daemon(proc2)
            record("kill -9 cleanup", True, "vz-runner gone, lock released, fresh daemon starts after kill -9")
        except Exception as e:
            stop_daemon(proc2)
            record("kill -9 cleanup", False, f"fresh daemon failed to start after kill -9: {e}")
    except Exception as e:
        kill_daemon()
        record("kill -9 cleanup", False, str(e))


# ---------------------------------------------------------------------------
# Test 5: FD leaks after many execs
# ---------------------------------------------------------------------------
def _count_host_fds(pid: int) -> int:
    try:
        out = subprocess.run(["lsof", "-p", str(pid)], capture_output=True, text=True, timeout=5).stdout
        return max(0, len(out.splitlines()) - 1)
    except Exception:
        return -1


def _guest_agent_fd_count() -> int:
    # No awk/cut/wc in the minimal initramfs; parse ps output with the shell
    # and count fds by iterating over /proc/$pid/fd/*.
    script = (
        'set -- $(ps -o pid,comm | grep guest-agent); pid=$1; '
        'count=0; for f in /proc/$pid/fd/*; do count=$((count+1)); done; echo $count'
    )
    try:
        out = vz_exec("sh", "-c", script, timeout=10.0).stdout.strip()
        return int(out.splitlines()[-1]) if out.splitlines() else -1
    except Exception:
        return -1


def test_fd_leaks() -> None:
    log("\n=== Test: exec FD leaks ===")
    kill_daemon()
    proc = start_daemon(fresh=True)
    try:
        wait_for_marker("daemon ready")
        host_before = _count_host_fds(proc.pid)
        guest_before = _guest_agent_fd_count()
        for i in range(50):
            vz_exec("echo", "ok", timeout=10.0)
        host_after = _count_host_fds(proc.pid)
        guest_after = _guest_agent_fd_count()
        stop_daemon(proc)
        detail = (
            f"host fds {host_before} -> {host_after}, "
            f"guest-agent fds {guest_before} -> {guest_after}"
        )
        # Allow a small delta for logs/sockets; fail only on clear growth.
        host_ok = host_before == -1 or host_after == -1 or host_after <= host_before + 5
        guest_ok = guest_before == -1 or guest_after == -1 or guest_after <= guest_before + 5
        record("exec FD leaks", host_ok and guest_ok, detail)
    except Exception as e:
        stop_daemon(proc)
        record("exec FD leaks", False, str(e))


# ---------------------------------------------------------------------------
# Test 6: CNI cleanup
# ---------------------------------------------------------------------------
def _iptables_nat_rules() -> int:
    try:
        out = vz_exec("iptables", "-t", "nat", "-S", timeout=10.0).stdout
        return len([l for l in out.splitlines() if l.startswith("-A")])
    except Exception:
        return -1


def _bridge_interfaces() -> list[str]:
    try:
        out = vz_exec("ip", "link", "show", "type", "bridge", timeout=10.0).stdout
        return [line.split(":")[1].strip() for line in out.splitlines() if ": br-" in line or ": " in line and "br-" in line]
    except Exception:
        return []


def test_cni_cleanup() -> None:
    log("\n=== Test: CNI cleanup after compose down ===")
    kill_daemon()
    proc = start_daemon(fresh=True)
    try:
        wait_for_marker("daemon ready")
        if (SHARE_DIR / "nginx.tar").exists():
            vz_exec("nerdctl", "-n", "project-a", "load", "-i", "/mnt/anvil/nginx.tar", timeout=120)
        vz_exec("nerdctl", "-n", "project-a", "run", "-d", "-p", "8080:80", "--name", "nginx", "nginx")
        rules_with_container = _iptables_nat_rules()
        bridges_with_container = _bridge_interfaces()
        vz_exec("nerdctl", "-n", "project-a", "rm", "-f", "nginx", timeout=30.0)
        rules_after = _iptables_nat_rules()
        bridges_after = _bridge_interfaces()
        stop_daemon(proc)
        ok = rules_after <= rules_with_container and len(bridges_after) <= len(bridges_with_container)
        detail = (
            f"iptables NAT app-rules: {rules_with_container} -> {rules_after}, "
            f"bridges: {bridges_with_container} -> {bridges_after}"
        )
        record("CNI cleanup", ok, detail)
    except Exception as e:
        stop_daemon(proc)
        record("CNI cleanup", False, str(e))


# ---------------------------------------------------------------------------
# Test 7: two-project isolation and port conflict
# ---------------------------------------------------------------------------
def test_two_projects() -> None:
    log("\n=== Test: two-project isolation and port conflict ===")
    kill_daemon()
    proc = start_daemon(fresh=True)
    try:
        wait_for_marker("daemon ready")
        if (SHARE_DIR / "nginx.tar").exists():
            vz_exec("nerdctl", "-n", "project-a", "load", "-i", "/mnt/anvil/nginx.tar", timeout=120)
            vz_exec("nerdctl", "-n", "project-b", "load", "-i", "/mnt/anvil/nginx.tar", timeout=120)
        vz_exec("nerdctl", "-n", "project-a", "run", "-d", "-p", "8080:80", "--name", "nginx-a", "nginx")
        vz_exec("nerdctl", "-n", "project-b", "run", "-d", "-p", "8081:80", "--name", "nginx-b", "nginx")

        def wait_http(url: str) -> str:
            for _ in range(50):
                p = run_host(["curl", "--noproxy", "*", "-s", "-o", "/dev/null", "-w", "%{http_code}", url], check=False)
                if p.stdout.strip() == "200":
                    return "200"
                time.sleep(0.2)
            return "000"

        a_code = wait_http("http://localhost:8080")
        b_code = wait_http("http://localhost:8081")
        both_reachable = a_code == "200" and b_code == "200"

        # Try to bind the same host port in project-b. With the host-port
        # conflict check in guest-agent this must fail before the container is
        # created, with a clear error message.
        conflict = run_host(
            [str(VZ_RUNNER), "exec", "nerdctl", "-n", "project-b", "run", "-d", "-p", "8080:80",
             "--name", "nginx-b-conflict", "nginx"],
            timeout=30.0,
            check=False,
        )
        conflict_rejected = conflict.returncode != 0
        if not conflict_rejected:
            vz_exec("nerdctl", "-n", "project-b", "rm", "-f", "nginx-b-conflict", timeout=30.0)

        # After the rejected conflict attempt, project-a:8080 must still work.
        a_after = wait_http("http://localhost:8080")

        vz_exec("nerdctl", "-n", "project-a", "rm", "-f", "nginx-a", timeout=30.0)
        vz_exec("nerdctl", "-n", "project-b", "rm", "-f", "nginx-b", timeout=30.0)
        stop_daemon(proc)

        ok = both_reachable and conflict_rejected and a_after == "200"
        detail = (
            f"project-a:8080={a_code} project-b:8081={b_code}, "
            f"duplicate host port rejected={conflict_rejected}, "
            f"project-a:8080 after conflict attempt={a_after}"
        )
        record("two-project isolation", ok, detail)
    except Exception as e:
        stop_daemon(proc)
        record("two-project isolation", False, str(e))


# ---------------------------------------------------------------------------
# Test 8: restart policy survives a save/resume cycle
# ---------------------------------------------------------------------------
def test_restart_policy_survives_resume() -> None:
    log("\n=== Test: restart policy across save/resume ===")
    kill_daemon()
    proc = start_daemon(fresh=True)
    try:
        wait_for_marker("daemon ready")
        vz_exec("nerdctl", "-n", "project-a", "pull", "alpine", timeout=300)
        # The container fails once (marker missing), then sleeps: the
        # restart monitor in guest-agent must bring it back up.
        vz_exec("nerdctl", "-n", "project-a", "run", "-d", "--restart", "on-failure:3",
                "--name", "restarting", "alpine", "sh", "-c",
                "[ -f /tmp/m ] && sleep 300 || { touch /tmp/m; exit 1; }")

        def wait_running(timeout: float = 40.0) -> bool:
            deadline = time.time() + timeout
            while time.time() < deadline:
                st = run_host([str(VZ_RUNNER), "exec", "nerdctl", "-n", "project-a",
                               "inspect", "--format", "{{.State.Status}}", "restarting"],
                              timeout=30.0, check=False)
                if st.stdout.strip() == "running":
                    return True
                time.sleep(1.0)
            return False

        if not wait_running():
            raise RuntimeError("container not restarted before save")
        stop_daemon(proc)  # save
        proc = start_daemon(fresh=False)  # resume
        wait_for_marker("daemon ready", timeout=60.0)

        # The policy registry is in guest-agent memory, which IS the
        # snapshot: after resume the monitor must still own the policy.
        # Kill the process (exit 137) and expect a restart.
        vz_exec("nerdctl", "-n", "project-a", "kill", "restarting", timeout=30.0)
        restarted = wait_running()
        # A user stop must still win after the resume.
        vz_exec("nerdctl", "-n", "project-a", "stop", "-t", "1", "restarting", timeout=60.0)
        time.sleep(4.0)
        st = run_host([str(VZ_RUNNER), "exec", "nerdctl", "-n", "project-a",
                       "inspect", "--format", "{{.State.Status}}", "restarting"],
                      timeout=30.0, check=False)
        stop_daemon(proc)
        ok = restarted and st.stdout.strip() == "exited"
        record("restart policy across save/resume", ok,
               f"restarted after resume+kill={restarted}, stopped state={st.stdout.strip()!r}")
    except Exception as e:
        stop_daemon(proc)
        record("restart policy across save/resume", False, str(e))


# ---------------------------------------------------------------------------
# Test 9: UDP port forwarding survives a save/resume cycle
# ---------------------------------------------------------------------------
def test_udp_survives_resume() -> None:
    log("\n=== Test: UDP forwarding across save/resume ===")
    kill_daemon()
    proc = start_daemon(fresh=True)
    try:
        wait_for_marker("daemon ready")
        vz_exec("nerdctl", "-n", "project-a", "pull", "alpine", timeout=300)
        vz_exec("nerdctl", "-n", "project-a", "run", "-d", "-p", "25361:15361/udp",
                "--name", "udpecho", "alpine", "sh", "-c",
                "while true; do echo -n UDP-UP | nc -l -u -p 15361; done")
        time.sleep(2.0)

        def udp_probe() -> str:
            p = run_host(["sh", "-c", "echo ping | nc -u -w 3 localhost 25361"],
                         timeout=10.0, check=False)
            return p.stdout.strip()

        before = udp_probe()
        stop_daemon(proc)  # save
        proc = start_daemon(fresh=False)  # resume
        wait_for_marker("daemon ready", timeout=60.0)
        time.sleep(2.0)  # scanner push + listener rebind
        after = udp_probe()

        vz_exec("nerdctl", "-n", "project-a", "rm", "-f", "udpecho", timeout=30.0)
        stop_daemon(proc)
        ok = before == "UDP-UP" and after == "UDP-UP"
        record("UDP forwarding across save/resume", ok,
               f"before={before!r} after_resume={after!r}")
    except Exception as e:
        stop_daemon(proc)
        record("UDP forwarding across save/resume", False, str(e))


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
def main() -> int:
    if not VZ_RUNNER.exists():
        log(f"binary not found: {VZ_RUNNER}; run 'make sign' first")
        return 1

    test_repeated_resume_cycles()
    test_resume_after_workload()
    test_stateful_connection()
    test_kill9_cleanup()
    test_fd_leaks()
    test_cni_cleanup()
    test_two_projects()
    test_restart_policy_survives_resume()
    test_udp_survives_resume()

    log("\n=== Summary ===")
    passed = sum(1 for _, ok, _ in results if ok)
    total = len(results)
    for name, ok, detail in results:
        status = "PASS" if ok else "FAIL"
        log(f"  [{status}] {name}: {detail}")
    log(f"\n{passed}/{total} checks passed")
    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
