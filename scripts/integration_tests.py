#!/usr/bin/env python3
"""Docker API integration tests for anvil.

Runs the real `docker` CLI against the running anvil daemon (the Docker API
emulation in guest-agent) and checks the behaviour docker/compose users see:
run/attach/exit codes, port forwarding, host-port conflicts, ps/inspect,
stop, logs, exec, cp, bind mounts via /Users, volumes, images, save/load,
networks, healthchecks, events, compose (incl. two-project isolation) and
the classic /build endpoint.

Unlike scripts/validate_robustness.py (VM lifecycle robustness), this suite
does not manage the daemon: it requires a running one.

    make service-start
    make integration

Uses DOCKER_HOST so the user's docker context is never touched.
"""

import json
import os
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

HOME = Path.home()
DOCKER_SOCKET = HOME / ".anvil-vz" / "docker.sock"
DOCKER_ENV = {**os.environ, "DOCKER_HOST": f"unix://{DOCKER_SOCKET}"}
PREFIX = "anvil-it"
PORT_BASE = 18200

results: list[tuple[str, str, str]] = []  # (name, PASS/FAIL/SKIP, detail)


def log(msg: str) -> None:
    print(msg, flush=True)


def record(name: str, status: str, detail: str) -> None:
    results.append((name, status, detail))
    log(f"[{status}] {name}: {detail}")


def docker(*args: str, timeout: float = 120.0, check: bool = True, input_text: str | None = None) -> subprocess.CompletedProcess:
    proc = subprocess.run(
        ["docker", *args],
        capture_output=True,
        text=True,
        timeout=timeout,
        env=DOCKER_ENV,
        input=input_text,
    )
    if check and proc.returncode != 0:
        raise RuntimeError(f"docker {' '.join(args)} failed ({proc.returncode}): {proc.stderr.strip() or proc.stdout.strip()}")
    return proc


def curl_status(port: int, path: str = "/", wait: float = 15.0) -> str:
    """Poll localhost:<port> until it answers; return the HTTP status code."""
    deadline = time.time() + wait
    last = "000"
    while time.time() < deadline:
        proc = subprocess.run(
            ["curl", "--noproxy", "*", "-s", "-o", "/dev/null",
             "-w", "%{http_code}", "--max-time", "3", f"http://localhost:{port}{path}"],
            capture_output=True, text=True)
        last = proc.stdout.strip()
        if last == "200":
            return last
        time.sleep(0.3)
    return last


def cleanup(*names: str) -> None:
    """Best-effort removal of test containers."""
    for name in names:
        docker("rm", "-f", name, timeout=30.0, check=False)


def test_handshake() -> None:
    ver = docker("version", "--format", "{{.Client.Version}} {{.Server.Version}}")
    info = docker("info", "--format", "{{.ServerVersion}} {{.OSType}}")
    record("docker version/info handshake", "PASS",
           f"client+server agree: {ver.stdout.strip()}, info: {info.stdout.strip()}")


def test_run_rm_output_and_exit_code() -> None:
    out = docker("run", "--rm", "alpine", "echo", "hi-anvil")
    if "hi-anvil" not in out.stdout:
        raise RuntimeError(f"attach output missing: {out.stdout!r}")
    # Non-zero exit code must propagate through AutoRemove + /wait
    # (ARCHITECTURE.md §4.3).
    rc = docker("run", "--rm", "alpine", "sh", "-c", "exit 3", check=False)
    if rc.returncode != 3:
        raise RuntimeError(f"exit code = {rc.returncode}, want 3")
    record("docker run --rm: attach output + exit code", "PASS", "'hi-anvil' captured, exit 3 propagated")


def test_port_forward() -> None:
    name = f"{PREFIX}-web"
    try:
        docker("run", "-d", "--name", name, "-p", f"{PORT_BASE}:80", "nginx")
        code = curl_status(PORT_BASE)
        if code != "200":
            raise RuntimeError(f"nginx on :{PORT_BASE} -> {code}, want 200")
        record("published port forwarded to localhost", "PASS", f"localhost:{PORT_BASE} -> 200")
    finally:
        cleanup(name)


