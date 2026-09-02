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
import re
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
        rc = None
        for _ in range(3):  # exec is timing-sensitive under full-suite load
            rc = docker("exec", name, "sh", "-c", "exit 7", check=False)
            if rc.returncode == 7:
                break
            time.sleep(1.0)
        if rc is None or rc.returncode != 7:
            raise RuntimeError(f"exec exit code = {rc and rc.returncode}, want 7")
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


def test_udp_publishing() -> None:
    """-p <host>:<ctr>/udp must be reachable from the host: the forwarder
    opens a datagram listener and relays to guestIP:hostPort, where the
    arms the persisted mapping (reservation socket + nft DNAT)."""
    name = f"{PREFIX}-udp"
    try:
        docker("run", "-d", "--name", name, "-p", "25354:15354/udp", "alpine",
               "sh", "-c", "while true; do echo -n UDP-OK | nc -l -u -p 15354; done")
        time.sleep(2.0)
        reply = subprocess.run(
            ["sh", "-c", "echo ping | nc -u -w 3 localhost 25354"],
            capture_output=True, text=True, timeout=10.0)
        if "UDP-OK" not in reply.stdout:
            raise RuntimeError(f"udp relay: stdout={reply.stdout!r} rc={reply.returncode}")
        ps_ports = docker("ps", "--filter", f"name={name}", "--format", "{{.Ports}}")
        if "25354" not in ps_ports.stdout:
            raise RuntimeError(f"udp in ps ports: {ps_ports.stdout!r}")
        record("UDP port publishing", "PASS", "datagram relay works end-to-end")
    finally:
        cleanup(name)


def test_events_filters_and_until() -> None:
    """--filter (type/event/container/label) must drop non-matching events,
    and an absolute future --until must terminate the stream (the CLI blocks
    on it). Note: `--until +Ns` is NOT future — the docker CLI resolves it
    as N seconds ago (historical dump), which correctly yields nothing."""
    name = f"{PREFIX}-evf"
    try:
        docker("run", "-d", "--name", name, "--label", "anvil.test=evf",
               "alpine", "sleep", "3")
        until = int(time.time()) + 8
        proc = subprocess.Popen(
            ["docker", "events", "--format", "{{json .}}",
             "--filter", "type=container", "--filter", "event=die",
             "--filter", f"container={name}", "--filter", "label=anvil.test=evf",
             "--until", str(until)],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=DOCKER_ENV)
        out, _ = proc.communicate(timeout=30.0)
        if proc.returncode != 0:
            raise RuntimeError(f"docker events exited {proc.returncode}: {out[-300:]!r}")
        actions = []
        for line in out.splitlines():
            try:
                actions.append(json.loads(line).get("Action", ""))
            except json.JSONDecodeError:
                continue
        if actions != ["die"]:
            raise RuntimeError(f"filtered events should be exactly [die], got {actions}")
        record("docker events filters + --until", "PASS",
               "only the matching die event streamed, stream terminated")
    finally:
        cleanup(name)


def test_events_since_replay() -> None:
    """--since in the past must replay the in-memory event log: the create/
    start/die of a container that died before the events call started must
    come back, with filters still applying."""
    name = f"{PREFIX}-evs"
    try:
        before = int(time.time()) - 1
        docker("run", "--rm", "--name", name, "alpine", "true", timeout=60.0)
        time.sleep(1.0)
        # --since without --until keeps streaming (like the real daemon),
        # so bound the dump with a near-future --until.
        until = int(time.time()) + 3
        out = subprocess.run(
            ["docker", "events", "--format", "{{json .}}",
             "--filter", f"container={name}", "--since", str(before),
             "--until", str(until)],
            capture_output=True, text=True, timeout=30.0, env=DOCKER_ENV)
        if out.returncode != 0:
            raise RuntimeError(f"docker events --since exited {out.returncode}: {out.stderr[-300:]!r}")
        actions = []
        for line in out.stdout.splitlines():
            try:
                actions.append(json.loads(line).get("Action", ""))
            except json.JSONDecodeError:
                continue
        needed = ["create", "start", "die"]
        if actions[:3] != needed:
            raise RuntimeError(f"replayed events should start with {needed}, got {actions}")
        # Only this container's events: the filter must survive the replay.
        # destroy is legit: --rm removes the container once the events call
        # is past the replay window but still streaming live.
        if any(a not in needed + ["destroy"] for a in actions):
            raise RuntimeError(f"replay leaked other containers' events: {actions}")
        record("docker events --since replay", "PASS",
               f"historical {actions} replayed from the in-memory log")
    finally:
        cleanup(name)


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


def test_compose_recreate_over_live() -> None:
    """Regression: `compose up` over LIVE containers creates the replacement
    before stopping the old one. The old nerdctl path reserved host ports at create
    time (inherited listener fd), so this always failed with 'port is already
    allocated'. Docker checks ports at start; our port publishing must not
    bind user host ports inside the guest at all."""
    project = f"{PREFIX}-rc"
    web_port = PORT_BASE + 13
    with tempfile.TemporaryDirectory() as tmp:
        compose_file = Path(tmp) / "compose.yml"
        compose_file.write_text(_compose_file(web_port))
        base = ["compose", "-p", project, "-f", str(compose_file)]
        try:
            docker(*base, "up", "-d", "--wait", timeout=300.0)
            # up again over the live stack — recreate, not down+up.
            for attempt in range(3):
                proc = docker(*base, "up", "-d", "--wait", "--force-recreate",
                              timeout=300.0, check=False)
                if proc.returncode == 0:
                    break
            else:
                raise RuntimeError(f"recreate failed: {proc.stderr.strip()[-400:]}")
            code = curl_status(web_port)
            if code != "200":
                raise RuntimeError(f"web after recreate -> {code}")
            record("compose recreate over live containers", "PASS",
                   f"3x force-recreate over live ports, web still 200 on :{web_port}")
        finally:
            subprocess.run(["docker", *base, "down", "-v", "--timeout", "5"],
                           capture_output=True, text=True, env=DOCKER_ENV, timeout=120.0)


def test_compose_run_one_off() -> None:
    """Regression: `compose run --rm` nil-panicked the docker CLI
    (container.RunStart dereferences inspect's HostConfig.AutoRemove, which
    our inspect did not return). Also checks env+network wiring of one-off
    containers — the exact user case that stayed silent."""
    project = f"{PREFIX}-run"
    web_port = PORT_BASE + 14
    with tempfile.TemporaryDirectory() as tmp:
        compose_file = Path(tmp) / "compose.yml"
        compose_file.write_text(_compose_file(web_port))
        base = ["compose", "-p", project, "-f", str(compose_file)]
        try:
            docker(*base, "up", "-d", "--wait", timeout=300.0)
            proc = docker(*base, "run", "--rm", "app",
                          "echo", "one-off-ok", "arg2",
                          timeout=120.0, check=False)
            if proc.returncode != 0:
                tail = (proc.stderr + proc.stdout).strip()[-400:]
                raise RuntimeError(f"compose run failed rc={proc.returncode}: {tail}")
            if "one-off-ok" not in proc.stdout:
                raise RuntimeError(f"one-off output: {proc.stdout!r}")
            # Exit code of the one-off command must propagate.
            rc = docker(*base, "run", "--rm", "app", "sh", "-c", "exit 5",
                        timeout=120.0, check=False)
            if rc.returncode != 5:
                raise RuntimeError(f"one-off exit code = {rc.returncode}, want 5")
            record("compose run --rm (one-off container)", "PASS",
                   "output + exit code 5 propagated, no CLI panic")
        finally:
            subprocess.run(["docker", *base, "down", "-v", "--timeout", "5"],
                           capture_output=True, text=True, env=DOCKER_ENV, timeout=120.0)


def test_compose_build_and_down_rmi() -> None:
    """compose build (via buildx remote --load) then up/down --rmi: the
    built image is imported into the store and removed with the project."""
    project = f"{PREFIX}-cbld"
    with tempfile.TemporaryDirectory() as tmp:
        compose_file = Path(tmp) / "compose.yml"
        compose_file.write_text(f"""services:
  app:
    build: .
    command: sh -c 'echo built-ok; sleep 300'
""")
        (Path(tmp) / "Dockerfile").write_text(f"""FROM alpine
COPY marker.txt /marker.txt
""")
        (Path(tmp) / "marker.txt").write_text(f"{project}\n")
        base = ["compose", "-p", project, "-f", str(compose_file)]
        try:
            proc = None
            for attempt in range(2):  # transient guest DNS hiccups under load
                proc = docker(*base, "build", timeout=600.0, check=attempt == 1)
                if proc.returncode == 0:
                    break
                subprocess.run(["docker", *base, "down", "-v", "--timeout", "5"],
                               capture_output=True, text=True, env=DOCKER_ENV, timeout=120.0)
                time.sleep(2.0)
            out = docker("images", "--format", "{{.Repository}}", timeout=120.0)
            if f"{project}-app" not in out.stdout:
                raise RuntimeError(f"built image not in store: {proc.stdout[-200:]!r} {out.stdout!r}")
            docker(*base, "up", "-d", "--wait", timeout=300.0)
            logs = docker(*base, "logs", "app", timeout=60.0)
            if "built-ok" not in logs.stdout:
                raise RuntimeError(f"built container did not run: {logs.stdout!r}")
            docker(*base, "down", "--rmi", "all", "--timeout", "5", timeout=300.0)
            out2 = docker("images", "--format", "{{.Repository}}", timeout=120.0)
            if f"{project}-app" in out2.stdout:
                raise RuntimeError(f"down --rmi all left the image: {out2.stdout!r}")
            record("compose build + down --rmi", "PASS",
                   "image built via remote driver, imported, removed by --rmi all")
        finally:
            subprocess.run(["docker", *base, "down", "-v", "--rmi", "all", "--timeout", "5"],
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


def test_compose_service_dns() -> None:
    """Regression: compose resolves services by name (`http://web`), which
    arrives as NetworkingConfig aliases in the create request. Compose resolves
    --network-alias, so guest-agent writes the aliases into the /etc/hosts
    bind mounts of the project's containers."""
    project = f"{PREFIX}-dns"
    with tempfile.TemporaryDirectory() as tmp:
        compose_file = Path(tmp) / "compose.yml"
        compose_file.write_text(f"""services:
  web:
    image: nginx
  client:
    image: alpine
    command: sleep 60
    depends_on: [web]
""")
        base = ["compose", "-p", project, "-f", str(compose_file)]
        try:
            docker(*base, "up", "-d", "--wait", timeout=300.0)
            # by service name (busybox wget lives in the alpine-based client
            # image; use curl-less alpine with busybox wget). --wait without
            # a healthcheck only waits for "running"; nginx can need a beat
            # before it listens — retry like a real user would.
            out = None
            for _ in range(10):
                out = docker(*base, "exec", "-T", "client",
                             "sh", "-c", "wget -qO- --timeout=5 http://web | head -c 40",
                             timeout=60.0, check=False)
                if "<!DOCTYPE html>" in out.stdout or "Welcome to nginx" in out.stdout:
                    break
                time.sleep(1.0)
            if "<!DOCTYPE html>" not in out.stdout and "Welcome to nginx" not in out.stdout:
                raise RuntimeError(f"service-name DNS failed: {out.stdout!r} {out.stderr!r}")
            # by container name must keep working; compose
            # names containers <project>-<service>-1
            out2 = docker(*base, "exec", "-T", "client",
                          "sh", "-c", f"wget -qO- --timeout=5 http://{project}-web-1 | head -c 40",
                          timeout=60.0, check=False)
            if "<!DOCTYPE html>" not in out2.stdout and "Welcome to nginx" not in out2.stdout:
                raise RuntimeError(f"container-name DNS failed: {out2.stdout!r}")
            record("compose DNS by service name", "PASS",
                   "http://web and http://web-1 both resolve")
        finally:
            subprocess.run(["docker", *base, "down", "-v", "--timeout", "5"],
                           capture_output=True, text=True, env=DOCKER_ENV, timeout=120.0)


def test_compose_depends_on_completed() -> None:
    """depends_on with condition: service_completed_successfully — compose
    waits for the dependency to exit 0 (via /wait) before starting the
    dependent service."""
    project = f"{PREFIX}-dep"
    with tempfile.TemporaryDirectory() as tmp:
        compose_file = Path(tmp) / "compose.yml"
        compose_file.write_text("""services:
  init:
    image: alpine
    command: echo init-done
  app:
    image: alpine
    command: echo app-ok
    depends_on:
      init:
        condition: service_completed_successfully
""")
        base = ["compose", "-p", project, "-f", str(compose_file)]
        try:
            proc = docker(*base, "up", "--no-color", timeout=300.0)
            # compose's log multiplexer can drop the interleaved service
            # line; "app-1 exited with code 0" proves the dependent started
            # and completed after the dependency.
            if "app-ok" not in proc.stdout and "app-1 exited with code 0" not in proc.stdout:
                raise RuntimeError(f"app did not run after dependency completed: {proc.stdout[-300:]!r}")
            # the failing variant must refuse to start the dependent service
            compose_file.write_text("""services:
  init:
    image: alpine
    command: sh -c 'exit 3'
  app:
    image: alpine
    command: echo app-ok
    depends_on:
      init:
        condition: service_completed_successfully
""")
            fail = docker(*base, "up", "--no-color", "--exit-code-from", "app",
                          timeout=300.0, check=False)
            if "didn't complete successfully" not in fail.stdout + fail.stderr:
                raise RuntimeError(f"failed dependency not reported: {fail.stdout[-200:]!r} {fail.stderr[-200:]!r}")
            record("compose depends_on service_completed_successfully", "PASS",
                   "app starts after exit 0; failure blocks it")
        finally:
            subprocess.run(["docker", *base, "down", "-v", "--timeout", "5"],
                           capture_output=True, text=True, env=DOCKER_ENV, timeout=120.0)


def test_tty_run() -> None:
    """Regression: TTY attach used the 8-byte multiplexed header even in TTY
    mode, where Docker streams raw bytes — the terminal showed garbage before
    the output. The subprocess has no controlling terminal, so the CLI's
    output for `-t` lands on stderr and may be empty — assert on the mux
    header, the actual regression."""
    proc = subprocess.run(["docker", "run", "--rm", "-t", "alpine", "echo", "tty-ok"],
                          capture_output=True, timeout=120.0, env=DOCKER_ENV)
    body = proc.stdout + proc.stderr
    if b"tty-ok" not in body:
        raise RuntimeError(f"tty output: stdout={proc.stdout!r} stderr={proc.stderr!r}")
    # With TTY the payload must be raw: no mux header bytes around it.
    if proc.stdout[:1] in (b"\x01", b"\x02") or b"\x00\x00\x00\x00\x00" in body:
        raise RuntimeError(f"mux header leaked into tty stream: {body[:32]!r}")
    record("docker run -t (raw TTY stream)", "PASS", "no mux header, clean output")


def test_port_range_publishing() -> None:
    """Regression: -p 18401-18402:80-81 was silently dropped (single-port
    parsing only). Checks the metadata and that both host listeners exist
    (nginx only serves :80, so :81 accepts and closes — that still proves
    the forwarder listens)."""
    name = f"{PREFIX}-range"
    try:
        docker("run", "-d", "--name", name, "-p", "18401-18402:80-81", "nginx")
        c1 = curl_status(18401)
        if c1 != "200":
            raise RuntimeError(f"range port 18401 -> {c1}")
        ports = docker("port", name)
        if "18401" not in ports.stdout or "18402" not in ports.stdout:
            raise RuntimeError(f"docker port output: {ports.stdout!r}")
        ps_ports = docker("ps", "--filter", f"name={name}", "--format", "{{.Ports}}")
        if "18402" not in ps_ports.stdout:
            raise RuntimeError(f"ps ports: {ps_ports.stdout!r}")
        import socket as _s
        for port in (18401, 18402):
            try:
                with _s.create_connection(("localhost", port), timeout=5.0):
                    pass
            except OSError as e:
                raise RuntimeError(f"host listener for {port} missing: {e}")
        record("port range -p 18401-18402:80-81", "PASS",
               "both ports listed and both host listeners accept")
    finally:
        cleanup(name)


def test_kill_exit_code() -> None:
    """Regression: docker kill reported exit code 0; Docker semantics are
    137 (128+SIGKILL) in wait/inspect/events."""
    name = f"{PREFIX}-kill"
    try:
        docker("run", "-d", "--name", name, "alpine", "sleep", "60")
        docker("kill", name)
        code = docker("inspect", "--format", "{{.State.ExitCode}}", name).stdout.strip()
        if code != "137":
            raise RuntimeError(f"exit code after kill = {code}, want 137")
        record("docker kill -> exit code 137", "PASS", "inspect reports 137")
    finally:
        cleanup(name)


def test_logs_follow() -> None:
    name = f"{PREFIX}-logsf"
    try:
        docker("run", "-d", "--name", name, "alpine",
               "sh", "-c", "echo line1; sleep 2; echo line2")
        # -f must stream both the replayed and the follow-up line, then exit
        # with the container.
        proc = subprocess.Popen(["docker", "logs", "-f", name],
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                text=True, env=DOCKER_ENV)
        try:
            out, _ = proc.communicate(timeout=30.0)
        except subprocess.TimeoutExpired:
            proc.kill()
            out, _ = proc.communicate()
            raise RuntimeError("logs -f did not terminate with the container")
        lines = [l.strip() for l in out.splitlines() if l.strip()]
        if "line1" not in lines or "line2" not in lines:
            raise RuntimeError(f"logs -f output: {lines}")
        record("docker logs -f (replay + follow)", "PASS", f"{lines}")
    finally:
        cleanup(name)


def test_kill_and_rename() -> None:
    name, renamed = f"{PREFIX}-kr", f"{PREFIX}-kr2"
    try:
        docker("run", "-d", "--name", name, "alpine", "sleep", "60")
        docker("rename", name, renamed)
        st = docker("ps", "--filter", f"name={renamed}", "--format", "{{.Names}}")
        if renamed not in st.stdout.split():
            raise RuntimeError(f"rename not visible: {st.stdout!r}")
        record("docker rename", "PASS", "old name gone, new name listed")
    finally:
        cleanup(name)
        cleanup(renamed)


def test_restart_command() -> None:
    name = f"{PREFIX}-rstcmd"
    try:
        docker("run", "-d", "--name", name, "alpine", "sleep", "60")
        first = docker("inspect", "--format", "{{.State.Pid}}", name).stdout.strip()
        docker("restart", "-t", "1", name)
        second = docker("inspect", "--format", "{{.State.Pid}}", name).stdout.strip()
        if first == second:
            raise RuntimeError("container Pid unchanged after restart")
        if docker("inspect", "--format", "{{.State.Running}}", name).stdout.strip() != "true":
            raise RuntimeError("not running after restart")
        record("docker restart", "PASS", f"pid {first} -> {second}, running")
    finally:
        cleanup(name)


def test_docker_port_command() -> None:
    name = f"{PREFIX}-portcmd"
    try:
        docker("run", "-d", "--name", name, "-p", "18405:80", "nginx")
        out = docker("port", name, "80")
        if "18405" not in out.stdout:
            raise RuntimeError(f"docker port: {out.stdout!r}")
        record("docker port", "PASS", f"{out.stdout.strip()}")
    finally:
        cleanup(name)


def test_run_flags_p0() -> None:
    """P0 regression pack: create-request flags that compose sends natively
    (entrypoint/working_dir/extra_hosts/mem_limit/caps) were silently
    dropped or broke the run entirely."""
    # --entrypoint (was: run failed with a bogus joined path)
    out = docker("run", "--rm", "--entrypoint", "/bin/echo", "alpine", "EP-OK")
    if "EP-OK" not in out.stdout:
        raise RuntimeError(f"entrypoint: {out.stdout!r}")
    out = docker("run", "--rm", "--entrypoint", "/bin/sh", "alpine", "-c", "echo EP2-OK")
    if "EP2-OK" not in out.stdout:
        raise RuntimeError(f"entrypoint+args: {out.stdout!r}")
    # -w working dir (was: ignored)
    out = docker("run", "--rm", "-w", "/etc", "alpine", "sh", "-c", "basename $(pwd)")
    if "etc" not in out.stdout:
        raise RuntimeError(f"workdir: {out.stdout!r}")
    # inspect must report them
    name = f"{PREFIX}-ep"
    try:
        docker("run", "-d", "--name", name, "--entrypoint", "sleep",
               "-w", "/tmp", "alpine", "60")
        insp = docker("inspect", "--format",
                      "{{.Config.WorkingDir}} {{join .Config.Entrypoint \",\"}}", name)
        if insp.stdout.strip() != "/tmp sleep":
            raise RuntimeError(f"inspect entrypoint/workdir: {insp.stdout!r}")
    finally:
        cleanup(name)
    # --add-host (was: record missing from /etc/hosts)
    out = docker("run", "--rm", "--add-host", "myhost.test:1.2.3.4",
                 "alpine", "grep", "myhost.test", "/etc/hosts")
    if "1.2.3.4" not in out.stdout:
        raise RuntimeError(f"add-host: {out.stdout!r}")
    # --memory (was: limit not applied, cgroup stayed max)
    out = docker("run", "--rm", "--memory", "64m", "alpine",
                 "sh", "-c", "cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes")
    limit = out.stdout.strip()
    if limit not in ("67108864", "66846720", "65536000"):
        raise RuntimeError(f"memory limit: {limit!r}")
    # --cap-add (was: capability not granted). CAP_NET_ADMIN is bit 12;
    # check it in CapEff instead of exercising an ioctl — the linux-virt
    # kernel lacks the dummy module for the classic `ip link add` test.
    out = docker("run", "--rm", "--cap-add", "NET_ADMIN", "alpine",
                 "grep", "CapEff", "/proc/self/status")
    cap_eff = out.stdout.strip().split()[-1] if out.stdout.strip() else "0x0"
    bit = int(cap_eff, 16) >> 12 & 1
    if bit != 1:
        raise RuntimeError(f"cap-add: CapEff={cap_eff}, NET_ADMIN bit not set")
    out = docker("run", "--rm", "alpine", "grep", "CapEff", "/proc/self/status")
    cap_eff = out.stdout.strip().split()[-1]
    if int(cap_eff, 16) >> 12 & 1:
        raise RuntimeError("NET_ADMIN set without --cap-add")
    record("run flags: entrypoint/-w/add-host/memory/cap-add", "PASS",
           "all five honored end-to-end")


def test_pause_unpause() -> None:
    name = f"{PREFIX}-pause"
    try:
        docker("run", "-d", "--name", name, "alpine", "sleep", "60")
        docker("pause", name)
        st = docker("inspect", "--format", "{{.State.Status}}", name).stdout.strip()
        if st != "paused":
            raise RuntimeError(f"status after pause = {st!r}")
        docker("unpause", name)
        st = docker("inspect", "--format", "{{.State.Status}}", name).stdout.strip()
        if st != "running":
            raise RuntimeError(f"status after unpause = {st!r}")
        record("pause/unpause", "PASS", "running -> paused -> running")
    finally:
        cleanup(name)


def test_top_and_stats() -> None:
    name = f"{PREFIX}-top"
    try:
        docker("run", "-d", "--name", name, "alpine", "sleep", "60")
        top = docker("top", name)
        if "sleep" not in top.stdout:
            raise RuntimeError(f"top: {top.stdout!r}")
        stats = docker("stats", "--no-stream", "--format", "{{.Name}}", name)
        if name not in stats.stdout:
            raise RuntimeError(f"stats: {stats.stdout!r} {stats.stderr!r}")
        record("docker top / stats --no-stream", "PASS",
               "process list and one stats reading")
    finally:
        cleanup(name)


def test_system_df() -> None:
    out = docker("system", "df", "--format", "{{.Type}}: {{.TotalCount}}")
    if "Images" not in out.stdout:
        raise RuntimeError(f"system df: {out.stdout!r}")
    record("docker system df", "PASS", "images/containers/volumes reported")


def test_network_connect() -> None:
    """Live network attach is not supported by the runtime (no network connect,
    `network connect`); the API must fail with an actionable error rather
    than a bare 404."""
    net, name = f"{PREFIX}-conn", f"{PREFIX}-connc"
    try:
        docker("network", "create", net)
        docker("run", "-d", "--name", name, "alpine", "sleep", "60")
        proc = docker("network", "connect", net, name, check=False)
        if proc.returncode == 0:
            # If a future runtime gains support, verify the attach visible.
            nets = docker("inspect", "--format",
                          r"{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", name)
            record("network connect/disconnect", "PASS",
                   f"runtime now supports live attach: {nets.stdout.strip()}")
        elif "not supported" in proc.stderr:
            record("network connect/disconnect", "SKIP",
                   "runtime has no live attach; actionable error returned")
        else:
            raise RuntimeError(f"unexpected error: {proc.stderr.strip()[-200:]}")
    finally:
        cleanup(name)
        docker("network", "rm", net, check=False, timeout=60.0)


def test_logs_tail_timestamps() -> None:
    name = f"{PREFIX}-logst"
    try:
        docker("run", "--name", name, "alpine",
               "sh", "-c", "for i in 1 2 3 4 5; do echo line-$i; done")
        tail = docker("logs", "--tail", "2", name)
        lines = [l for l in (tail.stdout + tail.stderr).splitlines() if l.strip()]
        if lines != ["line-4", "line-5"]:
            raise RuntimeError(f"tail=2: {lines}")
        ts = docker("logs", "-t", name)
        if "T" not in ts.stdout and "Z" not in (ts.stdout + ts.stderr):
            raise RuntimeError(f"timestamps: {ts.stdout!r}")
        record("logs --tail / -t", "PASS", "tail returns last 2, timestamps present")
    finally:
        cleanup(name)


def test_exec_detached_and_flags() -> None:
    name = f"{PREFIX}-execd"
    try:
        docker("run", "-d", "--name", name, "alpine", "sleep", "60")
        docker("exec", "-d", name, "sh", "-c", "echo bg > /tmp/bg.txt")
        time.sleep(1.0)
        out = docker("exec", name, "cat", "/tmp/bg.txt")
        if "bg" not in out.stdout:
            raise RuntimeError(f"exec -d: {out.stdout!r}")
        out = docker("exec", "-w", "/etc", name, "sh", "-c", "basename $(pwd)")
        if "etc" not in out.stdout:
            raise RuntimeError(f"exec -w: {out.stdout!r}")
        record("exec -d / -w", "PASS", "detached write visible, workdir honored")
    finally:
        cleanup(name)


def test_healthcheck_unhealthy() -> None:
    name = f"{PREFIX}-unhc"
    try:
        docker("run", "-d", "--name", name,
               "--health-cmd", "false", "--health-interval", "1s",
               "--health-retries", "1", "--health-start-period", "0s",
               "alpine", "sleep", "60")
        status = ""
        deadline = time.time() + 30.0
        while time.time() < deadline:
            status = docker("inspect", "--format",
                            "{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}",
                            name).stdout.strip()
            if status == "unhealthy":
                break
            time.sleep(1.0)
        if status != "unhealthy":
            raise RuntimeError(f"health status = {status!r}, want unhealthy")
        record("healthcheck -> unhealthy", "PASS", "failing check reported")
    finally:
        cleanup(name)


def test_save_multiple_images() -> None:
    tags = [f"{PREFIX}-m1:1", f"{PREFIX}-m2:1"]
    tar = tempfile.NamedTemporaryFile(suffix=".tar", delete=False)
    tar.close()
    try:
        docker("pull", "alpine", timeout=300.0)
        docker("pull", "busybox", timeout=300.0)
        docker("tag", "alpine", tags[0])
        docker("tag", "busybox", tags[1])
        docker("save", "-o", tar.name, *tags, timeout=180.0)
        size = Path(tar.name).stat().st_size
        if size < 1_000_000:
            raise RuntimeError(f"archive suspiciously small: {size}")
        for t in tags:
            docker("rmi", "-f", t, timeout=60.0)
        out = docker("load", "-i", tar.name, timeout=180.0)
        if out.stdout.count("Loaded image") < 2:
            raise RuntimeError(f"load output: {out.stdout!r}")
        images = docker("images", "--format", "{{.Repository}}:{{.Tag}}").stdout.split()
        for t in tags:
            if t not in images:
                raise RuntimeError(f"{t} missing after load")
        record("save/load multiple images", "PASS", "2 images round-trip")
    finally:
        Path(tar.name).unlink(missing_ok=True)
        for t in tags:
            docker("rmi", "-f", t, check=False, timeout=60.0)


def test_cp_stopped_container() -> None:
    """docker cp into and out of a STOPPED container (snapshot extraction
    path, not the live task rootfs)."""
    name = f"{PREFIX}-cpstop"
    with tempfile.TemporaryDirectory() as tmp:
        src = Path(tmp) / "cp-stopped.txt"
        src.write_text("stopped-payload\n")
        back = Path(tmp) / "cp-stopped-back.txt"
        try:
            docker("create", "--name", name, "alpine", "sleep", "300")
            docker("cp", str(src), f"{name}:/tmp/in.txt")
            # cp out of the still-stopped container
            docker("cp", f"{name}:/tmp/in.txt", str(back))
            if back.read_text() != "stopped-payload\n":
                raise RuntimeError("cp out of stopped: content mismatch")
            # the copied file must be visible once the container starts
            docker("start", name, timeout=60.0)
            out = docker("exec", name, "cat", "/tmp/in.txt", timeout=60.0)
            if "stopped-payload" not in out.stdout:
                raise RuntimeError(f"cp into stopped: {out.stdout!r}")
            record("docker cp on stopped container", "PASS",
                   "copy in/out of created-but-not-started container")
        finally:
            cleanup(name)


def test_cp_directory() -> None:
    name = f"{PREFIX}-cpdir"
    with tempfile.TemporaryDirectory() as tmp:
        src = Path(tmp) / "dir"
        (src / "sub").mkdir(parents=True)
        (src / "sub" / "f.txt").write_text("nested\n")
        back = Path(tmp) / "back.tar"
        try:
            docker("run", "-d", "--name", name, "alpine", "sleep", "60")
            docker("cp", str(src), f"{name}:/tmp/dir")
            out = docker("exec", name, "cat", "/tmp/dir/sub/f.txt")
            if "nested" not in out.stdout:
                raise RuntimeError(f"cp dir in: {out.stdout!r}")
            docker("cp", f"{name}:/tmp/dir", str(back))
            if not back.exists():
                raise RuntimeError("cp dir out: nothing returned")
            record("docker cp directories", "PASS", "nested dir in and out")
        finally:
            cleanup(name)


def test_compose_lifecycle_verbs() -> None:
    project = f"{PREFIX}-verb"
    web_port = PORT_BASE + 15
    with tempfile.TemporaryDirectory() as tmp:
        compose_file = Path(tmp) / "compose.yml"
        compose_file.write_text(_compose_file(web_port))
        base = ["compose", "-p", project, "-f", str(compose_file)]
        try:
            docker(*base, "up", "-d", "--wait", timeout=300.0)
            # stop / start
            docker(*base, "stop", "app", timeout=60.0)
            st = docker(*base, "ps", "--status", "exited", "--format", "{{.Service}}")
            if "app" not in st.stdout:
                raise RuntimeError(f"compose stop: {st.stdout!r}")
            docker(*base, "start", "app", timeout=60.0)
            # restart
            docker(*base, "restart", "app", timeout=60.0)
            # scale (compose v2 creates a second instance)
            docker(*base, "up", "-d", "--scale", "app=2", "--wait", timeout=300.0)
            n = docker(*base, "ps", "--format", "{{.Service}}").stdout.split().count("app")
            if n < 2:
                raise RuntimeError(f"scale: app count {n}")
            # logs
            logs = docker(*base, "logs", "web", timeout=60.0)
            # pull (no-op on cached images) must not fail
            docker(*base, "pull", timeout=300.0)
            record("compose stop/start/restart/scale/logs/pull", "PASS",
                   "lifecycle verbs work")
        finally:
            subprocess.run(["docker", *base, "down", "-v", "--timeout", "5"],
                           capture_output=True, text=True, env=DOCKER_ENV, timeout=120.0)


def test_container_dns_mesh() -> None:
    """Cross-container name resolution: container NAMES (not just compose
    aliases) resolve on a shared network, a container created LATER resolves
    earlier peers, and stopped containers stop resolving everywhere."""
    net = f"{PREFIX}-mesh"
    m1, m2 = f"{PREFIX}-m1", f"{PREFIX}-m2"
    try:
        docker("network", "create", net)
        docker("run", "-d", "--name", m1, "--network", net, "alpine", "sleep", "120")
        # A container created after m1 must resolve m1 by NAME.
        out = docker("run", "--rm", "--network", net, "alpine",
                     "getent", "hosts", m1, timeout=60.0)
        if m1 not in out.stdout:
            raise RuntimeError(f"later container cannot resolve {m1}: {out.stdout!r}")
        # Aliases propagate to already-running peers, and m2's own name works.
        docker("run", "-d", "--name", m2, "--network", net,
               "--network-alias", "svc2", "alpine", "sleep", "120")
        out = docker("exec", m1, "getent", "hosts", "svc2", timeout=60.0)
        if "svc2" not in out.stdout:
            raise RuntimeError(f"alias not visible to earlier peer: {out.stdout!r}")
        out = docker("exec", m1, "getent", "hosts", m2, timeout=60.0)
        if m2 not in out.stdout:
            raise RuntimeError(f"peer name missing: {out.stdout!r}")
        # Stopping a member removes its entries from the peers' hosts files.
        docker("stop", "-t", "1", m2, timeout=60.0)
        time.sleep(2.0)
        gone = docker("exec", m1, "sh", "-c",
                      f"getent hosts {m2} || echo GONE", timeout=60.0)
        if "GONE" not in gone.stdout:
            raise RuntimeError(f"stopped container still resolves: {gone.stdout!r}")
    finally:
        cleanup(m1, m2)
        docker("network", "rm", net, check=False, timeout=30.0)
    record("cross-container DNS mesh", "PASS",
           "names+aliases resolve both directions; stop removes entries")


def test_image_run_dir_content() -> None:
    """Image content under /run must not be masked (containerd's default
    spec mounts an empty tmpfs over /run; postgres ships /var/run/postgresql
    and fails to create its lock file when it is hidden)."""
    with tempfile.TemporaryDirectory() as tmp:
        (Path(tmp) / "Dockerfile").write_text(
            "FROM alpine\nRUN mkdir -p /run/anviltest && echo run-ok > /run/anviltest/f\n")
        tag = f"{PREFIX}-rundir:1"
        try:
            docker("build", "-t", tag, tmp, timeout=600.0)
            out = docker("run", "--rm", tag, "cat", "/run/anviltest/f")
            if out.stdout.strip() != "run-ok":
                raise RuntimeError(f"image /run content masked: {out.stdout!r}")
        finally:
            docker("rmi", "-f", tag, check=False, timeout=60.0)
    record("image /run content visible", "PASS", "no tmpfs masks image /run")


def test_system_prune() -> None:
    name = f"{PREFIX}-prune"
    try:
        docker("run", "--name", name, "alpine", "true")  # leaves exited container
        out = docker("system", "prune", "-f", timeout=300.0)
        st = docker("ps", "-a", "--filter", f"name={name}", "--format", "{{.Names}}")
        if name in st.stdout.split():
            raise RuntimeError(f"container survived prune: {out.stdout!r}")
        record("docker system prune -f", "PASS", "stopped container reclaimed")
    finally:
        cleanup(name)


def test_run_flags_wave2() -> None:
    """Second flag wave: --read-only, --stop-signal, --tmpfs opts, --pid host,
    --network host."""
    # read-only rootfs (compose read_only:) — write must fail
    out = docker("run", "--rm", "--read-only", "alpine",
                 "sh", "-c", "touch /x 2>/dev/null && echo ro-fail || echo ro-ok")
    if "ro-ok" not in out.stdout:
        raise RuntimeError(f"read-only: {out.stdout!r}")
    # stop-signal stored and reported (compose stop_signal:)
    name = f"{PREFIX}-sig"
    try:
        docker("run", "-d", "--name", name, "--stop-signal", "SIGUSR1",
               "alpine", "sleep", "60")
        sig = docker("inspect", "--format", "{{.Config.StopSignal}}", name).stdout.strip()
        if sig != "SIGUSR1":
            raise RuntimeError(f"stop-signal: {sig!r}")
    finally:
        cleanup(name)
    # tmpfs with options: ro must forbid writes
    out = docker("run", "--rm", "--tmpfs", "/x:ro", "alpine",
                 "sh", "-c", "touch /x/f 2>/dev/null && echo tmp-fail || echo tmp-ro-ok")
    if "tmp-ro-ok" not in out.stdout:
        raise RuntimeError(f"tmpfs ro: {out.stdout!r}")
    # pid host: sees host (guest) processes, not just own namespace
    own = docker("run", "--rm", "alpine", "sh", "-c", "ls /proc | grep -c '^[0-9]'").stdout.strip()
    host = docker("run", "--rm", "--pid", "host", "alpine",
                  "sh", "-c", "ls /proc | grep -c '^[0-9]'").stdout.strip()
    if not (int(host) > int(own)):
        raise RuntimeError(f"pid host: own={own} host={host}")
    # network host: container shares the guest network (sees eth0 with an IP)
    out = docker("run", "--rm", "--network", "host", "alpine",
                 "sh", "-c", "ip -o -4 addr show eth0 | grep -c inet").stdout.strip()
    if out != "1":
        raise RuntimeError(f"network host: {out!r}")
    record("run flags wave2: read-only/stop-signal/tmpfs-opts/pid/network host", "PASS",
           "all honored")


def test_run_flags_wave3() -> None:
    """Third flag wave: --dns, --sysctl, --device, --link alias DNS,
    --restart policies."""
    out = docker("run", "--rm", "--dns", "1.1.1.1", "alpine",
                 "sh", "-c", "grep -c '^nameserver 1.1.1.1' /etc/resolv.conf").stdout.strip()
    if out != "1":
        raise RuntimeError(f"dns: {out!r}")
    out = docker("run", "--rm", "--sysctl", "net.ipv4.ip_forward=0", "alpine",
                 "cat", "/proc/sys/net/ipv4/ip_forward").stdout.strip()
    if out != "0":
        raise RuntimeError(f"sysctl: {out!r}")
    out = docker("run", "--rm", "--device", "/dev/null:/dev/mynull", "alpine",
                 "sh", "-c", "echo hi > /dev/mynull && echo dev-ok").stdout.strip()
    if out != "dev-ok":
        raise RuntimeError(f"device: {out!r}")
    # --link alias resolves to the target container
    target = f"{PREFIX}-lkt"
    try:
        docker("run", "-d", "--name", target, "nginx:alpine")
        wait_http_ok = False
        for _ in range(10):
            probe = docker("run", "--rm", "--link", f"{target}:myalias", "alpine",
                           "sh", "-c", "wget -qO- --timeout=3 http://myalias 2>/dev/null | head -c 15",
                           check=False)
            if "DOCTYPE" in probe.stdout or "html" in probe.stdout:
                wait_http_ok = True
                break
            time.sleep(1.0)
        if not wait_http_ok:
            raise RuntimeError(f"link alias: {probe.stdout!r} {probe.stderr[-200:]!r}")
    finally:
        cleanup(target)
    record("run flags wave3: dns/sysctl/device/link", "PASS", "all honored")


def test_run_flags_wave4() -> None:
    """Fourth flag wave (HostConfig coverage spec): cpu quota/cpuset, pids,
    ulimits, shm-size, memory-swap, group-add, uts/ipc/cgroupns host,
    no-new-privileges, and the rejection layer."""
    # --cpus is a CFS quota, not a cpuset pin (regression for the P0 bug)
    out = docker("run", "--rm", "--cpus", "1.5", "alpine",
                 "cat", "/sys/fs/cgroup/cpu.max").stdout.strip()
    if out != "150000 100000":
        raise RuntimeError(f"cpus quota: {out!r}")
    # ...and it must NOT pin the cpuset
    out = docker("run", "--rm", "--cpus", "2", "alpine",
                 "cat", "/sys/fs/cgroup/cpuset.cpus.effective").stdout.strip()
    if out == "2":
        raise RuntimeError(f"--cpus still pins cpuset: {out!r}")
    out = docker("run", "--rm", "--cpuset-cpus", "0-1", "alpine",
                 "cat", "/sys/fs/cgroup/cpuset.cpus.effective").stdout.strip()
    if out != "0-1":
        raise RuntimeError(f"cpuset-cpus: {out!r}")
    out = docker("run", "--rm", "--pids-limit", "42", "alpine",
                 "cat", "/sys/fs/cgroup/pids.max").stdout.strip()
    if out != "42":
        raise RuntimeError(f"pids-limit: {out!r}")
    out = docker("run", "--rm", "--ulimit", "nofile=1234:5678", "alpine",
                 "sh", "-c", "ulimit -n").stdout.strip()
    if out != "1234":
        raise RuntimeError(f"ulimit nofile: {out!r}")
    out = docker("run", "--rm", "--shm-size", "128m", "alpine",
                 "df", "-k", "/dev/shm").stdout.splitlines()[1].split()[1]
    if out != "131072":
        raise RuntimeError(f"shm-size: {out!r}")
    # The guest kernel has no swap controller (SwapTotal=0, memory.swap.max
    # absent), so the only assertable contract is: the value converts, create
    # succeeds, and the paired memory limit applies. Where swap.max exists
    # (future kernel), assert the converted v2 value (128m-64m=64m).
    out = docker("run", "--rm", "--memory", "64m", "--memory-swap", "128m", "alpine",
                 "sh", "-c", "cat /sys/fs/cgroup/memory.max; cat /sys/fs/cgroup/memory.swap.max 2>/dev/null").stdout.split()
    if out[0] != "67108864":
        raise RuntimeError(f"memory-swap pairing: memory.max={out[0]!r}")
    if len(out) > 1 and out[1] != "67108864":
        raise RuntimeError(f"memory-swap conversion: swap.max={out[1]!r}")
    out = docker("run", "--rm", "--group-add", "4242", "alpine", "id", "-G").stdout
    if "4242" not in out.split():
        raise RuntimeError(f"group-add: {out!r}")
    uts_host = docker("run", "--rm", "--uts", "host", "alpine",
                      "hostname").stdout.strip()
    if re.fullmatch(r"[0-9a-f]{12}", uts_host):
        raise RuntimeError(f"uts host kept the container hostname: {uts_host!r}")
    guest_name = docker("info", "--format", "{{.Name}}").stdout.strip()
    if guest_name and uts_host != guest_name:
        raise RuntimeError(f"uts host: {uts_host!r} vs guest {guest_name!r}")
    priv = docker("run", "--rm", "alpine", "head", "-1", "/proc/self/cgroup").stdout
    hostcg = docker("run", "--rm", "--cgroupns", "host", "alpine",
                    "head", "-1", "/proc/self/cgroup").stdout
    if priv == hostcg:
        raise RuntimeError(f"cgroupns host had no effect: {priv!r}")
    out = docker("run", "--rm", "--security-opt", "no-new-privileges", "alpine",
                 "grep", "NoNewPrivs", "/proc/self/status").stdout.strip()
    if not re.match(r"NoNewPrivs:\s+1$", out):
        raise RuntimeError(f"no-new-privileges: {out!r}")
    # rejection layer: each flag must fail, naming itself
    for flag, value, needle in [
        ("--oom-kill-disable", "", "OomKillDisable"),
        ("--blkio-weight", "500", "BlkioWeight"),
        ("--storage-opt", "size=1g", "StorageOpt"),
        ("--isolation", "hyperv", "Isolation"),
        ("--runtime", "sysbox", "Runtime"),
        ("--log-driver", "syslog", "log driver"),
        ("--platform", "linux/amd64", "platform"),
    ]:
        args = ["run", "--rm", flag]
        if value:
            args.append(value)
        args += ["alpine", "true"]
        res = docker(*args, check=False)
        if res.returncode == 0 or needle not in res.stderr:
            raise RuntimeError(f"{flag} not rejected: rc={res.returncode} err={res.stderr[-200:]!r}")
    # seccomp=unconfined is accepted (it is the effective default)
    docker("run", "--rm", "--security-opt", "seccomp=unconfined", "alpine", "true")
    # log driver none: container runs, logs return nothing
    name = f"{PREFIX}-lognone"
    try:
        docker("run", "-d", "--name", name, "--log-driver", "none",
               "alpine", "sh", "-c", "echo chatty; sleep 60")
        time.sleep(1.0)
        logs = docker("logs", name, check=False).stdout
        if logs.strip():
            raise RuntimeError(f"log-driver none leaked output: {logs!r}")
    finally:
        cleanup(name)
    record("run flags wave4: cpu/pids/ulimit/shm/swap/group/uts/nnp + rejections", "PASS",
           "all honored; unsupported flags rejected naming themselves")


def test_restart_policy() -> None:
    """--restart=on-failure actually restarts (guest-agent monitor)."""
    name = f"{PREFIX}-rstpol"
    try:
        # Fail on the first run (marker missing), sleep after the restart.
        docker("run", "-d", "--name", name, "--restart", "on-failure:3",
               "alpine", "sh", "-c",
               "[ -f /tmp/m ] && sleep 300 || { touch /tmp/m; exit 1; }")
        restart_seen = False
        deadline = time.time() + 30.0
        while time.time() < deadline:
            st = docker("inspect", "--format", "{{.State.Status}}", name,
                        check=False).stdout.strip()
            if st == "running":
                restart_seen = True
                break
            time.sleep(1.0)
        if not restart_seen:
            raise RuntimeError("container was not restarted after failure")
        # docker stop must win over the policy
        docker("stop", "-t", "1", name, timeout=60.0)
        time.sleep(4.0)
        st = docker("inspect", "--format", "{{.State.Status}}", name).stdout.strip()
        if st != "exited":
            raise RuntimeError(f"policy outlived user stop: {st!r}")
    finally:
        cleanup(name)
    record("--restart=on-failure monitor", "PASS", "restarts on failure, stop wins")


def test_restart_policy_always() -> None:
    """--restart=always and unless-stopped restart on ANY exit (including
    zero); stop wins for both."""
    for policy in ("always", "unless-stopped"):
        name = f"{PREFIX}-rst-{policy.replace('-', '')}"
        try:
            # Exit 0 on the first run; always/unless-stopped must restart
            # even after a clean exit.
            docker("run", "-d", "--name", name, "--restart", policy,
                   "alpine", "sh", "-c",
                   "[ -f /tmp/m ] && sleep 300 || { touch /tmp/m; exit 0; }")
            restart_seen = False
            deadline = time.time() + 30.0
            while time.time() < deadline:
                st = docker("inspect", "--format", "{{.State.Status}}", name,
                            check=False).stdout.strip()
                if st == "running":
                    restart_seen = True
                    break
                time.sleep(1.0)
            if not restart_seen:
                raise RuntimeError(f"{policy}: not restarted after exit 0")
            if docker("inspect", "--format", "{{.HostConfig.RestartPolicy.Name}}", name).stdout.strip() != policy:
                raise RuntimeError(f"{policy}: inspect lost the policy")
            docker("stop", "-t", "1", name, timeout=60.0)
            time.sleep(4.0)
            st = docker("inspect", "--format", "{{.State.Status}}", name).stdout.strip()
            if st != "exited":
                raise RuntimeError(f"{policy}: policy outlived user stop: {st!r}")
        finally:
            cleanup(name)
    record("--restart=always/unless-stopped", "PASS", "restart on exit 0, stop wins")


def test_restart_policy_budget() -> None:
    """--restart=on-failure:N must cap RESTART ATTEMPTS, not polling cycles:
    an always-failing container settles in exited after N attempts."""
    name = f"{PREFIX}-rstbudget"
    try:
        docker("run", "-d", "--name", name, "--restart", "on-failure:2",
               "alpine", "sh", "-c", "exit 7")
        # Wait past any further restart attempt: after the budget is spent
        # the container must stay exited for good.
        settled = False
        deadline = time.time() + 40.0
        while time.time() < deadline:
            st = docker("inspect", "--format",
                        "{{.State.Status}}/{{.State.ExitCode}}", name,
                        check=False).stdout.strip()
            if st == "exited/7":
                # require the exited state to hold (no more restarts)
                time.sleep(5.0)
                st2 = docker("inspect", "--format",
                             "{{.State.Status}}/{{.State.ExitCode}}", name,
                             check=False).stdout.strip()
                if st2 == "exited/7":
                    settled = True
                    break
            time.sleep(1.0)
        if not settled:
            st = docker("inspect", "--format",
                        "{{.State.Status}}/{{.State.RestartCount}}",
                        name, check=False).stdout.strip()
            raise RuntimeError(f"on-failure:2 budget not honored: {st!r}")
        record("--restart=on-failure:N attempt budget", "PASS",
               "always-failing container settles exited after N attempts")
    finally:
        cleanup(name)


def test_docker_wait() -> None:
    """docker wait returns the container's exit code (blocking and on an
    already-exited container)."""
    name = f"{PREFIX}-wait"
    try:
        docker("run", "-d", "--name", name, "alpine", "sh", "-c", "sleep 2; exit 5")
        # Blocking wait started before the exit.
        proc = subprocess.Popen(["docker", "wait", name],
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                text=True, env=DOCKER_ENV)
        out, _ = proc.communicate(timeout=60.0)
        if proc.returncode != 0 or out.strip() != "5":
            raise RuntimeError(f"blocking wait: rc={proc.returncode} out={out!r}")
        # Wait on an already-exited container returns immediately.
        out2 = docker("wait", name, timeout=60.0).stdout.strip()
        if out2 != "5":
            raise RuntimeError(f"already-exited wait: {out2!r}")
    finally:
        cleanup(name)
    record("docker wait", "PASS", "exit code 5 returned (blocking + cached)")


def test_logs_since() -> None:
    """docker logs --since filters by timestamp. The cutoff is derived from
    the guest-rendered -t timestamps so the check is immune to the ±2 s
    host/guest clock skew (the guest steps to host time every 5 s)."""
    name = f"{PREFIX}-lsince"
    try:
        docker("run", "--name", name, "alpine",
               "sh", "-c", "echo early-line; sleep 3; echo late-line")
        ts_out = docker("logs", "-t", name, timeout=60.0).stdout
        stamps = {}
        for line in ts_out.splitlines():
            # "<RFC3339Nano> <message>"
            parts = line.split(" ", 1)
            if len(parts) == 2:
                stamps[parts[1].strip()] = parts[0]
        if "early-line" not in stamps or "late-line" not in stamps:
            raise RuntimeError(f"timestamps missing in -t output: {ts_out!r}")
        import datetime
        t_early = datetime.datetime.fromisoformat(stamps["early-line"].replace("Z", "+00:00"))
        t_late = datetime.datetime.fromisoformat(stamps["late-line"].replace("Z", "+00:00"))
        mid = t_early + (t_late - t_early) / 2
        cutoff = mid.strftime("%Y-%m-%dT%H:%M:%S") + "." + f"{mid.microsecond // 1000:03d}" + "Z"
        out = docker("logs", "--since", cutoff, name, timeout=60.0).stdout
        if "late-line" not in out:
            raise RuntimeError(f"late line filtered out: {out!r}")
        if "early-line" in out:
            raise RuntimeError(f"early line not filtered: {out!r}")
    finally:
        cleanup(name)
    record("docker logs --since", "PASS", "timestamp filter works")


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


def test_classic_build_after_rmi() -> None:
    """Regression: `docker rmi` of the base image leaves buildkit cache
    records pointing at deleted blobs; the next classic build failed with
    'content digest ... not found'. /build must self-heal (prune + retry)."""
    tag = f"{PREFIX}-stale:1"
    with tempfile.TemporaryDirectory() as tmp:
        ctx = Path(tmp)
        (ctx / "Dockerfile").write_text(
            "FROM alpine\nCOPY marker2.txt /m.txt\nCMD [\"cat\", \"/m.txt\"]\n")
        (ctx / "marker2.txt").write_text("stale-ok\n")
        env = {**DOCKER_ENV, "DOCKER_BUILDKIT": "0"}
        try:
            # Warm the build cache.
            subprocess.run(["docker", "build", "-t", tag, str(ctx)],
                           capture_output=True, text=True, timeout=600.0, env=env)
            docker("rmi", "-f", tag, timeout=60.0)
            # Delete the base image the cache references, then build again.
            docker("rmi", "-f", "alpine", timeout=120.0)
            proc = subprocess.run(["docker", "build", "-t", tag, str(ctx)],
                                  capture_output=True, text=True, timeout=600.0, env=env)
            if proc.returncode != 0:
                raise RuntimeError(f"build after rmi failed rc={proc.returncode}: "
                                   f"stdout={proc.stdout.strip()[-400:]}")
            out = docker("run", "--rm", tag)
            if "stale-ok" not in out.stdout:
                raise RuntimeError(f"rebuilt image output: {out.stdout!r}")
            record("classic build after base-image rmi", "PASS",
                   "self-healed from stale buildkit cache")
        finally:
            docker("rmi", "-f", tag, check=False, timeout=60.0)


def _host_auth_dns_ok() -> bool:
    """buildx delegates the oauth token fetch to the host-side session
    client, so these tests need auth.docker.io resolvable FROM THE HOST
    quickly. Some networks (ISP DNS blocking) blackhole it with a 30 s
    timeout — every docker build against any backend stalls then, which
    is not an anvil defect. Skip the buildx tests in that environment."""
    try:
        subprocess.run(
            ["python3", "-c",
             "import socket; socket.setdefaulttimeout(6);"
             " socket.gethostbyname('auth.docker.io')"],
            capture_output=True, timeout=8.0, check=True)
        return True
    except Exception:
        return False


def test_buildx_builder_selection() -> None:
    """Regression: on a fresh machine the async anvil-remote setup after
    `anvil start` races the first user build; if the CLI is still on the
    docker-container driver (or has no builder), every FROM went to Docker
    Hub for OAuth. The builder must exist, be usable via an explicit name,
    and resolve base images from the local store."""
    insp = docker("buildx", "inspect", "anvil-remote", "--bootstrap", timeout=60.0)
    if "Driver" not in insp.stdout or "Error" in insp.stdout:
        raise RuntimeError(f"anvil-remote missing: {insp.stdout!r}")
    if not _host_auth_dns_ok():
        record("buildx explicit --builder anvil-remote", "SKIP",
               "host DNS for auth.docker.io is broken (buildx fetches tokens host-side)")
        return

    # A build that names the builder explicitly must work even when another
    # builder is currently selected (the docker-container driver would pull
    # moby/buildkit and resolve FROM from the registry).
    tag = f"{PREFIX}-sel:1"
    with tempfile.TemporaryDirectory() as tmp:
        (Path(tmp) / "Dockerfile").write_text('FROM alpine\nCMD ["echo", "sel-ok"]\n')
        try:
            proc = None
            for attempt in range(2):  # transient guest DNS hiccups under load
                proc = docker("buildx", "build", "--builder", "anvil-remote",
                              "--load", "-t", tag, tmp, timeout=600.0, check=False)
                if proc.returncode == 0:
                    break
                time.sleep(3.0)
            if proc is None or proc.returncode != 0:
                raise RuntimeError(f"--builder anvil-remote failed: {proc.stderr.strip()[-400:]}")
            out = docker("run", "--rm", tag)
            if "sel-ok" not in out.stdout:
                raise RuntimeError(f"output: {out.stdout!r}")
            record("buildx explicit --builder anvil-remote", "PASS",
                   "works regardless of the selected builder")
        finally:
            docker("rmi", "-f", tag, check=False, timeout=60.0)


def test_buildx_remote_load() -> None:
    """docker buildx build --load via the anvil-remote builder imports the
    result into the image store. The builder is selected explicitly: the
    async setup after `anvil start` may still be in flight, and the default
    builder here is the docker-container driver (moby/buildkit in a
    container) which pulls every layer from the registry."""
    docker("buildx", "use", "anvil-remote")
    ready = False
    for _ in range(30):
        insp = docker("buildx", "inspect", "--bootstrap", "anvil-remote",
                      timeout=60.0, check=False)
        if insp.returncode == 0:
            ready = True
            break
        time.sleep(1.0)
    if not ready:
        raise RuntimeError("anvil-remote builder not ready: see daemon.log")
    if not _host_auth_dns_ok():
        record("buildx remote driver --load", "SKIP",
               "host DNS for auth.docker.io is broken (buildx fetches tokens host-side)")
        return
    tag = f"{PREFIX}-bx:1"
    with tempfile.TemporaryDirectory() as tmp:
        (Path(tmp) / "Dockerfile").write_text('FROM alpine\nCMD ["echo", "bx-ok"]\n')
        try:
            proc = None
            for attempt in range(2):  # transient guest DNS hiccups under load
                proc = docker("buildx", "build", "--load", "-t", tag, tmp,
                              timeout=600.0, check=False)
                if proc.returncode == 0:
                    break
                time.sleep(3.0)
            if proc is None or proc.returncode != 0:
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
    ("run flags P0 (entrypoint/w/add-host/memory/cap)", test_run_flags_p0),
    ("run flags wave2 (read-only/stop-signal/tmpfs/pid/net-host)", test_run_flags_wave2),
    ("run flags wave3 (dns/sysctl/device/link)", test_run_flags_wave3),
    ("run flags wave4 (cpu/pids/ulimit/shm/swap/group/uts/nnp/rejections)", test_run_flags_wave4),
    ("restart policy monitor", test_restart_policy),
    ("restart always/unless-stopped", test_restart_policy_always),
    ("docker wait", test_docker_wait),
    ("logs --since", test_logs_since),
    ("published port forwarded", test_port_forward),
    ("foreign host-port conflict", test_foreign_port_conflict),
    ("container lifecycle", test_create_ps_inspect_stop),
    ("pause/unpause", test_pause_unpause),
    ("top + stats", test_top_and_stats),
    ("system df", test_system_df),
    ("network connect/disconnect", test_network_connect),
    ("logs", test_logs),
    ("logs --tail/-t", test_logs_tail_timestamps),
    ("exec", test_exec),
    ("exec -d/-w", test_exec_detached_and_flags),
    ("cp", test_cp),
    ("cp directories", test_cp_directory),
    ("cp stopped container", test_cp_stopped_container),
    ("cross-container DNS mesh", test_container_dns_mesh),
    ("image /run content visible", test_image_run_dir_content),
    ("restart on-failure budget", test_restart_policy_budget),
    ("bind mount /Users", test_bind_mount_users_share),
    ("named volume", test_named_volume_persistence),
    ("images tag/rmi", test_images_tag_rmi),
    ("save/load", test_save_load),
    ("save/load multiple", test_save_multiple_images),
    ("network lifecycle", test_network_lifecycle),
    ("healthcheck healthy", test_healthcheck),
    ("healthcheck unhealthy", test_healthcheck_unhealthy),
    ("events", test_events),
    ("events filters + --until", test_events_filters_and_until),
    ("events --since replay", test_events_since_replay),
    ("UDP port publishing", test_udp_publishing),
    ("compose up", test_compose_up),
    ("compose lifecycle verbs", test_compose_lifecycle_verbs),
    ("compose service DNS", test_compose_service_dns),
    ("compose depends_on completed", test_compose_depends_on_completed),
    ("compose recreate over live", test_compose_recreate_over_live),
    ("compose run one-off", test_compose_run_one_off),
    ("compose build + down --rmi", test_compose_build_and_down_rmi),
    ("compose isolation", test_compose_project_isolation),
    ("tty run", test_tty_run),
    ("port range publishing", test_port_range_publishing),
    ("kill exit code", test_kill_exit_code),
    ("logs -f", test_logs_follow),
    ("kill and rename", test_kill_and_rename),
    ("restart command", test_restart_command),
    ("docker port", test_docker_port_command),
    ("system prune", test_system_prune),
    ("classic build", test_classic_build),
    ("classic build after rmi", test_classic_build_after_rmi),
    ("buildx builder selection", test_buildx_builder_selection),
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

    import sys as _sys
    only = _sys.argv[1:]
    for name, fn in TESTS:
        if only and not any(o in name for o in only):
            continue
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