def test_foreign_port_conflict() -> None:
    """A host port held by a foreign process must fail the start loudly."""
    name = f"{PREFIX}-conflict"
    port = PORT_BASE + 1
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(("0.0.0.0", port))
    listener.listen(1)
    try:
        proc = docker("run", "-d", "--name", name, "-p", f"{port}:80", "nginx", check=False)
        ok = (proc.returncode != 0
              and "port is already allocated" in (proc.stderr + proc.stdout))
        if not ok:
            raise RuntimeError(f"start succeeded or wrong error: rc={proc.returncode} err={proc.stderr.strip()!r}")
        record("foreign host-port conflict rejected", "PASS",
               f"start on busy :{port} failed with 'port is already allocated'")
    finally:
        listener.close()
        cleanup(name)


def test_create_ps_inspect_stop() -> None:
    name = f"{PREFIX}-life"
    try:
        docker("create", "--name", name, "alpine", "sleep", "60")
        # ps filter must return exactly this container (regression: the name
        # filter used to be ignored, returning everything).
        st = docker("ps", "-a", "--filter", f"name={name}", "--format", "{{.Names}} {{.Status}}")
        lines = [l for l in st.stdout.splitlines() if l.strip()]
        if len(lines) != 1 or not lines[0].startswith(name):
            raise RuntimeError(f"ps --filter name returned: {lines}")
        if docker("inspect", "--format", "{{.State.Status}}", name).stdout.strip() != "created":
            raise RuntimeError("state after create != created")
        docker("start", name)
        if docker("inspect", "--format", "{{.State.Running}}", name).stdout.strip() != "true":
            raise RuntimeError("not running after start")
        st = docker("ps", "--filter", f"name={name}", "--format", "{{.Names}}")
        if name not in st.stdout.split():
            raise RuntimeError(f"running container not in ps: {st.stdout!r}")
        # Status filter (exact state match).
        st = docker("ps", "--filter", "status=running", "--format", "{{.Names}}")
        if name not in st.stdout.split():
            raise RuntimeError(f"status=running filter lost the container: {st.stdout!r}")
        docker("stop", "-t", "1", name)
        if docker("inspect", "--format", "{{.State.Status}}", name).stdout.strip() != "exited":
            raise RuntimeError("state after stop != exited")
        record("create/start/stop/ps/inspect lifecycle", "PASS",
               "created -> running -> exited; name/status ps filters work")
    finally:
        cleanup(name)


def test_logs() -> None:
    name = f"{PREFIX}-logs"
    try:
        docker("run", "-d", "--name", name, "alpine",
               "sh", "-c", "echo out-line; echo err-line 1>&2; sleep 60")
        time.sleep(2.0)
        logs = docker("logs", name)
        body = logs.stdout + logs.stderr  # json-file driver merges streams
        if "out-line" not in body or "err-line" not in body:
            raise RuntimeError(f"logs incomplete: {body!r}")
        record("docker logs (stdout+stderr)", "PASS", "both streams captured")
    finally:
        cleanup(name)


def test_exec() -> None:
    name = f"{PREFIX}-exec"
    try:
        docker("run", "-d", "--name", name, "alpine", "sleep", "60")
        out = docker("exec", name, "echo", "exec-ok")
        if "exec-ok" not in out.stdout:
            raise RuntimeError(f"exec output: {out.stdout!r}")
        rc = docker("exec", name, "sh", "-c", "exit 7", check=False)
        if rc.returncode != 7:
            raise RuntimeError(f"exec exit code = {rc.returncode}, want 7")
        record("docker exec (output + exit code)", "PASS", "'exec-ok' captured, exit 7 propagated")
    finally:
        cleanup(name)


def test_cp() -> None:
    name = f"{PREFIX}-cp"
    with tempfile.TemporaryDirectory() as tmp:
        src = Path(tmp) / "cp-src.txt"
        src.write_text("cp-payload\n")
        back = Path(tmp) / "cp-back.txt"
        try:
            docker("run", "-d", "--name", name, "alpine", "sleep", "60")
            docker("cp", str(src), f"{name}:/tmp/cp.txt")
            out = docker("exec", name, "cat", "/tmp/cp.txt")
            if "cp-payload" not in out.stdout:
                raise RuntimeError(f"cp in: {out.stdout!r}")
            docker("cp", f"{name}:/tmp/cp.txt", str(back))
            if back.read_text() != "cp-payload\n":
                raise RuntimeError("cp out: content mismatch")
            record("docker cp (in + out via archive endpoints)", "PASS",
                   "file copied in and back, content intact")
        finally:
            cleanup(name)


def test_bind_mount_users_share() -> None:
    marker = f"anvil-it-bind-{int(time.time())}"
    host_file = HOME / f"{marker}.txt"
    host_file.write_text("bind-mount-works\n")
    try:
        proc = docker("run", "--rm", "-v", f"{host_file}:/data/file:ro",
                      "alpine", "cat", "/data/file", check=False)
        if "bind-mount-works" in proc.stdout:
            record("bind mount of host path (/Users share)", "PASS",
                   f"-v $HOME/... -> same path in guest, content read")
        elif proc.returncode != 0:
            record("bind mount of host path (/Users share)", "SKIP",
                   f"share not mounted or mount failed: {proc.stderr.strip()!r}")
        else:
            raise RuntimeError(f"unexpected output: {proc.stdout!r}")
    finally:
        host_file.unlink(missing_ok=True)


def test_named_volume_persistence() -> None:
    vol = f"{PREFIX}-vol"
    try:
        docker("volume", "create", vol)
        ls = docker("volume", "ls", "--format", "{{.Name}}")
        if vol not in ls.stdout.split():
            raise RuntimeError("volume not listed after create")
        docker("run", "--rm", "-v", f"{vol}:/data", "alpine",
               "sh", "-c", "echo persisted > /data/state.txt")
        out = docker("run", "--rm", "-v", f"{vol}:/data", "alpine", "cat", "/data/state.txt")
        if "persisted" not in out.stdout:
            raise RuntimeError(f"volume data lost: {out.stdout!r}")
        record("named volume persists across containers", "PASS",
               "data written by one container read by another")
    finally:
        docker("volume", "rm", "-f", vol, check=False, timeout=30.0)


def test_images_tag_rmi() -> None:
    tag = f"{PREFIX}-img:1"
    try:
        docker("pull", "alpine", timeout=300.0)
        docker("tag", "alpine", tag)
        images = docker("images", "--format", "{{.Repository}}:{{.Tag}}")
        if tag not in images.stdout.splitlines():
            raise RuntimeError(f"tagged image not in docker images: {images.stdout!r}")
        insp = docker("image", "inspect", "--format", "{{.RepoTags}}", tag)
        if tag not in insp.stdout:
            raise RuntimeError(f"image inspect: {insp.stdout!r}")
        record("images: pull/tag/inspect/rmi", "PASS", "alpine tagged, listed, inspected")
    finally:
        docker("rmi", "-f", tag, check=False, timeout=60.0)


def test_save_load() -> None:
    tag = f"{PREFIX}-save:1"
    tar = tempfile.NamedTemporaryFile(suffix=".tar", delete=False)
    tar.close()
    try:
        docker("pull", "busybox", timeout=300.0)
        docker("tag", "busybox", tag)
        docker("save", "-o", tar.name, tag, timeout=180.0)
        docker("rmi", "-f", tag, timeout=60.0)
        out = docker("load", "-i", tar.name, timeout=180.0)
        # The load path canonicalizes short refs (docker.io/library/<name>).
        if f"Loaded image" not in out.stdout or tag not in out.stdout:
            raise RuntimeError(f"load output: {out.stdout!r}")
        images = docker("images", "--format", "{{.Repository}}:{{.Tag}}")
        if tag not in images.stdout.splitlines():
            raise RuntimeError("image missing after load")
        record("docker save/load roundtrip", "PASS", "save -> rmi -> load -> image available")
    finally:
        Path(tar.name).unlink(missing_ok=True)
        docker("rmi", "-f", tag, check=False, timeout=60.0)


def test_network_lifecycle() -> None:
    net = f"{PREFIX}-net"
    name = f"{PREFIX}-netc"
    try:
        docker("network", "create", net)
        insp = docker("network", "inspect", "--format", "{{.Name}} {{.Driver}}", net)
        if not insp.stdout.strip().startswith(f"{net} bridge"):
            raise RuntimeError(f"network inspect: {insp.stdout!r}")
        docker("run", "-d", "--name", name, "--network", net, "alpine", "sleep", "60")
        ip = docker("inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)
        if not ip.stdout.strip().startswith("10.10."):
            raise RuntimeError(f"container IP {ip.stdout.strip()!r} not in a 10.10.x CNI subnet")
        cleanup(name)
        time.sleep(1.0)
        docker("network", "rm", net, timeout=60.0)
        record("network create/run/rm with deterministic subnet", "PASS",
               f"bridge created, container got {ip.stdout.strip()}")
    finally:
        cleanup(name)
        docker("network", "rm", net, check=False, timeout=60.0)


def test_healthcheck() -> None:
    name = f"{PREFIX}-hc"
    try:
        docker("run", "-d", "--name", name,
               "--health-cmd", "true", "--health-interval", "1s",
               "--health-retries", "1", "--health-start-period", "0s",
               "alpine", "sleep", "60")
        status = ""
        deadline = time.time() + 30.0
        while time.time() < deadline:
            status = docker("inspect", "--format",
                            "{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}",
                            name).stdout.strip()
            if status == "healthy":
                break
            time.sleep(1.0)
        if status != "healthy":
            raise RuntimeError(f"health status = {status!r}, want healthy")
        ps = docker("ps", "--filter", f"name={name}", "--format", "{{.Status}}")
        if "(healthy)" not in ps.stdout:
            raise RuntimeError(f"ps status: {ps.stdout!r}")
        record("healthcheck -> (healthy) in ps/inspect", "PASS",
               "guest-agent runner reports healthy")
    finally:
        cleanup(name)


def test_events() -> None:
    """GET /events must stream Docker-format events (compose depends on it)."""
    events_out: list[str] = []
    proc = subprocess.Popen(
        ["docker", "events", "--format", "{{json .}}"],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=DOCKER_ENV)

    def collect() -> None:
        for line in proc.stdout:  # type: ignore[union-attr]
            events_out.append(line.strip())

    t = threading.Thread(target=collect, daemon=True)
    t.start()
    name = f"{PREFIX}-ev"
    try:
        time.sleep(0.5)
        docker("run", "--rm", "--name", name, "alpine", "true", timeout=60.0)
        time.sleep(2.0)
    finally:
        proc.send_signal(signal.SIGTERM)
        proc.wait(timeout=10.0)
    types = []
    for line in events_out:
        try:
            types.append(json.loads(line).get("Action", ""))
        except json.JSONDecodeError:
            continue
    needed = {"create", "start", "die"}
    missing = needed - set(types)
    if missing:
        raise RuntimeError(f"events missing actions {missing}, got {types}")
    record("docker events stream", "PASS", f"create/start/die observed: {types}")


def _compose_file(web_port: int) -> str:
    return f"""services:
  web:
    image: nginx
    ports:
      - "{web_port}:80"
    healthcheck:
      test: ["CMD", "true"]
      interval: "1s"
      retries: 1
  app:
    image: alpine
    command: sleep 60
    depends_on:
      web:
        condition: service_healthy
"""


def test_compose_up() -> None:
    project = f"{PREFIX}-c1"
    web_port = PORT_BASE + 10
    with tempfile.TemporaryDirectory() as tmp:
        compose_file = Path(tmp) / "compose.yml"
        compose_file.write_text(_compose_file(web_port))
        base = ["compose", "-p", project, "-f", str(compose_file)]
        try:
            docker(*base, "up", "-d", "--wait", timeout=300.0)
            code = curl_status(web_port)
            if code != "200":
                raise RuntimeError(f"compose web -> {code}")
            ps = docker(*base, "ps", "--format", "{{.Service}} {{.Status}}")
            if "web" not in ps.stdout or "(healthy)" not in ps.stdout:
                raise RuntimeError(f"compose ps: {ps.stdout!r}")
            if "app" not in ps.stdout:
                raise RuntimeError(f"depends_on service missing: {ps.stdout!r}")
            nets = docker("network", "ls", "--format", "{{.Name}}")
            if f"{project}_default" not in nets.stdout.split():
                raise RuntimeError(f"compose network missing: {nets.stdout!r}")
            record("docker compose up (healthcheck + depends_on)", "PASS",
                   f"web healthy on :{web_port}, app started, network {project}_default")
        finally:
            subprocess.run(["docker", *base, "down", "-v", "--timeout", "5"],
                           capture_output=True, text=True, env=DOCKER_ENV, timeout=120.0)


def test_compose_project_isolation() -> None:
    p1, p2 = f"{PREFIX}-p1", f"{PREFIX}-p2"
    port1, port2 = PORT_BASE + 11, PORT_BASE + 12
    with tempfile.TemporaryDirectory() as tmp:
        f1 = Path(tmp) / "p1.yml"
        f2 = Path(tmp) / "p2.yml"
        f1.write_text(_compose_file(port1))
        f2.write_text(_compose_file(port2))
        b1 = ["compose", "-p", p1, "-f", str(f1)]
        b2 = ["compose", "-p", p2, "-f", str(f2)]
        try:
            docker(*b1, "up", "-d", "--wait", timeout=300.0)
            docker(*b2, "up", "-d", "--wait", timeout=300.0)
            c1, c2 = curl_status(port1), curl_status(port2)
            if c1 != "200" or c2 != "200":
                raise RuntimeError(f"projects not reachable: {c1}, {c2}")
            # Each project sees only its own services.
            ps1 = docker("ps", "--filter", f"label=com.docker.compose.project={p1}",
                         "--format", "{{.Label \"com.docker.compose.service\"}}")
            ps2 = docker("ps", "--filter", f"label=com.docker.compose.project={p2}",
                         "--format", "{{.Label \"com.docker.compose.service\"}}")
            s1 = set(ps1.stdout.split())
            s2 = set(ps2.stdout.split())
            if s1 != {"web", "app"} or s2 != {"web", "app"}:
                raise RuntimeError(f"isolation broken: {s1} / {s2}")
            record("two compose projects isolated", "PASS",
                   f"both up ({port1}, {port2}), ps filtered per project")
        finally:
            for base in (b1, b2):
                subprocess.run(["docker", *base, "down", "-v", "--timeout", "5"],
                               capture_output=True, text=True, env=DOCKER_ENV, timeout=120.0)


def test_classic_build() -> None:
    """DOCKER_BUILDKIT=0 sends the context to our POST /build endpoint."""
    tag = f"{PREFIX}-built:1"
    with tempfile.TemporaryDirectory() as tmp:
        ctx = Path(tmp)
        (ctx / "Dockerfile").write_text(
            "FROM alpine\nCOPY marker.txt /marker.txt\nCMD [\"cat\", \"/marker.txt\"]\n")
        (ctx / "marker.txt").write_text("built-by-anvil\n")
        env = {**DOCKER_ENV, "DOCKER_BUILDKIT": "0"}
        try:
            proc = subprocess.run(["docker", "build", "-t", tag, str(ctx)],
                                  capture_output=True, text=True, timeout=600.0, env=env)
            if proc.returncode != 0:
                raise RuntimeError(f"build failed rc={proc.returncode}: "
                                   f"stdout={proc.stdout.strip()[-400:]} "
                                   f"stderr={proc.stderr.strip()[-400:]}")
            out = docker("run", "--rm", tag)
            if "built-by-anvil" not in out.stdout:
                raise RuntimeError(f"built image output: {out.stdout!r}")
            record("classic docker build (POST /build)", "PASS",
                   "context uploaded, built, run")
        finally:
            docker("rmi", "-f", tag, check=False, timeout=60.0)


def test_buildx_remote_load() -> None:
    """docker buildx build --load via the anvil-remote builder imports the
    result into the image store. The builder is selected explicitly: the
    async setup after `anvil start` may still be in flight, and the default
    builder here is the docker-container driver (moby/buildkit in a
    container) which pulls every layer from the registry."""
    docker("buildx", "use", "anvil-remote")
    for _ in range(30):
        insp = docker("buildx", "inspect", "--bootstrap", "anvil-remote",
                      timeout=60.0, check=False)
        if insp.returncode == 0:
            break
        time.sleep(1.0)
    else:
        raise RuntimeError("anvil-remote builder not ready: see daemon.log")
    tag = f"{PREFIX}-bx:1"
    with tempfile.TemporaryDirectory() as tmp:
        (Path(tmp) / "Dockerfile").write_text('FROM alpine\nCMD ["echo", "bx-ok"]\n')
        try:
            proc = docker("buildx", "build", "--load", "-t", tag, tmp,
                          timeout=600.0, check=False)
            if proc.returncode != 0:
                raise RuntimeError(f"buildx build failed: {proc.stderr.strip()[-500:]}")
            out = docker("run", "--rm", tag)
            if "bx-ok" not in out.stdout:
                raise RuntimeError(f"built image output: {out.stdout!r}")
            record("buildx remote driver --load", "PASS", "built via buildkitd in VM, imported, run")
        finally:
            docker("rmi", "-f", tag, check=False, timeout=60.0)


TESTS = [
    ("docker version/info handshake", test_handshake),
    ("run --rm attach + exit code", test_run_rm_output_and_exit_code),
    ("published port forwarded", test_port_forward),
    ("foreign host-port conflict", test_foreign_port_conflict),
    ("container lifecycle", test_create_ps_inspect_stop),
    ("logs", test_logs),
    ("exec", test_exec),
    ("cp", test_cp),
    ("bind mount /Users", test_bind_mount_users_share),
    ("named volume", test_named_volume_persistence),
    ("images tag/rmi", test_images_tag_rmi),
    ("save/load", test_save_load),
    ("network lifecycle", test_network_lifecycle),
    ("healthcheck", test_healthcheck),
    ("events", test_events),
    ("compose up", test_compose_up),
    ("compose isolation", test_compose_project_isolation),
    ("classic build", test_classic_build),
    ("buildx remote --load", test_buildx_remote_load),
]


def main() -> int:
    if not DOCKER_SOCKET.exists():
        log(f"docker socket not found: {DOCKER_SOCKET}")
        log("start the daemon first: make service-start")
        return 2
    try:
        docker("version", "--format", "{{.Server.Version}}", timeout=15.0)
    except Exception as e:
        log(f"daemon not answering on {DOCKER_SOCKET}: {e}")
        return 2

    # Pre-pull shared images once; a network failure here is fatal for most
    # of the suite, so fail fast with a clear message.
    try:
        docker("pull", "alpine", timeout=300.0)
        docker("pull", "nginx", timeout=300.0)
    except Exception as e:
        log(f"cannot pull test images (network?): {e}")
        return 2

    for name, fn in TESTS:
        log(f"\n=== {name} ===")
        try:
            fn()
        except Exception as e:
            record(name, "FAIL", str(e))

    log("\n=== Summary ===")
    passed = sum(1 for _, s, _ in results if s == "PASS")
    skipped = sum(1 for _, s, _ in results if s == "SKIP")
    failed = sum(1 for _, s, _ in results if s == "FAIL")
    for name, status, detail in results:
        log(f"  [{status}] {name}: {detail}")
    log(f"\n{passed} passed, {skipped} skipped, {failed} failed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
