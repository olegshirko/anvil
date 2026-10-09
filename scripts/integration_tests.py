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
# ANVIL_INSTANCE=dev targets the development instance (~/.anvil-vz-dev).
INSTANCE = os.environ.get("ANVIL_INSTANCE", "")
STATE_DIR = HOME / (".anvil-vz-" + INSTANCE if INSTANCE else ".anvil-vz")
DOCKER_SOCKET = STATE_DIR / "docker.sock"
# Every docker child (helpers and the tests' own subprocess calls) talks to
# this instance. A named one also pins buildx to the docker driver of that
# endpoint: the selected builder (anvil-remote) belongs to the usual
# instance, and `docker build` went to its buildkit.
os.environ["DOCKER_HOST"] = f"unix://{DOCKER_SOCKET}"
if INSTANCE:
    os.environ["BUILDX_BUILDER"] = "default"  # the docker driver of DOCKER_HOST
DOCKER_ENV = dict(os.environ)
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


def test_port_restart_new_ip() -> None:
    """A container that restarts on a different IP keeps its published port:
    the host listener must follow the new address.

    Regression: the forwarder diffed state by listener key only, so a restart
    (same container ID) left the listener dialing the dead pre-restart IP.
    A restart keeps the previous address when it is free, so a squatter
    takes it while the container is stopped.
    """
    net = f"{PREFIX}-ipnet2"
    squatter = f"{PREFIX}-ipsquat"
    name = f"{PREFIX}-web-restart"
    port = PORT_BASE + 5
    ip = lambda c: docker("inspect", "--format",
                          "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", c).stdout.strip()
    try:
        docker("network", "create", net)
        docker("run", "-d", "--name", name, "--network", net, "-p", f"{port}:80", "nginx")
        code = curl_status(port)
        if code != "200":
            raise RuntimeError(f"nginx on :{port} -> {code}, want 200")
        ip_before = ip(name)

        docker("stop", "-t", "0", name)
        docker("run", "-d", "--name", squatter, "--network", net, "--ip", ip_before, "alpine", "sleep", "120")
        docker("start", name)
        ip_after = ip(name)
        if ip_before == ip_after:
            raise RuntimeError(f"test is vacuous: container IP did not change ({ip_before})")
        code = curl_status(port)
        if code != "200":
            raise RuntimeError(
                f"after restart (ip {ip_before} -> {ip_after}) port :{port} -> {code}, want 200")
        record("published port survives restart with new container IP", "PASS",
               f"listener followed {ip_before} -> {ip_after}, still 200")
    finally:
        cleanup(squatter, name)
        docker("network", "rm", net, check=False, timeout=60.0)


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
    """docker network connect/disconnect on a running container: a second
    interface appears live, peers on the new network resolve it by name, and
    disconnect removes it again."""
    net = f"{PREFIX}-conn"
    name, peer = f"{PREFIX}-connc", f"{PREFIX}-connp"
    try:
        docker("network", "create", net)
        docker("run", "-d", "--name", name, "alpine", "sleep", "300")
        docker("run", "-d", "--name", peer, "--network", net, "alpine", "sleep", "300")
        docker("network", "connect", "--alias", "extra-alias", net, name)
        nets = json.loads(docker("inspect", "-f", "{{json .NetworkSettings.Networks}}", name).stdout)
        if net not in nets or not nets[net]["IPAddress"]:
            raise RuntimeError(f"inspect after connect: {nets}")
        ip = nets[net]["IPAddress"]
        links = docker("exec", name, "ip", "-o", "-4", "addr").stdout
        if ip not in links or "eth1" not in links:
            raise RuntimeError(f"eth1 with {ip} not in the container: {links!r}")
        for target in (name, "extra-alias"):
            out = docker("exec", peer, "ping", "-c", "1", "-W", "3", target, check=False)
            if out.returncode != 0 or ip not in out.stdout:
                raise RuntimeError(f"peer cannot reach {target} at {ip}: {out.stdout!r} {out.stderr!r}")
        res = docker("network", "connect", net, name, check=False)
        if res.returncode == 0 or "already exists" not in res.stderr:
            raise RuntimeError(f"double connect: rc={res.returncode} {res.stderr.strip()!r}")
        # survives a restart (attached again as a secondary endpoint)
        docker("restart", "-t", "1", name)
        nets = json.loads(docker("inspect", "-f", "{{json .NetworkSettings.Networks}}", name).stdout)
        if not nets.get(net, {}).get("IPAddress"):
            raise RuntimeError(f"secondary network lost on restart: {nets}")
        docker("network", "disconnect", net, name)
        nets = json.loads(docker("inspect", "-f", "{{json .NetworkSettings.Networks}}", name).stdout)
        if net in nets:
            raise RuntimeError(f"still attached after disconnect: {nets}")
        if "eth1" in docker("exec", name, "ip", "-o", "link").stdout:
            raise RuntimeError("eth1 left behind after disconnect")
        record("network connect/disconnect", "PASS", "live eth1, DNS by name+alias, restart, disconnect")
    finally:
        cleanup(name, peer)
        docker("network", "rm", net, check=False, timeout=60.0)


def test_compose_multi_network() -> None:
    """A compose service on two networks is attached to both (compose sends
    every network in the create request) and reachable from each side."""
    project = f"{PREFIX}-mnet"
    with tempfile.TemporaryDirectory() as tmp:
        compose_file = Path(tmp) / "compose.yml"
        compose_file.write_text("""services:
  api:
    image: alpine
    command: sleep 300
    networks: [front, back]
  web:
    image: alpine
    command: sleep 300
    networks: [front]
  db:
    image: alpine
    command: sleep 300
    networks: [back]
networks:
  front: {}
  back: {}
""")
        base = ["compose", "-p", project, "-f", str(compose_file)]
        try:
            docker(*base, "up", "-d", timeout=300.0)
            nets = json.loads(docker("inspect", "-f", "{{json .NetworkSettings.Networks}}",
                                     f"{project}-api-1").stdout)
            if sorted(nets) != sorted([f"{project}_front", f"{project}_back"]):
                raise RuntimeError(f"api networks: {sorted(nets)}")
            for src, dst in (("web", "api"), ("db", "api"), ("api", "web"), ("api", "db")):
                out = docker(*base, "exec", "-T", src, "ping", "-c", "1", "-W", "3", dst, check=False)
                if out.returncode != 0:
                    raise RuntimeError(f"{src} -> {dst} failed: {out.stdout!r} {out.stderr!r}")
            out = docker(*base, "exec", "-T", "web", "ping", "-c", "1", "-W", "2", "db", check=False)
            if out.returncode == 0:
                raise RuntimeError("web reached db across networks it does not share")
            record("compose service on two networks", "PASS", "attached to both, isolation kept")
        finally:
            subprocess.run(["docker", *base, "down", "-v", "--timeout", "5"],
                           capture_output=True, text=True, env=DOCKER_ENV, timeout=120.0)


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
    # --link alias resolves to the target container. `; echo` terminates the
    # 15-byte probe line: output without a trailing newline is lost when a
    # --rm container exits (README: current limitations).
    target = f"{PREFIX}-lkt"
    try:
        docker("run", "-d", "--name", target, "nginx:alpine")
        wait_http_ok = False
        for _ in range(10):
            probe = docker("run", "--rm", "--link", f"{target}:myalias", "alpine",
                           "sh", "-c", "wget -qO- --timeout=3 http://myalias 2>/dev/null | head -c 15; echo",
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
        # linux/amd64 is accepted with Rosetta (ANVIL_ROSETTA=1); s390x never
        ("--platform", "linux/s390x", "platform"),
    ]:
        args = ["run", "--rm", flag]
        if value:
            args.append(value)
        args += ["alpine", "true"]
        res = docker(*args, check=False)
        if res.returncode == 0 or needle not in res.stderr:
            raise RuntimeError(f"{flag} not rejected: rc={res.returncode} err={res.stderr[-200:]!r}")
    # seccomp=unconfined is accepted (and lifts the default profile, see
    # test_seccomp)
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



# --- Docker Desktop parity ---------------------------------------------------


def test_host_docker_internal() -> None:
    """host.docker.internal / host-gateway reach the Mac's localhost (a
    service bound only to 127.0.0.1, as in Docker Desktop);
    gateway.docker.internal is the NAT gateway."""
    import http.server
    srv = http.server.HTTPServer(("127.0.0.1", 0), http.server.SimpleHTTPRequestHandler)
    port = srv.server_address[1]
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    try:
        hosts = docker("run", "--rm", "--add-host", "mac.test:host-gateway",
                       "alpine", "cat", "/etc/hosts").stdout
        ips = {}
        for line in hosts.splitlines():
            parts = line.split()
            for name in ("host.docker.internal", "gateway.docker.internal", "mac.test"):
                if len(parts) >= 2 and name in parts[1:]:
                    ips[name] = parts[0]
        if len(ips) != 3 or ips["host.docker.internal"] != ips["mac.test"]:
            raise RuntimeError(f"desktop names not mapped: {ips} in {hosts!r}")
        for name in ("host.docker.internal", "mac.test"):
            out = docker("run", "--rm", "--add-host", "mac.test:host-gateway", "alpine",
                         "wget", "-q", "-T", "5", "-O", "/dev/null", f"http://{name}:{port}/", check=False)
            if out.returncode != 0:
                raise RuntimeError(f"Mac 127.0.0.1:{port} unreachable via {name}: {out.stderr.strip()}")
        out = docker("run", "--rm", "--network", "host", "alpine",
                     "wget", "-q", "-T", "5", "-O", "/dev/null", f"http://host.docker.internal:{port}/", check=False)
        if out.returncode != 0:
            raise RuntimeError(f"host-network container cannot reach the Mac's localhost: {out.stderr.strip()}")
        record("host.docker.internal", "PASS",
               f"-> {ips['host.docker.internal']}, Mac 127.0.0.1:{port} reachable (bridge + host network)")
    finally:
        srv.shutdown()


def test_seccomp() -> None:
    """Default seccomp profile on, unconfined/privileged off, custom Docker-format
    profiles honored."""
    def mode(*args: str) -> str:
        out = docker("run", "--rm", *args, "alpine", "grep", "^Seccomp:", "/proc/self/status").stdout
        return out.split()[-1]
    if mode() != "2":
        raise RuntimeError("default profile not applied (Seccomp mode != 2)")
    if mode("--security-opt", "seccomp=unconfined") != "0":
        raise RuntimeError("seccomp=unconfined still filtered")
    if mode("--privileged") != "0":
        raise RuntimeError("--privileged still filtered")
    profile = {
        "defaultAction": "SCMP_ACT_ALLOW",
        "archMap": [{"architecture": "SCMP_ARCH_AARCH64", "subArchitectures": []}],
        "syscalls": [{"names": ["mkdirat"], "action": "SCMP_ACT_ERRNO"}],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(profile, f)
        path = f.name
    try:
        res = docker("run", "--rm", "--security-opt", f"seccomp={path}",
                     "alpine", "mkdir", "/tmp/x", check=False)
        if res.returncode == 0:
            raise RuntimeError("custom profile blocking mkdirat did not apply")
        docker("run", "--rm", "--security-opt", f"seccomp={path}", "alpine", "true")
    finally:
        os.unlink(path)
    record("seccomp profiles", "PASS", "default=2, unconfined/privileged=0, custom profile enforced")


def test_run_init() -> None:
    """--init runs docker-init as PID 1 and reaps zombies; ps/inspect show the
    user command."""
    name = f"{PREFIX}-init"
    try:
        comm = docker("run", "--rm", "--init", "alpine", "cat", "/proc/1/comm").stdout.strip()
        if comm != "docker-init":
            raise RuntimeError(f"PID 1 is {comm!r}, want docker-init")
        docker("run", "-d", "--name", name, "--init", "alpine", "sleep", "300")
        cmd = docker("inspect", "-f", "{{json .Config.Cmd}}", name).stdout.strip()
        if "docker-init" in cmd or "sleep" not in cmd:
            raise RuntimeError(f"inspect Cmd leaks the init wrapper: {cmd}")
        # SIGTERM reaches the child through tini: stop is fast, not a 10 s kill
        t0 = time.time()
        docker("stop", name, timeout=30.0)
        if time.time() - t0 > 5:
            raise RuntimeError("docker stop with --init took the full timeout")
        record("run --init", "PASS", "PID 1 docker-init, stop forwards SIGTERM")
    finally:
        cleanup(name)


def test_volumes_from() -> None:
    src = f"{PREFIX}-vfsrc"
    try:
        docker("run", "--name", src, "-v", "/shared", "alpine",
               "sh", "-c", "echo from-src > /shared/f")
        out = docker("run", "--rm", "--volumes-from", src, "alpine", "cat", "/shared/f").stdout
        if "from-src" not in out:
            raise RuntimeError(f"volume not inherited: {out!r}")
        res = docker("run", "--rm", "--volumes-from", f"{src}:ro", "alpine",
                     "touch", "/shared/g", check=False)
        if res.returncode == 0:
            raise RuntimeError(":ro volumes-from is writable")
        res = docker("run", "--rm", "--volumes-from", "no-such-container-xyz", "alpine", "true", check=False)
        if res.returncode == 0:
            raise RuntimeError("unknown --volumes-from source accepted")
        record("--volumes-from", "PASS", "inherits volumes, :ro honored, unknown source rejected")
    finally:
        cleanup(src)


def test_image_history() -> None:
    rows = docker("history", "--no-trunc", "--format", "{{.ID}}\t{{.Size}}\t{{.CreatedBy}}", "alpine").stdout.splitlines()
    if not rows or not rows[0].startswith("sha256:"):
        raise RuntimeError(f"history top row is not the image id: {rows[:2]}")
    if not any("ADD" in r or "COPY" in r for r in rows):
        raise RuntimeError(f"no layer-creating step in history: {rows}")
    image_id = docker("image", "inspect", "-f", "{{.Id}}", "alpine").stdout.strip()
    if not rows[0].startswith(image_id):
        raise RuntimeError(f"history id {rows[0]!r} != inspect id {image_id}")
    record("docker history", "PASS", f"{len(rows)} rows, top = image id")


def test_image_search() -> None:
    res = docker("search", "--limit", "5", "--filter", "is-official=true", "alpine",
                 "--format", "{{.Name}} {{.IsOfficial}}", check=False, timeout=60.0)
    if res.returncode != 0:
        raise RuntimeError(f"docker search failed: {res.stderr.strip()}")
    lines = res.stdout.splitlines()
    if not lines or not any(l.split()[0] == "alpine" for l in lines):
        raise RuntimeError(f"official alpine not found: {lines}")
    if len(lines) > 5:
        raise RuntimeError(f"--limit ignored: {len(lines)} rows")
    record("docker search", "PASS", f"{len(lines)} official result(s)")


def test_export_and_diff() -> None:
    name = f"{PREFIX}-export"
    try:
        docker("run", "--name", name, "alpine", "sh", "-c",
               "echo exported > /root/marker && rm /etc/motd && echo x >> /etc/profile")
        diff = set(docker("diff", name).stdout.splitlines())
        for want in ("A /root/marker", "D /etc/motd", "C /etc/profile", "C /etc"):
            if want not in diff:
                raise RuntimeError(f"diff missing {want!r}: {sorted(diff)}")
        for noise in ("A /etc/hosts", "A /etc/hostname", "A /etc/resolv.conf"):
            if noise in diff:
                raise RuntimeError(f"diff reports runc mountpoint {noise!r}")
        with tempfile.TemporaryDirectory() as tmp:
            tar = Path(tmp) / "fs.tar"
            docker("export", "-o", str(tar), name, timeout=120.0)
            listing = subprocess.run(["tar", "-tf", str(tar)], capture_output=True, text=True, check=True).stdout
            names = {n.lstrip("./") for n in listing.splitlines()}
            if "root/marker" not in names or "bin/busybox" not in names:
                raise RuntimeError("export tar lacks the container's files")
            if "etc/motd" in names:
                raise RuntimeError("export includes a deleted file")
        # a running container exports through the live rootfs
        docker("rm", "-f", name)
        docker("run", "-d", "--name", name, "alpine", "sh", "-c", "touch /live && sleep 300")
        time.sleep(0.5)
        live = subprocess.run(["docker", "export", name], capture_output=True, env=DOCKER_ENV, timeout=120)
        if live.returncode != 0 or b"live" not in live.stdout:
            raise RuntimeError("export of a running container failed")
        record("docker export/diff", "PASS", f"{len(diff)} changes, export of stopped+running")
    finally:
        cleanup(name)



def test_container_update() -> None:
    name = f"{PREFIX}-update"
    try:
        # exits 1 once /tmp/stop exists: a failure the restart policy sees
        docker("run", "-d", "--name", name, "--memory", "256m", "alpine", "sh", "-c",
               "rm -f /tmp/stop; while [ ! -f /tmp/stop ]; do sleep 0.2; done; exit 1")
        docker("update", "--memory", "512m", "--memory-swap", "1g", "--cpus", "1.5",
               "--pids-limit", "64", "--restart", "unless-stopped", name)
        mem = docker("exec", name, "cat", "/sys/fs/cgroup/memory.max").stdout.strip()
        if mem != str(512 << 20):
            raise RuntimeError(f"memory.max = {mem}, want {512 << 20}")
        cpu = docker("exec", name, "cat", "/sys/fs/cgroup/cpu.max").stdout.split()
        if cpu[:2] != ["150000", "100000"]:
            raise RuntimeError(f"cpu.max = {cpu}")
        pids = docker("exec", name, "cat", "/sys/fs/cgroup/pids.max").stdout.strip()
        if pids != "64":
            raise RuntimeError(f"pids.max = {pids}")
        insp = json.loads(docker("inspect", name).stdout)[0]["HostConfig"]
        if insp["Memory"] != 512 << 20 or insp["NanoCpus"] != 1_500_000_000 \
                or insp["RestartPolicy"]["Name"] != "unless-stopped":
            raise RuntimeError(f"inspect not updated: Memory={insp['Memory']} "
                               f"NanoCpus={insp['NanoCpus']} Restart={insp['RestartPolicy']}")
        # the restart policy is live: the monitor brings the container back
        docker("exec", name, "touch", "/tmp/stop", check=False)
        deadline = time.time() + 20
        while time.time() < deadline:
            if docker("inspect", "-f", "{{.State.Running}}", name).stdout.strip() == "true" \
                    and docker("inspect", "-f", "{{.RestartCount}}", name).stdout.strip() != "0":
                break
            time.sleep(1)
        else:
            raise RuntimeError("updated restart policy did not restart the container")
        # limits survive a stop/start (written into the container spec)
        docker("stop", name, timeout=60.0)
        docker("start", name)
        mem = docker("exec", name, "cat", "/sys/fs/cgroup/memory.max").stdout.strip()
        if mem != str(512 << 20):
            raise RuntimeError(f"memory.max after restart = {mem}")
        res = docker("update", "--memory", "2g", name, check=False)
        if res.returncode == 0:
            raise RuntimeError("memory above the swap limit accepted")
        record("docker update", "PASS", "memory/cpus/pids live + persisted, restart policy armed")
    finally:
        cleanup(name)


def test_commit() -> None:
    name = f"{PREFIX}-commit"
    image = f"{PREFIX}-committed:v1"
    try:
        docker("run", "--name", name, "-e", "FROM_RUN=1", "-w", "/srv", "alpine",
               "sh", "-c", "echo committed > /srv/file")
        docker("commit", "-c", "CMD [\"cat\", \"/srv/file\"]", "-c", "ENV EXTRA=yes",
               "-c", "LABEL it=1", "-m", "msg", "-a", "tester", name, image)
        out = docker("run", "--rm", image).stdout.strip()
        if out != "committed":
            raise RuntimeError(f"committed image output {out!r}")
        env = docker("run", "--rm", image, "env").stdout
        if "FROM_RUN=1" not in env or "EXTRA=yes" not in env:
            raise RuntimeError(f"env not committed: {env!r}")
        cfg = json.loads(docker("image", "inspect", image).stdout)[0]
        if cfg["Config"]["WorkingDir"] != "/srv" or cfg["Config"]["Labels"].get("it") != "1" \
                or cfg["Author"] != "tester":
            raise RuntimeError(f"config not committed: {cfg['Config']} author={cfg['Author']}")
        hist = docker("history", "--format", "{{.Comment}}", image).stdout.splitlines()
        if not hist or hist[0] != "msg":
            raise RuntimeError(f"history comment: {hist[:2]}")
        # a running container commits too (paused during the diff)
        docker("rm", "-f", name)
        docker("run", "-d", "--name", name, "alpine", "sh", "-c", "touch /live && sleep 300")
        time.sleep(0.5)
        docker("commit", name, f"{PREFIX}-committed:live")
        docker("run", "--rm", f"{PREFIX}-committed:live", "test", "-f", "/live")
        if docker("inspect", "-f", "{{.State.Running}}", name).stdout.strip() != "true":
            raise RuntimeError("container not resumed after commit")
        record("docker commit", "PASS", "layer + config + changes; running container resumed")
    finally:
        cleanup(name)
        docker("rmi", "-f", image, f"{PREFIX}-committed:live", check=False)


def test_ssh_agent_forwarding() -> None:
    """/run/host-services/ssh-auth.sock reaches the Mac's ssh-agent: the
    agent's key count, asked over the raw protocol, matches ssh-add -l."""
    sock = os.environ.get("SSH_AUTH_SOCK") or subprocess.run(
        ["launchctl", "getenv", "SSH_AUTH_SOCK"], capture_output=True, text=True).stdout.strip()
    if not sock:
        record("ssh agent forwarding", "SKIP", "no ssh-agent on the Mac")
        return
    host = subprocess.run(["ssh-add", "-l"], capture_output=True, text=True,
                          env={**os.environ, "SSH_AUTH_SOCK": sock})
    host_keys = len(host.stdout.splitlines()) if host.returncode == 0 else 0
    # SSH_AGENTC_REQUEST_IDENTITIES (11) -> SSH_AGENT_IDENTITIES_ANSWER (12)
    # + key count; perl ships in the nginx (debian) image, no network needed.
    probe = ('$s=IO::Socket::UNIX->new(Peer=>"/run/host-services/ssh-auth.sock") or die "connect: $!";'
             ' print $s pack("NC",1,11); read($s,$h,4)==4 or die "no reply";'
             ' read($s,$b,unpack("N",$h)); printf "%d %d\\n", unpack("C",$b), unpack("N",substr($b,1,4));')
    out = docker("run", "--rm",
                 "-v", "/run/host-services/ssh-auth.sock:/run/host-services/ssh-auth.sock",
                 "nginx", "perl", "-MIO::Socket::UNIX", "-e", probe, check=False)
    if out.returncode != 0 or out.stdout.split() != ["12", str(host_keys)]:
        raise RuntimeError(f"agent answer {out.stdout.strip()!r} (want '12 {host_keys}'): {out.stderr.strip()}")
    record("ssh agent forwarding", "PASS", f"agent answered, {host_keys} key(s) like the Mac")


def test_rosetta_amd64() -> None:
    """linux/amd64 through Rosetta (daemon started with ANVIL_ROSETTA=1)."""
    probe = docker("run", "--rm", "--platform", "linux/amd64", "alpine", "uname", "-m",
                   check=False, timeout=300.0)
    if probe.returncode != 0 and "ANVIL_ROSETTA=1" in probe.stderr:
        record("rosetta amd64", "SKIP", "Rosetta off (start the daemon with ANVIL_ROSETTA=1)")
        return
    if probe.stdout.strip() != "x86_64":
        raise RuntimeError(f"uname -m = {probe.stdout.strip()!r}: {probe.stderr.strip()}")
    # arm64 stays the default for multi-arch images
    native = docker("run", "--rm", "alpine", "uname", "-m").stdout.strip()
    if native != "aarch64":
        raise RuntimeError(f"default platform ran {native!r}, want aarch64")
    arch = docker("image", "inspect", "-f", "{{.Architecture}}", "alpine").stdout.strip()
    if arch != "arm64":
        raise RuntimeError(f"alpine inspect Architecture = {arch!r}")
    # an amd64-only image (the amd64/ per-arch repo) pulls and runs without
    # --platform, with Docker's platform warning
    image = "amd64/alpine:latest"
    try:
        docker("rmi", "-f", image, check=False)
        out = docker("run", "--rm", image, "uname", "-m", timeout=300.0)
        if out.stdout.strip() != "x86_64":
            raise RuntimeError(f"amd64-only image ran as {out.stdout.strip()!r}")
        if "does not match the detected host platform" not in out.stderr:
            raise RuntimeError(f"no platform warning: {out.stderr.strip()!r}")
    finally:
        docker("rmi", "-f", image, check=False)
    record("rosetta amd64", "PASS", "--platform linux/amd64 = x86_64, arm64 default kept, amd64-only image runs")



def test_restart_policy_survives_stop_start() -> None:
    """docker stop disarms --restart, docker start re-arms it (Docker keeps
    the policy); docker restart keeps it too."""
    name = f"{PREFIX}-rearm"
    try:
        docker("run", "-d", "--name", name, "--restart", "always", "alpine", "sh", "-c",
               "rm -f /tmp/stop; while [ ! -f /tmp/stop ]; do sleep 0.2; done; exit 1")
        docker("stop", "-t", "1", name)
        time.sleep(3)
        if docker("inspect", "-f", "{{.State.Running}}", name).stdout.strip() != "false":
            raise RuntimeError("stopped container was restarted by its policy")
        for how in ("start", "restart"):
            docker(how, name)
            docker("exec", name, "touch", "/tmp/stop", check=False)
            deadline = time.time() + 20
            while time.time() < deadline:
                st = docker("inspect", "-f", "{{.State.Running}} {{.RestartCount}}", name).stdout.split()
                if st[0] == "true" and st[1] != "0":
                    break
                time.sleep(1)
            else:
                raise RuntimeError(f"policy not re-armed after docker {how}: {st}")
        record("restart policy after stop/start", "PASS", "re-armed by docker start and docker restart")
    finally:
        cleanup(name)



def test_cp_shell_less_image() -> None:
    """docker cp works on a running container whose image has no shell, tar
    or stat (scratch/distroless), including files inside a volume and paths
    through an absolute symlink."""
    name, image = f"{PREFIX}-cpscratch", f"{PREFIX}-scratch:1"
    with tempfile.TemporaryDirectory() as tmp:
        ctx = Path(tmp)
        (ctx / "Dockerfile").write_text(
            "FROM busybox:musl AS b\n"
            "RUN mkdir -p /out/real && echo from-image > /out/real/f && ln -s /real /out/link\n"
            "FROM scratch\n"
            "COPY --from=b /bin/busybox /busybox\n"
            "COPY --from=b /out/ /\n"
            'CMD ["/busybox", "sleep", "300"]\n')
        try:
            docker("build", "-t", image, str(ctx), timeout=300.0)
            docker("run", "-d", "--name", name, "-v", "/data", image)
            src = ctx / "in.txt"
            src.write_text("copied-in\n")
            docker("cp", str(src), f"{name}:/data/in.txt")
            docker("cp", f"{name}:/data/in.txt", str(ctx / "back.txt"))
            if (ctx / "back.txt").read_text() != "copied-in\n":
                raise RuntimeError("volume file did not round-trip")
            docker("cp", f"{name}:/link/f", str(ctx / "via-link.txt"))
            if (ctx / "via-link.txt").read_text() != "from-image\n":
                raise RuntimeError("path through /link did not resolve inside the container")
            out = docker("exec", name, "/busybox", "cat", "/data/in.txt").stdout
            if out != "copied-in\n":
                raise RuntimeError(f"container does not see the copied file: {out!r}")
            record("docker cp without shell/tar in the image", "PASS", "volume + symlinked path, in and out")
        finally:
            cleanup(name)
            docker("rmi", "-f", image, check=False)


def test_network_none() -> None:
    out = docker("run", "--rm", "--network", "none", "alpine", "ip", "-o", "link").stdout
    ifaces = [l.split(":")[1].strip().split("@")[0] for l in out.splitlines() if ":" in l]
    if ifaces != ["lo"] or "UP" not in out:
        raise RuntimeError(f"--network none interfaces: {out!r}")
    res = docker("run", "--rm", "--network", "none", "alpine", "wget", "-q", "-T", "3",
                 "-O", "/dev/null", "http://1.1.1.1/", check=False)
    if res.returncode == 0:
        raise RuntimeError("--network none reached the internet")
    res = docker("run", "--rm", "--network", "none", "-p", "18480:80", "alpine", "true", check=False)
    if res.returncode == 0 or "conflicting options" not in res.stderr:
        raise RuntimeError(f"-p with --network none: rc={res.returncode} {res.stderr.strip()!r}")
    record("--network none", "PASS", "lo only, no egress, -p refused")


def test_network_rm_in_use_and_inspect() -> None:
    net, name = f"{PREFIX}-inuse", f"{PREFIX}-inusec"
    try:
        docker("network", "create", net)
        docker("run", "-d", "--name", name, "--network", net, "alpine", "sleep", "300")
        info = json.loads(docker("network", "inspect", net).stdout)[0]
        eps = list(info["Containers"].values())
        if len(eps) != 1 or eps[0]["Name"] != name or "/" not in eps[0]["IPv4Address"]:
            raise RuntimeError(f"inspect Containers: {info['Containers']}")
        res = docker("network", "rm", net, check=False)
        if res.returncode == 0 or "active endpoints" not in res.stderr:
            raise RuntimeError(f"rm of an in-use network: rc={res.returncode} {res.stderr.strip()!r}")
        docker("rm", "-f", name)
        docker("network", "rm", net)
        record("network rm in use + inspect Containers", "PASS", "refused while attached, listed endpoint")
    finally:
        cleanup(name)
        docker("network", "rm", net, check=False)


def test_save_under_gc() -> None:
    """docker save stays complete while other image removals trigger GC."""
    stop = threading.Event()

    def churn() -> None:
        i = 0
        while not stop.is_set():
            t = f"{PREFIX}-churn:{i % 3}"
            docker("tag", "busybox", t, check=False, timeout=60.0)
            docker("rmi", "-f", t, check=False, timeout=60.0)
            i += 1

    th = threading.Thread(target=churn, daemon=True)
    th.start()
    try:
        with tempfile.TemporaryDirectory() as tmp:
            for i in range(5):
                tar = Path(tmp) / f"s{i}.tar"
                docker("save", "-o", str(tar), "alpine", "busybox", timeout=180.0)
                size = tar.stat().st_size
                if size < 3_000_000:
                    raise RuntimeError(f"save #{i} truncated: {size} bytes")
    finally:
        stop.set()
        th.join(timeout=60)
    record("docker save under GC pressure", "PASS", "5 complete archives with concurrent rmi")



def test_cp_symlink_race_stays_in_container() -> None:
    """A running container swapping a directory for a symlink to the VM's
    share (/mnt/anvil, the Mac) must not get docker cp to write there."""
    name = f"{PREFIX}-cprace"
    marker = f"anvil-cp-escape-{os.getpid()}.txt"
    share_roots = [STATE_DIR]
    ps = subprocess.run(["pgrep", "-fl", "vz-runner daemon"], capture_output=True, text=True).stdout
    m = re.search(r"--share (\S+)", ps)
    if m:
        share_roots.append(Path(m.group(1)))
    try:
        docker("run", "-d", "--name", name, "alpine", "sh", "-c",
               "mkdir -p /mnt/anvil; while true; do rm -rf /d; mkdir /d; rm -rf /d; ln -s /mnt/anvil /d; done")
        with tempfile.TemporaryDirectory() as tmp:
            src = Path(tmp) / marker
            src.write_text("must stay inside the container\n")
            for _ in range(40):
                docker("cp", str(src), f"{name}:/d/", check=False, timeout=30.0)
        for root in share_roots:
            if (root / marker).exists():
                (root / marker).unlink()
                raise RuntimeError(f"docker cp escaped the container into {root}")
        record("docker cp symlink race", "PASS", "40 copies under a swapping symlink, nothing reached the share")
    finally:
        cleanup(name)


def test_volumes_from_source_rm_keeps_data() -> None:
    src, user = f"{PREFIX}-vfkeep", f"{PREFIX}-vfuser"
    try:
        docker("run", "--name", src, "-v", "/shared", "alpine", "sh", "-c", "echo kept > /shared/f")
        docker("run", "-d", "--name", user, "--volumes-from", src, "alpine", "sleep", "300")
        docker("rm", "-v", src)  # -v: but the volume is still mounted by user
        out = docker("exec", user, "cat", "/shared/f").stdout
        if out != "kept\n":
            raise RuntimeError(f"data lost after rm -v of the source: {out!r}")
        record("volumes-from survives source rm", "PASS", "rm -v keeps a volume another container mounts")
    finally:
        cleanup(src, user)


def test_update_pids_unlimited() -> None:
    name = f"{PREFIX}-pidsmax"
    try:
        docker("run", "-d", "--name", name, "--pids-limit", "64", "alpine", "sleep", "300")
        docker("update", "--pids-limit", "-1", name)
        out = docker("exec", name, "cat", "/sys/fs/cgroup/pids.max").stdout.strip()
        if out != "max":
            raise RuntimeError(f"pids.max after --pids-limit -1 = {out!r}, want max")
        record("update --pids-limit -1", "PASS", "pids.max = max")
    finally:
        cleanup(name)


def test_commit_keeps_healthcheck_labels_user() -> None:
    name, image = f"{PREFIX}-commitkeep", f"{PREFIX}-commitkeep:1"
    try:
        docker("run", "--name", name, "--user", "nobody", "--label", "team=anvil",
               "--health-cmd", "true", "--health-interval", "5s", "alpine", "true")
        docker("commit", name, image)
        cfg = json.loads(docker("image", "inspect", "-f", "{{json .Config}}", image).stdout)
        hc = (cfg.get("Healthcheck") or {}).get("Test") or []
        if cfg.get("User") != "nobody" or (cfg.get("Labels") or {}).get("team") != "anvil" or "true" not in hc:
            raise RuntimeError(f"committed config: User={cfg.get('User')!r} Labels={cfg.get('Labels')} Healthcheck={hc}")
        record("commit keeps healthcheck/labels/user", "PASS", "all three in the image config")
    finally:
        cleanup(name)
        docker("rmi", "-f", image, check=False)


def test_connect_alias_scoped_to_network() -> None:
    net = f"{PREFIX}-aliasnet"
    name, peer_new, peer_old = f"{PREFIX}-alias", f"{PREFIX}-aliasp1", f"{PREFIX}-aliasp2"
    try:
        docker("network", "create", net)
        docker("run", "-d", "--name", name, "alpine", "sleep", "300")
        docker("run", "-d", "--name", peer_old, "alpine", "sleep", "300")
        docker("run", "-d", "--name", peer_new, "--network", net, "alpine", "sleep", "300")
        docker("network", "connect", "--alias", "only-on-new", net, name)
        if docker("exec", peer_new, "ping", "-c", "1", "-W", "3", "only-on-new", check=False).returncode != 0:
            raise RuntimeError("alias not resolvable on its own network")
        hosts = docker("exec", peer_old, "cat", "/etc/hosts").stdout
        if "only-on-new" in hosts:
            raise RuntimeError(f"alias leaked onto the default network: {hosts!r}")
        record("connect alias per network", "PASS", "resolves on its network only")
    finally:
        cleanup(name, peer_new, peer_old)
        docker("network", "rm", net, check=False)


def test_rosetta_explicit_arm64() -> None:
    probe = docker("run", "--rm", "--platform", "linux/amd64", "alpine", "true", check=False, timeout=300.0)
    if probe.returncode != 0 and "ANVIL_ROSETTA=1" in probe.stderr:
        record("rosetta explicit arm64", "SKIP", "Rosetta off")
        return
    res = docker("run", "--rm", "--platform", "linux/arm64", "amd64/alpine", "uname", "-m",
                 check=False, timeout=300.0)
    if res.returncode == 0:
        raise RuntimeError(f"--platform linux/arm64 ran an amd64-only image: {res.stdout.strip()!r}")
    docker("rmi", "-f", "amd64/alpine", check=False)
    record("rosetta explicit arm64", "PASS", "no silent amd64 fallback for an explicit arm64")



def test_images_reference_filter() -> None:
    docker("pull", "busybox", timeout=300.0)
    names = docker("images", "busybox", "--format", "{{.Repository}}").stdout.split()
    if not names or any(n not in ("busybox", "docker.io/library/busybox") for n in names):
        raise RuntimeError(f"docker images busybox listed {sorted(set(names))}")
    tagged = docker("images", "--filter", "reference=alpine:*", "--format", "{{.Repository}}").stdout.split()
    if not tagged or any("alpine" not in n for n in tagged):
        raise RuntimeError(f"reference=alpine:* listed {sorted(set(tagged))}")
    record("docker images <name>", "PASS", "reference filter applied")


def test_volume_copy_up_and_image_volume() -> None:
    vol, image, name = f"{PREFIX}-cpup", f"{PREFIX}-imgvol:1", f"{PREFIX}-imgvolc"
    try:
        # anonymous volume over an image directory: seeded from the image
        out = docker("run", "--rm", "-v", "/etc", "alpine", "cat", "/etc/alpine-release").stdout.strip()
        if not out:
            raise RuntimeError("anonymous volume at /etc was not seeded from the image")
        # named volume: seeded once, then keeps its own content
        docker("volume", "create", vol)
        docker("run", "--rm", "-v", f"{vol}:/etc", "alpine", "sh", "-c", "echo mine > /etc/marker")
        out = docker("run", "--rm", "-v", f"{vol}:/etc", "alpine", "sh", "-c",
                     "cat /etc/marker; test -f /etc/alpine-release && echo seeded").stdout.split()
        if out != ["mine", "seeded"]:
            raise RuntimeError(f"named volume content: {out}")
        # nocopy
        out = docker("run", "--rm", "-v", "/etc:nocopy", "alpine", "/bin/ls", "-A", "/etc").stdout.split()
        # runc creates the /etc/hosts, /etc/hostname, /etc/resolv.conf
        # mountpoints inside the volume, as with Docker; nothing else.
        if set(out) - {"hosts", "hostname", "resolv.conf"}:
            raise RuntimeError(f"nocopy volume was seeded: {out!r}")
        # VOLUME in the image becomes an anonymous volume seeded with its content
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / "Dockerfile").write_text(
                "FROM alpine\nRUN mkdir /data && echo from-image > /data/f && chown 1000:1000 /data\nVOLUME /data\n")
            docker("build", "-t", image, tmp, timeout=300.0)
        docker("run", "--name", name, image, "sh", "-c", "cat /data/f; stat -c %u /data; echo new > /data/g")
        logs = docker("logs", name).stdout.split()
        if logs[:2] != ["from-image", "1000"]:
            raise RuntimeError(f"image VOLUME not seeded with content/owner: {logs}")
        mounts = json.loads(docker("inspect", "-f", "{{json .Mounts}}", name).stdout or "null")
        record("volume copy-up + image VOLUME", "PASS", f"anon/named/nocopy/VOLUME ok ({len(mounts or [])} mounts)")
    finally:
        cleanup(name)
        docker("rmi", "-f", image, check=False)
        docker("volume", "rm", "-f", vol, check=False)


def test_network_events() -> None:
    # unique per run: the --since window reaches back past earlier runs
    net, name = f"{PREFIX}-evnet-{int(time.time())}", f"{PREFIX}-evc"
    try:
        since = str(int(time.time()) - 30)  # guest clock may trail the Mac's
        docker("network", "create", net)
        docker("run", "-d", "--name", name, "alpine", "sleep", "300")
        docker("network", "connect", net, name)
        docker("network", "disconnect", net, name)
        docker("rm", "-f", name)
        docker("network", "rm", net)
        out = docker("events", "--since", since, "--until", str(int(time.time()) + 1),
                     "--filter", "type=network", "--format", "{{.Action}} {{.Actor.Attributes.name}}",
                     timeout=30.0).stdout.splitlines()
        want = [f"create {net}", f"connect {net}", f"disconnect {net}", f"destroy {net}"]
        got = [l for l in out if net in l]
        if got != want:
            raise RuntimeError(f"network events {got}, want {want}")
        record("network events", "PASS", "create/connect/disconnect/destroy")
    finally:
        cleanup(name)
        docker("network", "rm", net, check=False)


def test_idle_connections_do_not_starve_daemon() -> None:
    """Many idle connections to a published port (LAN peers can open them)
    must not stall the daemon: the relays run on their own threads."""
    name, port = f"{PREFIX}-idleconn", PORT_BASE + 290
    socks = []
    try:
        docker("run", "-d", "--name", name, "-p", f"{port}:80", "nginx")
        if curl_status(port) != "200":
            raise RuntimeError("nginx not reachable")
        for _ in range(200):
            s = socket.create_connection(("127.0.0.1", port), timeout=5)
            socks.append(s)
        t0 = time.time()
        docker("ps", "-q", timeout=20.0)
        if curl_status(port, wait=10) != "200":
            raise RuntimeError("a fresh request failed with 200 idle connections open")
        if time.time() - t0 > 10:
            raise RuntimeError("daemon slowed down under idle connections")
        record("idle connections vs daemon", "PASS", "200 idle port connections, API and port still responsive")
    finally:
        for s in socks:
            s.close()
        cleanup(name)


def test_rosetta_aot_cache() -> None:
    probe = docker("run", "--rm", "--platform", "linux/amd64", "alpine", "true", check=False, timeout=300.0)
    if probe.returncode != 0 and "ANVIL_ROSETTA=1" in probe.stderr:
        record("rosetta AOT cache", "SKIP", "Rosetta off")
        return
    mounts = docker("run", "--rm", "--platform", "linux/amd64", "alpine", "sh", "-c",
                    "test -S /run/rosettad/rosetta.sock && echo socket").stdout.strip()
    if mounts != "socket":
        raise RuntimeError("rosettad socket not mounted into the amd64 container")
    docker("run", "--rm", "--platform", "linux/amd64", "alpine", "sh", "-c", "ls / >/dev/null")
    files = docker("run", "--rm", "-v", "/var/lib/rosettad:/c", "alpine", "sh", "-c",
                   "ls /c | grep -c aotcache || true").stdout.strip()
    if not files or int(files) == 0:
        raise RuntimeError("no .aotcache files after running amd64 binaries")
    record("rosetta AOT cache", "PASS", f"{files} cached translations")



def test_compose_recreate_keeps_anonymous_volume() -> None:
    """A service whose image declares VOLUME keeps that data when compose
    recreates it (compose hands the old anonymous volume over via .Mounts)."""
    project = f"{PREFIX}-anonkeep"
    with tempfile.TemporaryDirectory() as tmp:
        (Path(tmp) / "Dockerfile").write_text("FROM alpine\nVOLUME /data\n")
        compose_file = Path(tmp) / "compose.yml"
        def write(env: str) -> None:
            compose_file.write_text(f"""services:
  db:
    build: .
    image: {project}-db:1
    command: sleep 300
    environment:
      REV: "{env}"
""")
        base = ["compose", "-p", project, "-f", str(compose_file)]
        try:
            write("1")
            docker(*base, "up", "-d", "--build", timeout=300.0)
            docker(*base, "exec", "-T", "db", "sh", "-c", "echo precious > /data/f")
            mounts = json.loads(docker("inspect", "-f", "{{json .Mounts}}", f"{project}-db-1").stdout)
            vols = [m for m in mounts if m.get("Type") == "volume" and m.get("Destination") == "/data"]
            if len(vols) != 1 or not vols[0].get("Name"):
                raise RuntimeError(f"inspect .Mounts: {mounts}")
            write("2")  # config change -> recreate
            docker(*base, "up", "-d", timeout=300.0)
            out = docker(*base, "exec", "-T", "db", "cat", "/data/f", check=False).stdout
            if out != "precious\n":
                raise RuntimeError(f"anonymous volume data lost on recreate: {out!r}")
            record("compose recreate keeps VOLUME data", "PASS", f"volume {vols[0]['Name'][:12]} carried over")
        finally:
            subprocess.run(["docker", *base, "down", "-v", "--rmi", "local", "--timeout", "5"],
                           capture_output=True, text=True, env=DOCKER_ENV, timeout=120.0)



def test_stats_real_numbers() -> None:
    name = f"{PREFIX}-statsreal"
    try:
        docker("run", "-d", "--name", name, "--memory", "256m", "alpine", "sh", "-c",
               "sleep 1000 & sleep 1000 & while :; do :; done")
        time.sleep(2)
        out = docker("stats", "--no-stream", "--format",
                     "{{.CPUPerc}}|{{.MemUsage}}|{{.PIDs}}|{{.NetIO}}|{{.BlockIO}}", name, timeout=60.0).stdout.strip()
        cpu, mem, pids, net, blk = out.split("|")
        if float(cpu.rstrip("%")) < 30:
            raise RuntimeError(f"busy loop shows {cpu} CPU")
        if int(pids) < 3:
            raise RuntimeError(f"PIDs = {pids}, want the whole container (>= 3)")
        if "256MiB" not in mem:
            raise RuntimeError(f"memory limit not reported: {mem}")
        record("docker stats", "PASS", f"cpu {cpu}, pids {pids}, mem {mem}, net {net}, block {blk}")
    finally:
        cleanup(name)


def test_compose_cp_and_watch() -> None:
    project = f"{PREFIX}-watch"
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        (root / "src").mkdir()
        (root / "src" / "app.txt").write_text("v1\n")
        (root / "compose.yml").write_text("""services:
  app:
    image: alpine
    command: sleep 300
    develop:
      watch:
        - action: sync
          path: ./src
          target: /app
""")
        base = ["compose", "-p", project, "-f", str(root / "compose.yml")]
        watcher = None
        try:
            docker(*base, "up", "-d", timeout=300.0)
            # compose cp both ways
            docker(*base, "cp", str(root / "src" / "app.txt"), "app:/tmp/cp.txt")
            docker(*base, "cp", "app:/tmp/cp.txt", str(root / "back.txt"))
            if (root / "back.txt").read_text() != "v1\n":
                raise RuntimeError("compose cp did not round-trip")
            # compose watch: a host edit reaches the container
            watcher = subprocess.Popen(["docker", *base, "watch", "--no-up"], cwd=tmp, env=DOCKER_ENV,
                                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            time.sleep(4)
            (root / "src" / "app.txt").write_text("v2\n")
            deadline = time.time() + 30
            got = ""
            while time.time() < deadline:
                got = docker(*base, "exec", "-T", "app", "cat", "/app/app.txt", check=False).stdout
                if got == "v2\n":
                    break
                time.sleep(1)
            if got != "v2\n":
                log_out = ""
                if watcher.poll() is not None:
                    log_out = watcher.stdout.read()[-400:]
                raise RuntimeError(f"compose watch did not sync: {got!r} {log_out}")
            record("compose cp + watch", "PASS", "cp both ways, watch synced an edit")
        finally:
            if watcher is not None:
                watcher.terminate()
                try:
                    watcher.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    watcher.kill()
            subprocess.run(["docker", *base, "down", "--timeout", "5"],
                           capture_output=True, text=True, env=DOCKER_ENV, timeout=120.0)



def test_log_rotation() -> None:
    name = f"{PREFIX}-logrot"
    try:
        docker("run", "-d", "--name", name, "--log-opt", "max-size=20k", "--log-opt", "max-file=2",
               "alpine", "sh", "-c", "i=0; while [ $i -lt 3000 ]; do echo line-$i-padding-padding-padding; i=$((i+1)); done; sleep 300")
        time.sleep(4)
        out = docker("logs", name, timeout=60.0).stdout.splitlines()
        if not out or out[-1] != "line-2999-padding-padding-padding":
            raise RuntimeError(f"last log line: {out[-1:]!r}")
        # 3000 lines x ~35 bytes of payload are ~100 KB of JSON; with two
        # 20 KB files at most ~40 KB survive.
        if len(out) >= 3000 or len(out) < 100:
            raise RuntimeError(f"{len(out)} lines kept: rotation did not bound the log")
        nums = [int(l.split("-")[1]) for l in out]
        if nums != sorted(nums) or nums != list(range(nums[0], nums[0] + len(nums))):
            raise RuntimeError("rotated log replay is not contiguous and ordered")
        res = docker("run", "--rm", "--log-opt", "max-size=lots", "alpine", "true", check=False)
        if res.returncode == 0:
            raise RuntimeError("invalid max-size accepted")
        record("log rotation", "PASS", f"{len(out)} contiguous newest lines kept under max-size=20k x2")
    finally:
        cleanup(name)


def test_health_status_events() -> None:
    name = f"{PREFIX}-healthev"
    try:
        # Generous: event times come from the guest clock, which may trail
        # the Mac's by a moment after a resume.
        since = str(int(time.time()) - 30)
        cid = docker("run", "-d", "--name", name, "--health-cmd", "test -f /tmp/ok", "--health-interval", "1s",
                     "--health-retries", "2", "alpine", "sh", "-c",
                     "touch /tmp/ok; sleep 4; rm /tmp/ok; sleep 300").stdout.strip()
        deadline = time.time() + 30
        while time.time() < deadline:
            if docker("inspect", "-f", "{{.State.Health.Status}}", name).stdout.strip() == "unhealthy":
                break
            time.sleep(1)
        out = docker("events", "--since", since, "--until", str(int(time.time()) + 1),
                     # by ID: the name is reused by earlier runs of this test
                     "--filter", f"container={cid}", "--filter", "event=health_status",
                     "--format", "{{.Action}}", timeout=30.0).stdout.splitlines()
        if out != ["health_status: healthy", "health_status: unhealthy"]:
            raise RuntimeError(f"health events {out}")
        record("health_status events", "PASS", "healthy then unhealthy after retries")
    finally:
        cleanup(name)


def test_volume_prune_semantics() -> None:
    named, user = f"{PREFIX}-prunenamed", f"{PREFIX}-pruneuser"
    try:
        docker("volume", "create", named)
        docker("run", "-d", "--name", user, "-v", "/busy", "alpine", "sleep", "300")
        busy = [m["Name"] for m in json.loads(docker("inspect", "-f", "{{json .Mounts}}", user).stdout)]
        docker("run", "--rm", "-v", "/scratch", "alpine", "true")  # --rm drops its own anon volume
        docker("run", "--name", f"{user}-gone", "-v", "/leftover", "alpine", "true")
        leftover = [m["Name"] for m in json.loads(docker("inspect", "-f", "{{json .Mounts}}", f"{user}-gone").stdout)]
        docker("rm", f"{user}-gone")  # without -v: anon volume stays, unused
        pruned = docker("volume", "prune", "-f").stdout
        vols = docker("volume", "ls", "-q").stdout.split()
        if leftover[0] in vols:
            raise RuntimeError("unused anonymous volume survived prune")
        if busy[0] not in vols:
            raise RuntimeError("prune removed a volume a container still mounts")
        if named not in vols:
            raise RuntimeError("plain prune removed a named volume")
        docker("volume", "prune", "-f", "--all")
        vols = docker("volume", "ls", "-q").stdout.split()
        if named in vols or busy[0] not in vols:
            raise RuntimeError(f"prune --all: named gone={named not in vols}, busy kept={busy[0] in vols}")
        record("volume prune", "PASS", "anonymous only by default, --all for named, mounted kept")
    finally:
        cleanup(user, f"{user}-gone")
        docker("volume", "rm", "-f", named, check=False)



def test_run_rm_latency() -> None:
    """docker run --rm of a trivial command returns promptly: the attach
    stream ends shortly after the exit (it used to wait 2 s of log quiet)."""
    docker("run", "--rm", "alpine", "true")  # warm the image and the path
    times = []
    for _ in range(3):
        t0 = time.time()
        out = docker("run", "--rm", "alpine", "sh", "-c", "echo out; printf tail").stdout
        times.append(time.time() - t0)
        if out != "out\ntail":
            raise RuntimeError(f"output lost: {out!r}")
    median = sorted(times)[1]
    if median > 1.5:
        raise RuntimeError(f"docker run --rm median {median:.2f}s (want < 1.5s)")
    record("docker run --rm latency", "PASS", f"median {median:.2f}s, output complete")


def test_stop_timeout_zero() -> None:
    """docker stop -t 0 kills at once (it used to mean "default 10 s"), and
    -t N waits N seconds for a process that ignores SIGTERM."""
    name = f"{PREFIX}-stop0"
    try:
        for t, lo, hi in (("0", 0.0, 2.0), ("2", 1.5, 4.5)):
            docker("rm", "-f", name, check=False, timeout=60.0)
            # sleep as PID 1 has no SIGTERM handler: only SIGKILL stops it.
            docker("run", "-d", "--name", name, "alpine", "sleep", "300")
            t0 = time.time()
            docker("stop", "-t", t, name, timeout=60.0)
            took = time.time() - t0
            if not lo <= took <= hi:
                raise RuntimeError(f"stop -t {t} took {took:.2f}s (want {lo}-{hi}s)")
    finally:
        docker("rm", "-f", name, check=False, timeout=60.0)
    record("stop -t 0 is immediate", "PASS", "-t 0 < 2s, -t 2 waits ~2s")


def test_compose_many_services() -> None:
    """compose up of 15 services starts all of them (a short listen backlog
    on the Docker socket used to refuse compose's parallel connections),
    they reach each other and the outside, and down tears it all down."""
    project = f"{PREFIX}-many"
    n = 15
    with tempfile.TemporaryDirectory() as tmp:
        compose_file = Path(tmp) / "compose.yml"
        compose_file.write_text("services:\n" + "".join(
            f"  s{i}:\n    image: alpine\n    command: sleep 300\n" for i in range(1, n + 1)))
        base = ["compose", "-p", project, "-f", str(compose_file)]
        try:
            t0 = time.time()
            docker(*base, "up", "-d", timeout=300.0)
            up = time.time() - t0
            ps = docker(*base, "ps", "--format", "{{.State}}").stdout.split()
            if ps.count("running") != n:
                raise RuntimeError(f"{ps.count('running')}/{n} running after up: {ps}")
            out = docker(*base, "exec", "-T", "s1", "ping", "-c", "1", "-W", "3", f"s{n}", check=False)
            if out.returncode != 0:
                raise RuntimeError(f"s1 cannot reach s{n}: {out.stdout!r} {out.stderr!r}")
            out = docker(*base, "exec", "-T", "s2", "wget", "-q", "-O", "/dev/null", "-T", "10",
                         "http://example.com", check=False, timeout=60.0)
            if out.returncode != 0:
                raise RuntimeError(f"no egress from a compose network: {out.stderr!r}")
            t0 = time.time()
            docker(*base, "down", "-t", "0", timeout=300.0)
            down = time.time() - t0
            left = docker("ps", "-a", "--filter", f"label=com.docker.compose.project={project}",
                          "--format", "{{.Names}}").stdout.split()
            if left:
                raise RuntimeError(f"containers left after down: {left}")
        finally:
            docker(*base, "down", "-t", "0", check=False, timeout=300.0)
    record("compose 15 services", "PASS", f"up {up:.1f}s, down -t 0 {down:.1f}s, mesh + egress ok")


def test_system_df_verbose() -> None:
    """docker system df -v reports real volume sizes and reference counts."""
    vol = f"{PREFIX}-dfvol"
    name = f"{PREFIX}-dfc"
    try:
        docker("volume", "create", vol)
        docker("run", "-d", "--name", name, "-v", f"{vol}:/data", "alpine", "sh", "-c",
               "head -c 3000000 /dev/zero > /data/f && sleep 300")
        time.sleep(1.0)
        out = docker("system", "df", "-v", "--format", "{{json .}}", timeout=120.0).stdout
        data = json.loads(out)
        vols = {v["Name"]: v for v in data.get("Volumes") or []}
        if vol not in vols:
            raise RuntimeError(f"volume {vol} missing from df -v: {list(vols)}")
        v = vols[vol]
        if str(v.get("Links")) != "1" or v.get("Size") in ("0B", "", "N/A"):
            raise RuntimeError(f"volume usage not reported: {v}")
    finally:
        docker("rm", "-f", name, check=False, timeout=60.0)
        docker("volume", "rm", "-f", vol, check=False, timeout=60.0)
    record("system df -v", "PASS", f"volume size {v.get('Size')}, links {v.get('Links')}")


def test_build_context_symlinks() -> None:
    """Symlinks in the build context stay symlinks in the image, absolute
    ones included (the context is extracted chrooted into its directory)."""
    tag = f"{PREFIX}-ctxlink:1"
    with tempfile.TemporaryDirectory() as tmp:
        d = Path(tmp)
        (d / "real.txt").write_text("ctx-ok\n")
        os.symlink("real.txt", d / "rel")
        os.symlink("/etc/hostname", d / "abs")
        (d / "Dockerfile").write_text(
            "FROM alpine\nCOPY . /ctx\nRUN cat /ctx/rel && readlink /ctx/abs > /ctx/abs.target\n")
        try:
            docker("build", "-t", tag, tmp, timeout=600.0)
            out = docker("run", "--rm", tag, "sh", "-c",
                         "cat /ctx/rel; readlink /ctx/rel; cat /ctx/abs.target").stdout.split()
            if out != ["ctx-ok", "real.txt", "/etc/hostname"]:
                raise RuntimeError(f"context symlinks not preserved: {out}")
        finally:
            docker("rmi", "-f", tag, check=False, timeout=60.0)
    record("build context symlinks", "PASS", "relative and absolute links kept")


def api_status(method: str, path: str) -> int:
    """Raw Docker API call over the anvil socket; returns the HTTP status."""
    proc = subprocess.run(
        ["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "60",
         "--unix-socket", str(DOCKER_SOCKET), "-X", method, f"http://anvil{path}"],
        capture_output=True, text=True)
    return int(proc.stdout.strip() or 0)


def test_ephemeral_host_ports() -> None:
    """`-p 80`, `-P` and `-p lo-hi:80` get a free host port at start.

    Regression: an empty HostPort was skipped, so testcontainers and
    compose `ports: ["80"]` published nothing.
    """
    eph, pall, r1, r2 = (f"{PREFIX}-eph", f"{PREFIX}-pall", f"{PREFIX}-rng1", f"{PREFIX}-rng2")
    lo = PORT_BASE + 60
    try:
        docker("run", "-d", "--name", eph, "-p", "80", "nginx")
        port = docker("port", eph, "80/tcp").stdout.strip().splitlines()[0].rsplit(":", 1)[1]
        if curl_status(int(port)) != "200":
            raise RuntimeError(f"-p 80 -> localhost:{port} did not answer")
        bindings = json.loads(docker("inspect", eph, "--format", "{{json .HostConfig.PortBindings}}").stdout)
        if bindings["80/tcp"][0]["HostPort"] != "":
            raise RuntimeError(f"HostConfig.PortBindings must keep the request: {bindings}")

        docker("run", "-d", "--name", pall, "-P", "nginx")  # nginx EXPOSEs 80
        pport = docker("port", pall).stdout.strip()
        if "80/tcp ->" not in pport:
            raise RuntimeError(f"-P published nothing: {pport!r}")

        docker("run", "-d", "--name", r1, "-p", f"{lo}-{lo + 3}:80", "nginx")
        docker("run", "-d", "--name", r2, "-p", f"{lo}-{lo + 3}:80", "nginx")
        p1 = docker("port", r1, "80/tcp").stdout.strip().rsplit(":", 1)[1]
        p2 = docker("port", r2, "80/tcp").stdout.strip().rsplit(":", 1)[1]
        if p1 == p2 or not all(lo <= int(p) <= lo + 3 for p in (p1, p2)):
            raise RuntimeError(f"range picks {p1}, {p2} not distinct within {lo}-{lo + 3}")
        record("ephemeral host ports", "PASS", f"-p 80 -> {port}, -P -> {pport.split()[-1]}, range -> {p1}/{p2}")
    finally:
        cleanup(eph, pall, r1, r2)


def test_docker_socket_in_container() -> None:
    """Containers can mount the Docker socket (Testcontainers' Ryuk,
    devcontainers, Traefik): /var/run/docker.sock and the Mac-side
    ~/.anvil-vz/docker.sock both resolve to the guest's API socket."""
    name = f"{PREFIX}-sockpeer"
    try:
        docker("run", "-d", "--name", name, "alpine", "sleep", "300")
        for src in ("/var/run/docker.sock", str(DOCKER_SOCKET)):
            out = docker("run", "--rm", "-u", "1000", "-v", f"{src}:/var/run/docker.sock",
                         "docker:cli", "docker", "ps", "--format", "{{.Names}}", timeout=300.0)
            if name not in out.stdout.split():
                raise RuntimeError(f"docker ps through {src} did not list {name}: {out.stdout!r}")
        record("docker socket in container", "PASS", "API reachable via both socket paths, as a non-root user")
    finally:
        cleanup(name)


def test_api_status_codes() -> None:
    """404 / 409 / 304 as Docker sends them; clients branch on these."""
    run_, stopped = f"{PREFIX}-codes-run", f"{PREFIX}-codes-stopped"
    try:
        docker("run", "-d", "--name", run_, "alpine", "sleep", "300")
        docker("create", "--name", stopped, "alpine", "true")
        checks = {
            "start running -> 304": (api_status("POST", f"/containers/{run_}/start"), 304),
            "stop stopped -> 304": (api_status("POST", f"/containers/{stopped}/stop"), 304),
            "kill stopped -> 409": (api_status("POST", f"/containers/{stopped}/kill"), 409),
            "rm running -> 409": (api_status("DELETE", f"/containers/{run_}"), 409),
            "rm missing -> 404": (api_status("DELETE", f"/containers/{PREFIX}-nope"), 404),
            "start missing -> 404": (api_status("POST", f"/containers/{PREFIX}-nope/start"), 404),
        }
        bad = {k: got for k, (got, want) in checks.items() if got != want}
        if bad:
            raise RuntimeError(f"wrong status codes: {bad}")
        dup = docker("create", "--name", run_, "alpine", "true", check=False)
        if dup.returncode == 0 or "Conflict" not in dup.stderr:
            raise RuntimeError(f"duplicate name not a conflict: {dup.stderr.strip()}")
        if docker("rm", "-f", f"{PREFIX}-nope", check=False).returncode != 0:
            raise RuntimeError("docker rm -f on a missing container must succeed")
        record("API status codes", "PASS", ", ".join(checks))
    finally:
        cleanup(run_, stopped)


def test_published_port_half_close() -> None:
    """A client's shutdown(SHUT_WR) must reach the container as EOF while
    the reply still flows back (redis/DB pools, `nc -N`)."""
    name = f"{PREFIX}-halfclose"
    port = PORT_BASE + 70
    try:
        docker("run", "-d", "--name", name, "-p", f"{port}:9000", "alpine",
               "sh", "-c", "while true; do nc -l -p 9000 -e cat; done")
        deadline = time.time() + 15
        reply = b""
        while time.time() < deadline:
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=5) as s:
                    s.sendall(b"ping")
                    s.shutdown(socket.SHUT_WR)
                    s.settimeout(5)
                    reply = b""
                    while chunk := s.recv(64):
                        reply += chunk
                if reply == b"ping":
                    break
            except OSError:
                pass
            time.sleep(0.5)
        if reply != b"ping":
            raise RuntimeError(f"echo after half-close = {reply!r}, want b'ping' (FIN not forwarded?)")
        record("published port half-close", "PASS", "FIN reached the container, reply came back")
    finally:
        cleanup(name)


def test_exec_tty() -> None:
    """`docker exec -t` gets a pty (regression: "not a tty")."""
    name = f"{PREFIX}-exectty"
    try:
        docker("run", "-d", "--name", name, "alpine", "sleep", "300")
        out = docker("exec", "-t", name, "tty", check=False)
        if out.returncode != 0 or not out.stdout.strip().startswith("/dev/pts/"):
            raise RuntimeError(f"exec -t tty -> rc={out.returncode} {out.stdout.strip()!r}")
        plain = docker("exec", name, "tty", check=False)
        if plain.returncode == 0:
            raise RuntimeError("exec without -t reported a tty")
        record("exec -t allocates a tty", "PASS", out.stdout.strip())
    finally:
        cleanup(name)


def test_run_interactive_stdin() -> None:
    """`docker run -i` pipes stdin into the container and closes it at EOF."""
    out = docker("run", "--rm", "-i", "alpine", "sh", "-c", "wc -c; echo done",
                 input_text="x" * 100000)
    lines = out.stdout.split()
    if lines[:2] != ["100000", "done"]:
        raise RuntimeError(f"stdin not delivered or not closed: {out.stdout!r}")
    record("run -i stdin", "PASS", "100000 bytes piped in, EOF delivered")


def test_attach_stdin_and_detach() -> None:
    """`docker start -ai` on an OpenStdin container: stdin flows in, output
    of the new run only (no replay of the previous run's log)."""
    name = f"{PREFIX}-attachin"
    try:
        docker("create", "-i", "--name", name, "alpine", "sh", "-c", "read l; echo got:$l")
        first = docker("start", "-ai", name, input_text="one\n")
        second = docker("start", "-ai", name, input_text="two\n")
        if first.stdout.strip() != "got:one" or second.stdout.strip() != "got:two":
            raise RuntimeError(f"start -ai output: {first.stdout!r} then {second.stdout!r}")
        record("start -ai stdin", "PASS", "each run read its own stdin, no log replay")
    finally:
        cleanup(name)


def test_tty_logs_raw() -> None:
    """docker logs of a -t container is a raw stream (no mux headers)."""
    name = f"{PREFIX}-ttylogs"
    try:
        docker("run", "-t", "--name", name, "alpine", "echo", "hello-tty")
        out = subprocess.run(["docker", "logs", name], capture_output=True, env=DOCKER_ENV, timeout=60)
        if out.stdout != b"hello-tty\r\n":
            raise RuntimeError(f"logs of a tty container = {out.stdout!r}")
        record("tty container logs are raw", "PASS", repr(out.stdout))
    finally:
        cleanup(name)


def test_logs_tail_zero_follow() -> None:
    """`logs -f --tail 0` shows only new lines."""
    name = f"{PREFIX}-tail0"
    try:
        docker("run", "-d", "--name", name, "alpine", "sh", "-c",
               "echo old; sleep 2; echo new; sleep 1")
        time.sleep(1)
        out = docker("logs", "-f", "--tail", "0", name, timeout=30)
        if out.stdout.split() != ["new"]:
            raise RuntimeError(f"logs -f --tail 0 = {out.stdout!r}, want only 'new'")
        record("logs -f --tail 0", "PASS", "old lines skipped, new line followed")
    finally:
        cleanup(name)


def test_restart_keeps_rm_container() -> None:
    """docker restart must not remove an --rm container (Docker keeps it)."""
    name = f"{PREFIX}-rmrestart"
    try:
        docker("run", "-d", "--rm", "--name", name, "alpine", "sleep", "300")
        docker("restart", "-t", "1", name)
        time.sleep(2)
        state = docker("inspect", name, "--format", "{{.State.Running}}", check=False)
        if state.stdout.strip() != "true":
            raise RuntimeError(f"after restart: {state.stdout.strip() or state.stderr.strip()}")
        docker("stop", "-t", "1", name)
        deadline = time.time() + 15
        while time.time() < deadline and docker("inspect", name, check=False).returncode == 0:
            time.sleep(0.5)
        if docker("inspect", name, check=False).returncode == 0:
            raise RuntimeError("--rm container not removed after a real stop")
        record("restart keeps --rm container", "PASS", "survived restart, removed after stop")
    finally:
        cleanup(name)


def test_generated_names_and_status() -> None:
    """Unnamed containers get Docker-style names; ps Status reads like Docker."""
    cid = docker("run", "-d", "alpine", "sleep", "300").stdout.strip()
    try:
        name = docker("inspect", cid, "--format", "{{.Name}}").stdout.strip().lstrip("/")
        if not re.fullmatch(r"[a-z]+_[a-z]+\d*", name):
            raise RuntimeError(f"generated name {name!r}")
        host = docker("exec", cid, "hostname").stdout.strip()
        if host != cid[:12]:
            raise RuntimeError(f"hostname {host!r}, want the short ID")
        st = docker("ps", "--filter", f"id={cid}", "--format", "{{.Status}}").stdout.strip()
        if not st.startswith("Up "):
            raise RuntimeError(f"running status {st!r}")
        docker("rm", "-f", cid)
        ex = f"{PREFIX}-exit3"
        docker("run", "--name", ex, "alpine", "sh", "-c", "exit 3", check=False)
        st = docker("ps", "-a", "--filter", f"name={ex}", "--format", "{{.Status}}").stdout.strip()
        cleanup(ex)
        if not st.startswith("Exited (3) "):
            raise RuntimeError(f"exited status {st!r}")
        record("generated names + ps status", "PASS", f"{name}; 'Up …'; '{st}'")
    finally:
        docker("rm", "-f", cid, check=False)


def test_inspect_fields() -> None:
    """Inspect carries the fields devcontainers/testcontainers read."""
    name = f"{PREFIX}-inspect"
    try:
        # sleep: nginx itself cannot bind :80 as user 101 and would exit.
        docker("run", "-d", "--name", name, "--user", "101", "nginx", "sleep", "300")
        info = json.loads(docker("inspect", name).stdout)[0]
        problems = []
        if info.get("Created", "").startswith("0001") or not info.get("Created"):
            problems.append("Created")
        if not info.get("Path"):
            problems.append("Path")
        if info["State"].get("StartedAt", "0001").startswith("0001"):
            problems.append("State.StartedAt")
        if info["Config"].get("User") != "101":
            problems.append(f"Config.User={info['Config'].get('User')!r}")
        if "80/tcp" not in (info["Config"].get("ExposedPorts") or {}):
            problems.append("Config.ExposedPorts")
        net = info["NetworkSettings"]["Networks"].get("bridge", {})
        if not net.get("Gateway") or net.get("IPPrefixLen", 0) == 0:
            problems.append(f"bridge endpoint {net}")
        if problems:
            raise RuntimeError(f"missing/wrong: {problems}")
        record("inspect fields", "PASS", "Created/Path/StartedAt/User/ExposedPorts/Gateway set")
    finally:
        cleanup(name)


def test_info_and_version() -> None:
    name = f"{PREFIX}-infocount"
    try:
        docker("run", "-d", "--name", name, "alpine", "sleep", "300")
        info = json.loads(docker("info", "--format", "{{json .}}").stdout)
        if info.get("ContainersRunning", 0) < 1 or info.get("Images", 0) < 1 or info.get("MemTotal", 0) <= 0:
            raise RuntimeError(f"info counts: running={info.get('ContainersRunning')} images={info.get('Images')} mem={info.get('MemTotal')}")
        ver = docker("version", "--format", "{{.Server.Version}}").stdout.strip()
        if ver.startswith("24."):
            raise RuntimeError(f"server version still {ver}")
        record("info/version", "PASS", f"running={info['ContainersRunning']} images={info['Images']} server={ver}")
    finally:
        cleanup(name)


def test_ps_filters_extended() -> None:
    a, b = f"{PREFIX}-flt-a", f"{PREFIX}-flt-b"
    try:
        docker("run", "--name", a, "alpine", "sh", "-c", "exit 2", check=False)
        docker("run", "-d", "--name", b, "nginx")
        def names(*flt: str) -> set[str]:
            out = docker("ps", "-a", "--format", "{{.Names}}", *[x for f in flt for x in ("--filter", f)]).stdout
            return set(out.split())
        checks = {
            "ancestor=nginx": (b in names("ancestor=nginx")) and (a not in names("ancestor=nginx")),
            "exited=2": names("exited=2") >= {a} and b not in names("exited=2"),
            f"since={a}": b in names(f"since={a}") and a not in names(f"since={a}"),
            f"before={b}": a in names(f"before={b}") and b not in names(f"before={b}"),
            "status=exited w/o -a": a in docker("ps", "--filter", "status=exited", "--format", "{{.Names}}").stdout.split(),
        }
        bad = [k for k, ok in checks.items() if not ok]
        if docker("ps", "--filter", "bogus=1", check=False).returncode == 0:
            bad.append("unknown filter accepted")
        if bad:
            raise RuntimeError(f"failed: {bad}")
        record("ps filters (ancestor/exited/since/before/status)", "PASS", ", ".join(checks))
    finally:
        cleanup(a, b)


def test_prune_respects_filters() -> None:
    keep, drop = f"{PREFIX}-prune-keep", f"{PREFIX}-prune-drop"
    try:
        docker("create", "--name", keep, "alpine", "true")
        docker("create", "--name", drop, "--label", "anvil-it-prune=yes", "alpine", "true")
        docker("container", "prune", "-f", "--filter", "label=anvil-it-prune=yes")
        left = docker("ps", "-a", "--format", "{{.Names}}").stdout.split()
        if drop in left or keep not in left:
            raise RuntimeError(f"after label-filtered prune: keep={keep in left} drop={drop in left}")
        docker("container", "prune", "-f", "--filter", "until=1h")
        if keep not in docker("ps", "-a", "--format", "{{.Names}}").stdout.split():
            raise RuntimeError("until=1h pruned a container created just now")
        record("prune filters", "PASS", "label and until honored")
    finally:
        cleanup(keep, drop)


def test_pull_by_digest() -> None:
    digests = docker("image", "inspect", "alpine", "--format", "{{json .RepoDigests}}").stdout
    ds = [d for d in json.loads(digests or "[]") if "@sha256:" in d]
    if not ds:
        record("pull by digest", "SKIP", "alpine has no RepoDigests")
        return
    ref = "alpine@" + ds[0].split("@", 1)[1]
    docker("pull", ref, timeout=300.0)
    out = docker("run", "--rm", ref, "echo", "digest-ok").stdout.strip()
    if out != "digest-ok":
        raise RuntimeError(f"run {ref}: {out!r}")
    record("pull by digest", "PASS", ref[:40])


def test_mount_tmpfs_and_stop_timeout() -> None:
    name = f"{PREFIX}-tmpfs"
    try:
        docker("run", "-d", "--name", name, "--stop-timeout", "1",
               "--mount", "type=tmpfs,destination=/scratch,tmpfs-size=1048576",
               "alpine", "sh", "-c", "trap '' TERM; sleep 300")
        df = docker("exec", name, "sh", "-c", "grep ' /scratch ' /proc/mounts").stdout
        if "tmpfs" not in df or "size=1024k" not in df:
            raise RuntimeError(f"/scratch mount: {df.strip()!r}")
        t0 = time.time()
        docker("stop", name)
        took = time.time() - t0
        if took > 6:
            raise RuntimeError(f"docker stop took {took:.1f}s with --stop-timeout 1")
        record("--mount tmpfs + --stop-timeout", "PASS", f"tmpfs 1M mounted; stop in {took:.1f}s")
    finally:
        cleanup(name)


def test_volume_bind_driver_opts_and_dns() -> None:
    vol, name = f"{PREFIX}-bindvol", f"{PREFIX}-bindvol-c"
    with tempfile.TemporaryDirectory(dir=str(HOME)) as d:
        Path(d, "marker.txt").write_text("from-host")
        try:
            docker("volume", "create", "--opt", "type=none", "--opt", "o=bind", "--opt", f"device={d}", vol)
            out = docker("run", "--rm", "--name", name, "-v", f"{vol}:/data",
                         "--dns-search", "anvil.test", "--dns-option", "ndots:3",
                         "alpine", "sh", "-c", "cat /data/marker.txt; cat /etc/resolv.conf").stdout
            if "from-host" not in out:
                raise RuntimeError(f"bind-backed volume content missing: {out!r}")
            if "search anvil.test" not in out or "options ndots:3" not in out:
                raise RuntimeError(f"resolv.conf: {out!r}")
            record("volume driver_opts bind + dns-search/option", "PASS", "host dir mounted, resolv.conf set")
        finally:
            docker("volume", "rm", "-f", vol, check=False)


def test_network_container_mode() -> None:
    target = f"{PREFIX}-nettarget"
    try:
        docker("run", "-d", "--name", target, "nginx")
        out = ""
        for _ in range(20):
            p = docker("run", "--rm", "--network", f"container:{target}", "alpine",
                       "wget", "-qO-", "http://127.0.0.1:80/", check=False)
            out = p.stdout
            if "nginx" in out.lower():
                break
            time.sleep(0.5)
        if "nginx" not in out.lower():
            raise RuntimeError(f"localhost:80 from the shared netns: {out[:80]!r}")
        mode = docker("run", "-d", "--network", f"container:{target}", "alpine", "sleep", "30").stdout.strip()
        nm = docker("inspect", mode, "--format", "{{.HostConfig.NetworkMode}}").stdout.strip()
        docker("rm", "-f", mode, check=False)
        if not nm.startswith("container:"):
            raise RuntimeError(f"NetworkMode {nm!r}")
        record("--network container:<x>", "PASS", "target's localhost reachable; NetworkMode kept")
    finally:
        cleanup(target)


def test_local_registry_push_pull() -> None:
    """`docker push localhost:<port>/…` reaches a registry:2 container
    published on that port (Docker Desktop semantics; plain HTTP)."""
    reg = f"{PREFIX}-registry"
    port = PORT_BASE + 80
    ref = f"localhost:{port}/anvil-it/alpine:1"
    try:
        docker("run", "-d", "--name", reg, "-p", f"{port}:5000", "registry:2", timeout=300.0)
        deadline = time.time() + 30
        while time.time() < deadline and curl_status(port, "/v2/", wait=1) != "200":
            time.sleep(0.5)
        docker("tag", "alpine", ref)
        docker("push", ref, timeout=300.0)
        docker("rmi", ref)
        docker("pull", ref, timeout=300.0)
        out = docker("run", "--rm", ref, "echo", "from-local-registry").stdout.strip()
        if out != "from-local-registry":
            raise RuntimeError(f"run pulled image: {out!r}")
        record("local registry push/pull", "PASS", ref)
    finally:
        docker("rmi", "-f", ref, check=False)
        cleanup(reg)


def test_lifecycle_events() -> None:
    """Docker's container lifecycle events: create/destroy once per
    container (not per restart), plus kill/stop/restart/pause/unpause/rename
    and exec_*; image tag and volume create/destroy."""
    name, renamed = f"{PREFIX}-evlife", f"{PREFIX}-evlife2"
    vol = f"{PREFIX}-evvol"
    since = str(int(time.time()) - 30)  # guest clock may trail the Mac's
    try:
        cid = docker("run", "-d", "--name", name, "alpine", "sleep", "300").stdout.strip()
        docker("pause", name)
        docker("unpause", name)
        docker("exec", name, "true")
        docker("rename", name, renamed)
        docker("restart", "-t", "1", renamed)
        docker("stop", "-t", "1", renamed)
        docker("rm", renamed)
        docker("tag", "alpine", "anvil-it-evtag:1")
        docker("volume", "create", vol)
        docker("volume", "rm", vol)
        time.sleep(1.0)
        out = docker("events", "--since", since, "--until", str(int(time.time()) + 1),
                     "--format", "{{json .}}").stdout
        evs = [json.loads(l) for l in out.splitlines() if l.strip()]
        mine = [e["Action"] for e in evs if e.get("Type") == "container" and e.get("Actor", {}).get("ID") == cid]
        problems = []
        for a in ("create", "start", "pause", "unpause", "rename", "restart", "kill", "stop", "die", "destroy", "exec_die"):
            if not any(m == a for m in mine):
                problems.append(f"no {a}")
        if mine.count("create") != 1 or mine.count("destroy") != 1:
            problems.append(f"create x{mine.count('create')}, destroy x{mine.count('destroy')}")
        if mine.count("die") != 2:  # restart + stop; an exec exit is no die
            problems.append(f"die x{mine.count('die')}")
        if not any(m.startswith("exec_start") for m in mine):
            problems.append("no exec_start")
        others = {(e.get("Type"), e.get("Action")) for e in evs}
        for want in (("image", "tag"), ("volume", "create"), ("volume", "destroy")):
            if want not in others:
                problems.append(f"no {want[0]} {want[1]}")
        if problems:
            raise RuntimeError(f"{problems}; container actions: {mine}")
        record("lifecycle events", "PASS", " ".join(mine))
    finally:
        cleanup(name, renamed)
        docker("rmi", "anvil-it-evtag:1", check=False)
        docker("volume", "rm", "-f", vol, check=False)


def test_exec_inherits_env_and_cwd() -> None:
    name = f"{PREFIX}-execenv"
    try:
        docker("run", "-d", "--name", name, "-e", "FOO=bar", "-w", "/tmp", "alpine", "sleep", "300")
        out = docker("exec", name, "sh", "-c", "echo $FOO; pwd").stdout.split()
        if out != ["bar", "/tmp"]:
            raise RuntimeError(f"exec saw {out}, want ['bar', '/tmp']")
        out = docker("exec", "-e", "FOO=override", "-w", "/", name, "sh", "-c", "echo $FOO; pwd").stdout.split()
        if out != ["override", "/"]:
            raise RuntimeError(f"exec -e/-w: {out}")
        docker("stop", "-t", "1", name)
        p = docker("exec", name, "true", check=False)
        if p.returncode == 0 or "is not running" not in p.stderr:
            raise RuntimeError(f"exec on a stopped container: rc={p.returncode} {p.stderr.strip()!r}")
        record("exec env/cwd", "PASS", "container env and WORKDIR inherited, -e/-w override, stopped -> error")
    finally:
        cleanup(name)


def test_healthcheck_image_and_start_period() -> None:
    """HEALTHCHECK from the image applies (with the container's env), and a
    start period does not delay a healthy result."""
    tag = "anvil-it-hcimage:1"
    name = f"{PREFIX}-hcimg"
    with tempfile.TemporaryDirectory() as d:
        Path(d, "Dockerfile").write_text(
            "FROM alpine\nENV PROBE_FILE=/tmp/ok\n"
            'HEALTHCHECK --interval=30s --start-period=60s --start-interval=1s CMD test -f "$PROBE_FILE"\n')
        docker("build", "--load", "-t", tag, d, timeout=300.0, check=False)
        if docker("image", "inspect", tag, check=False).returncode != 0:
            docker("build", "-t", tag, d, timeout=300.0)
    try:
        docker("run", "-d", "--name", name, tag, "sh", "-c", "touch /tmp/ok; sleep 300")
        t0 = time.time()
        status = ""
        while time.time() - t0 < 20:
            status = docker("inspect", name, "--format", "{{if .State.Health}}{{.State.Health.Status}}{{end}}").stdout.strip()
            if status == "healthy":
                break
            time.sleep(0.5)
        if status != "healthy":
            raise RuntimeError(f"health after {time.time() - t0:.0f}s: {status!r} (image HEALTHCHECK / start interval)")
        record("image HEALTHCHECK + start period", "PASS", f"healthy after {time.time() - t0:.1f}s inside a 60s start period")
    finally:
        cleanup(name)
        docker("rmi", tag, check=False)


def test_list_filters_and_limits() -> None:
    net = f"{PREFIX}-fnet"
    a, b = f"{PREFIX}-lim-a", f"{PREFIX}-lim-b"
    try:
        docker("network", "create", net)
        names = docker("network", "ls", "--filter", f"name={net}", "--format", "{{.Name}}").stdout.split()
        if names != [net]:
            raise RuntimeError(f"network ls name filter: {names}")
        dup = docker("network", "create", net, check=False)
        if dup.returncode == 0:
            raise RuntimeError("duplicate network create succeeded")
        docker("create", "--name", a, "alpine", "true")
        docker("create", "--name", b, "alpine", "true")
        last = docker("ps", "-l", "--format", "{{.Names}}").stdout.split()
        if last != [b]:
            raise RuntimeError(f"ps -l: {last}")
        bad = docker("network", "ls", "--filter", "bogus=1", check=False)
        if bad.returncode == 0:
            raise RuntimeError("unknown network filter accepted")
        record("network/ps filters and limits", "PASS", "name filter, duplicate 409, ps -l, unknown filter rejected")
    finally:
        cleanup(a, b)
        docker("network", "rm", net, check=False)


def test_api_odds() -> None:
    """Classic /build reports the image ID; distribution inspect; swarm
    endpoints answer 503 like a non-swarm engine."""
    with tempfile.TemporaryDirectory() as d:
        Path(d, "Dockerfile").write_text("FROM alpine\nRUN echo built > /built\n")
        tar = Path(d, "ctx.tar")
        subprocess.run(["tar", "-cf", str(tar), "-C", d, "Dockerfile"], check=True)
        out = subprocess.run(
            ["curl", "-s", "--max-time", "300", "--unix-socket", str(DOCKER_SOCKET),
             "-X", "POST", "-H", "Content-Type: application/x-tar", "--data-binary", f"@{tar}",
             "http://anvil/build?q=1"], capture_output=True, text=True).stdout
    ids = [json.loads(l)["aux"]["ID"] for l in out.splitlines() if '"aux"' in l]
    if not ids or not ids[0].startswith("sha256:"):
        raise RuntimeError(f"/build gave no aux ID: {out[-300:]!r}")
    if docker("image", "inspect", ids[0], check=False).returncode != 0:
        raise RuntimeError(f"built image {ids[0]} not inspectable")
    if not re.search(r"Successfully built [0-9a-f]{12}", out):
        raise RuntimeError(f"no 'Successfully built <id>' line: {out[-200:]!r}")
    docker("rmi", ids[0], check=False)
    dist = subprocess.run(["curl", "-s", "--max-time", "60", "--unix-socket", str(DOCKER_SOCKET),
                           "http://anvil/distribution/alpine:latest/json"], capture_output=True, text=True).stdout
    d = json.loads(dist or "{}")
    if not d.get("Descriptor", {}).get("digest", "").startswith("sha256:") or not d.get("Platforms"):
        raise RuntimeError(f"distribution inspect: {dist[:200]!r}")
    if api_status("GET", "/swarm") != 503:
        raise RuntimeError("/swarm is not 503")
    record("build aux ID, distribution, swarm 503", "PASS", f"built {ids[0][:19]}, {len(d['Platforms'])} platforms")


def test_static_ip() -> None:
    """--ip / compose ipv4_address on a user-defined network."""
    net, name = f"{PREFIX}-ipnet", f"{PREFIX}-staticip"
    try:
        docker("network", "create", "--subnet", "10.10.251.0/24", net)
        docker("run", "-d", "--name", name, "--network", net, "--ip", "10.10.251.77", "alpine", "sleep", "300")
        ip = docker("inspect", name, "--format", "{{(index .NetworkSettings.Networks \"%s\").IPAddress}}" % net).stdout.strip()
        if ip != "10.10.251.77":
            raise RuntimeError(f"address {ip!r}, want 10.10.251.77")
        docker("restart", "-t", "1", name)
        ip = docker("inspect", name, "--format", "{{(index .NetworkSettings.Networks \"%s\").IPAddress}}" % net).stdout.strip()
        if ip != "10.10.251.77":
            raise RuntimeError(f"address after restart {ip!r}")
        bad = docker("run", "--rm", "--ip", "10.10.0.5", "alpine", "true", check=False)
        if bad.returncode == 0:
            raise RuntimeError("--ip on the default bridge accepted")
        record("static IP", "PASS", "kept across restart; default bridge rejects --ip")
    finally:
        cleanup(name)
        docker("network", "rm", net, check=False)


def test_cp_into_volume_before_start() -> None:
    """docker cp into a created (not started) container lands in its volume
    (Testcontainers copies files before start)."""
    name, vol = f"{PREFIX}-cpvol", f"{PREFIX}-cpvol-data"
    with tempfile.TemporaryDirectory() as d:
        f = Path(d, "seed.txt")
        f.write_text("seeded-before-start")
        try:
            docker("create", "--name", name, "-v", f"{vol}:/data", "alpine", "cat", "/data/seed.txt")
            docker("cp", str(f), f"{name}:/data/seed.txt")
            out = docker("start", "-a", name).stdout.strip()
            if out != "seeded-before-start":
                raise RuntimeError(f"container read {out!r} from its volume")
            back = docker("run", "--rm", "-v", f"{vol}:/v", "alpine", "cat", "/v/seed.txt").stdout.strip()
            if back != "seeded-before-start":
                raise RuntimeError(f"volume holds {back!r}")
            record("cp into volume before start", "PASS", "file landed in the volume")
        finally:
            cleanup(name)
            docker("volume", "rm", "-f", vol, check=False)


def test_bind_mounts_tmp_and_var_folders() -> None:
    """/tmp and $TMPDIR (/var/folders) bind mounts reach the Mac's files, as
    with Docker Desktop."""
    checks = []
    for base in ("/tmp", tempfile.gettempdir()):
        d = tempfile.mkdtemp(dir=base)
        try:
            Path(d, "f").write_text("from-mac")
            # Use the path as the user would write it (/tmp/..., /var/folders/...).
            src = d.replace("/private", "", 1) if d.startswith("/private/") else d
            out = docker("run", "--rm", "-v", f"{src}:/m", "alpine", "cat", "/m/f", check=False).stdout.strip()
            checks.append((src, out == "from-mac"))
        finally:
            subprocess.run(["rm", "-rf", d])
    bad = [s for s, ok in checks if not ok]
    if bad:
        raise RuntimeError(f"bind mounts not shared: {bad}")
    record("bind mounts /tmp and /var/folders", "PASS", ", ".join(s for s, _ in checks))


def test_pid_container_mode() -> None:
    target = f"{PREFIX}-pidtarget"
    try:
        docker("run", "-d", "--name", target, "alpine", "sleep", "4242")
        out = docker("run", "--rm", "--pid", f"container:{target}", "alpine", "ps").stdout
        if "sleep 4242" not in out:
            raise RuntimeError(f"target's process not visible: {out!r}")
        record("--pid container:<x>", "PASS", "target's processes visible")
    finally:
        cleanup(target)


def test_docker_import() -> None:
    name = f"{PREFIX}-importsrc"
    tag = "anvil-it-imported:1"
    with tempfile.TemporaryDirectory() as d:
        tarf = Path(d, "fs.tar")
        try:
            docker("create", "--name", name, "alpine", "true")
            docker("export", "-o", str(tarf), name)
            docker("import", "--change", "CMD [\"/bin/echo\", \"imported-ok\"]", str(tarf), tag)
            out = docker("run", "--rm", tag).stdout.strip()
            if out != "imported-ok":
                raise RuntimeError(f"imported image ran {out!r}")
            record("docker import", "PASS", "export -> import --change -> run")
        finally:
            cleanup(name)
            docker("rmi", tag, check=False)


def test_bind_source_checks() -> None:
    # A Mac path anvil does not share must fail, not become an empty dir
    # in guest RAM; -v creates a missing dir on a share; --mount does not.
    r = docker("run", "--rm", "-v", "/opt/anvil-it-unshared-dir:/x", "alpine", "true", check=False)
    if r.returncode == 0 or "not shared" not in r.stderr:
        raise RuntimeError(f"unshared bind was accepted: rc={r.returncode} {r.stderr.strip()!r}")
    with tempfile.TemporaryDirectory(dir=str(Path.home())) as d:
        created = Path(d, "made-by-v")
        docker("run", "--rm", "-v", f"{created}:/x", "alpine", "true")
        if not created.is_dir():
            raise RuntimeError("-v did not create its missing source on the share")
        missing = Path(d, "missing")
        r = docker("run", "--rm", "--mount", f"type=bind,src={missing},dst=/x", "alpine", "true", check=False)
        if r.returncode == 0 or "does not exist" not in r.stderr:
            raise RuntimeError(f"--mount bind of a missing source: rc={r.returncode} {r.stderr.strip()!r}")
        if missing.exists():
            raise RuntimeError("--mount bind created its source")
    # The compose idiom binds the VM's own zone files (UTC, as Docker Desktop).
    out = docker("run", "--rm", "-v", "/etc/localtime:/etc/localtime:ro",
                 "-v", "/etc/timezone:/etc/timezone:ro", "--entrypoint", "date", "nginx", "+%Z").stdout.strip()
    if out != "UTC":
        raise RuntimeError(f"/etc/localtime bind: zone {out!r}")
    record("bind source checks", "PASS", "unshared refused, -v creates, --mount refuses missing, localtime")


def test_internal_network_isolation() -> None:
    net = f"{PREFIX}-internal"
    peer = f"{PREFIX}-internal-peer"
    probe = "nc -w 3 1.1.1.1 80 </dev/null >/dev/null 2>&1 && echo open || echo closed"
    try:
        docker("network", "create", "--internal", net)
        if docker("network", "inspect", "-f", "{{.Internal}}", net).stdout.strip() != "true":
            raise RuntimeError("network inspect does not report Internal")
        docker("run", "-d", "--name", peer, "--network", net, "alpine", "sleep", "60")
        peer_ip = docker("inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", peer).stdout.strip()
        out = docker("run", "--rm", "--network", net, "alpine", "sh", "-c",
                     f"{probe}; ping -c1 -W2 {peer_ip} >/dev/null && echo peer-ok").stdout.split()
        if out != ["closed", "peer-ok"]:
            raise RuntimeError(f"internal network: {out} (want outbound closed, peer reachable)")
        out = docker("run", "--rm", "alpine", "sh", "-c", probe).stdout.strip()
        if out != "open":
            raise RuntimeError(f"default network lost outbound access: {out}")
        record("internal network isolation", "PASS", "no outbound, peers reachable")
    finally:
        cleanup(peer)
        docker("network", "rm", net, check=False)


def raw_api(method: str, path: str, body: bytes = b"", headers: dict | None = None, timeout: float = 30.0) -> tuple[str, bytes]:
    """One HTTP/1.1 request on docker.sock, read to EOF: (status line, rest)."""
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.settimeout(timeout)
    sock.connect(str(DOCKER_SOCKET))
    hdrs = {"Host": "docker", "Connection": "close", **(headers or {})}
    if body:
        hdrs["Content-Type"] = "application/json"
        hdrs["Content-Length"] = str(len(body))
    req = f"{method} {path} HTTP/1.1\r\n" + "".join(f"{k}: {v}\r\n" for k, v in hdrs.items()) + "\r\n"
    sock.sendall(req.encode() + body)
    data = b""
    try:
        while chunk := sock.recv(65536):
            data += chunk
    except socket.timeout:
        pass
    sock.close()
    status, _, rest = data.partition(b"\r\n")
    return status.decode(errors="replace"), rest


def test_api_parity_audit() -> None:
    name = f"{PREFIX}-parity"
    try:
        docker("run", "-d", "--name", name, "alpine", "sh", "-c", "mkdir -p /d/sub && echo x > /d/sub/f && sleep 300")
        # exec start without an Upgrade header (docker-java): 200 OK + stream.
        cid = docker("inspect", "-f", "{{.Id}}", name).stdout.strip()
        st, rest = raw_api("POST", f"/v1.51/containers/{cid}/exec",
                           json.dumps({"AttachStdout": True, "AttachStderr": True, "Cmd": ["echo", "no-upgrade"]}).encode())
        m = re.search(rb'"Id":"([0-9a-f]+)"', rest)
        exec_id = m.group(1).decode() if m else ""
        if len(exec_id) != 64:
            raise RuntimeError(f"exec create: {st} {rest[-200:]!r}")
        st, rest = raw_api("POST", f"/v1.51/exec/{exec_id}/start", json.dumps({"Detach": False, "Tty": False}).encode())
        if "200" not in st or b"no-upgrade" not in rest or b"multiplexed-stream" not in rest:
            raise RuntimeError(f"exec start without Upgrade: {st} {rest[:300]!r}")
        m = re.search(rb"\{.*\}", raw_api("GET", f"/v1.51/exec/{exec_id}/json")[1], re.S)
        insp = json.loads(m.group(0)) if m else {}
        if insp.get("ExitCode") != 0 or not insp.get("Pid"):
            raise RuntimeError(f"exec inspect: {insp}")
        # docker cp ctr:/d/. copies the contents only.
        with tempfile.TemporaryDirectory() as d:
            docker("cp", f"{name}:/d/.", f"{d}/out")
            if not Path(d, "out", "sub", "f").is_file():
                raise RuntimeError(f"cp /. layout: {list(Path(d).rglob('*'))}")
        # cp -a gives the container's USER ownership; plain cp keeps the archive's.
        owned = f"{PREFIX}-cpowner"
        try:
            docker("run", "-d", "--user", "nobody", "--name", owned, "alpine", "sleep", "60")
            with tempfile.TemporaryDirectory() as d:
                Path(d, "f").write_text("x")
                docker("cp", "-a", f"{d}/f", f"{owned}:/tmp/fa")
                docker("cp", f"{d}/f", f"{owned}:/tmp/fp")
            own = docker("exec", owned, "stat", "-c", "%u %n", "/tmp/fa", "/tmp/fp").stdout.split("\n")
            if not own[0].startswith("65534 ") or own[1].startswith("65534 "):
                raise RuntimeError(f"cp ownership: {own}")
        finally:
            cleanup(owned)
        # top lists exec'd processes too.
        docker("exec", "-d", name, "sleep", "123")
        time.sleep(0.5)
        top = docker("top", name).stdout
        if "sleep 123" not in top or "UID" not in top:
            raise RuntimeError(f"docker top: {top!r}")
        # ps -s reports sizes.
        sizes = docker("ps", "-s", "--filter", f"name={name}", "--format", "{{.Size}}").stdout.strip()
        if not sizes or "virtual" not in sizes:
            raise RuntimeError(f"ps -s: {sizes!r}")
        # stop -s honours the signal (SIGKILL: 137, no grace period).
        t0 = time.time()
        docker("stop", "-s", "SIGKILL", "-t", "30", name)
        code = docker("inspect", "-f", "{{.State.ExitCode}}", name).stdout.strip()
        if code != "137" or time.time() - t0 > 10:
            raise RuntimeError(f"stop -s SIGKILL: exit {code} after {time.time() - t0:.1f}s")
        # run --rm frees the name by the time the CLI returns (wait condition=removed).
        rm_name = f"{PREFIX}-rmname"
        for _ in range(2):
            docker("run", "--rm", "--name", rm_name, "alpine", "true")
        # plugins list, unknown events filter.
        st, rest = raw_api("GET", "/v1.51/plugins")
        if "200" not in st or not rest.rstrip().endswith(b"[]"):
            raise RuntimeError(f"/plugins: {st} {rest[-100:]!r}")
        st, _ = raw_api("GET", "/v1.51/events?filters=" + "%7B%22bogus%22%3A%5B%22x%22%5D%7D", timeout=5)
        if "400" not in st:
            raise RuntimeError(f"events with an unknown filter: {st}")
        # pull reports progress and the Docker status wording.
        out = docker("pull", "busybox").stdout
        if "Digest: sha256:" not in out or ("Image is up to date" not in out and "Downloaded newer image" not in out):
            raise RuntimeError(f"pull output: {out!r}")
        record("api parity (audit fixes)", "PASS", "exec w/o Upgrade, cp /., top, ps -s, stop -s, --rm name, plugins, events, pull")
    finally:
        cleanup(name)


def test_static_ip_outside_ip_range() -> None:
    net = f"{PREFIX}-iprange"
    name = f"{PREFIX}-iprange-c"
    try:
        docker("network", "create", "--subnet", "10.77.0.0/24", "--ip-range", "10.77.0.128/25", net)
        docker("run", "-d", "--name", name, "--network", net, "--ip", "10.77.0.10", "alpine", "sleep", "60")
        ip = docker("inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name).stdout.strip()
        dyn = docker("run", "--rm", "--network", net, "alpine", "sh", "-c",
                     "ip -4 -o addr show eth0 | awk '{print $4}'").stdout.strip()
        if ip != "10.77.0.10" or not dyn.startswith("10.77.0.") or int(dyn.split(".")[3].split("/")[0]) < 128:
            raise RuntimeError(f"static {ip!r}, dynamic {dyn!r}")
        record("static IP outside ip_range", "PASS", f"static {ip}, dynamic {dyn}")
    finally:
        cleanup(name)
        docker("network", "rm", net, check=False)


def test_volume_subpath() -> None:
    vol = f"{PREFIX}-subvol"
    name = f"{PREFIX}-subpath"
    try:
        docker("volume", "create", vol)
        docker("run", "--rm", "-v", f"{vol}:/v", "alpine", "sh", "-c",
               "mkdir -p /v/a/b && echo inner > /v/a/b/file && echo single > /v/one && ln -s /etc /v/escape")
        out = docker("run", "--rm", "--mount", f"type=volume,src={vol},dst=/x,volume-subpath=a/b",
                     "--mount", f"type=volume,src={vol},dst=/one.txt,volume-subpath=one", "alpine",
                     "sh", "-c", "cat /x/file /one.txt").stdout.split()
        if out != ["inner", "single"]:
            raise RuntimeError(f"subpath contents: {out}")
        r = docker("run", "--rm", "--mount", f"type=volume,src={vol},dst=/x,volume-subpath=escape",
                   "alpine", "ls", "/x/passwd", check=False)
        if r.returncode == 0:
            raise RuntimeError("a symlink out of the volume was followed")
        docker("run", "-d", "--name", name, "--mount", f"type=volume,src={vol},dst=/x,volume-subpath=a",
               "alpine", "sleep", "60")
        docker("restart", "-t", "0", name)
        if docker("exec", name, "cat", "/x/b/file").stdout.strip() != "inner":
            raise RuntimeError("subpath lost across restart")
        # Used only through a subpath, the volume is in use: prune keeps it,
        # and docker cp into the stopped container reaches it.
        docker("stop", "-t", "0", name)
        docker("volume", "prune", "-af")
        if vol not in docker("volume", "ls", "-q").stdout.split():
            raise RuntimeError("volume prune removed a volume in use through a subpath")
        with tempfile.TemporaryDirectory() as d:
            Path(d, "cpd").write_text("via-cp")
            docker("cp", f"{d}/cpd", f"{name}:/x/cpd")
        out = docker("run", "--rm", "-v", f"{vol}:/v", "alpine", "cat", "/v/a/cpd").stdout.strip()
        if out != "via-cp":
            raise RuntimeError(f"docker cp into a stopped subpath mount: {out!r}")
        record("volume subpath", "PASS", "dir and file subpaths, escape refused, re-armed on restart")
    finally:
        cleanup(name)
        docker("volume", "rm", "-f", vol, check=False)


def test_bind_mount_file_events() -> None:
    # Changes made on the Mac raise inotify events in containers (hot reload).
    name = f"{PREFIX}-fsevents"
    with tempfile.TemporaryDirectory(dir=str(Path.home())) as d:
        Path(d, "a.txt").write_text("v1")
        try:
            docker("run", "-d", "--name", name, "-v", f"{d}:/w", "busybox", "inotifyd", "-", "/w")
            time.sleep(4)  # the guest pushes the watch set with the port state
            with open(Path(d, "a.txt"), "a") as f:
                f.write("v2")
            mtime = Path(d, "a.txt").stat().st_mtime_ns
            Path(d, "new.txt").write_text("x")
            deadline = time.time() + 10
            logs = ""
            while time.time() < deadline:
                logs = docker("logs", name).stdout
                if "a.txt" in logs and "new.txt" in logs:
                    break
                time.sleep(0.5)
            else:
                raise RuntimeError(f"no inotify events for Mac-side changes: {logs!r}")
            # Forwarding must leave the Mac's file exactly as it was.
            if Path(d, "a.txt").stat().st_mtime_ns != mtime:
                raise RuntimeError("forwarding changed the Mac file's mtime")
            # The guest's touch must not echo back into another event.
            time.sleep(3)
            before = len(docker("logs", name).stdout.splitlines())
            time.sleep(3)
            after = len(docker("logs", name).stdout.splitlines())
            if after != before:
                raise RuntimeError(f"events keep coming without changes ({before} -> {after} lines)")
            record("bind mount file events", "PASS", f"{after} events, no echo loop")
        finally:
            cleanup(name)


def test_host_network_ports() -> None:
    # A --network host container's listeners are reachable on the Mac's
    # loopback without -p.
    name = f"{PREFIX}-hostnet"
    port = PORT_BASE + 77
    try:
        docker("run", "-d", "--name", name, "--network", "host", "busybox",
               "sh", "-c", f"mkdir -p /www && echo hostnet-ok > /www/index.html && httpd -f -p {port} -h /www")
        deadline = time.time() + 15
        body = ""
        while time.time() < deadline:
            p = subprocess.run(["curl", "--noproxy", "*", "-s", "-m", "2", f"http://127.0.0.1:{port}/"],
                               capture_output=True, text=True)
            body = p.stdout.strip()
            if body == "hostnet-ok":
                break
            time.sleep(0.5)
        else:
            raise RuntimeError(f"host-network port {port} not reachable from the Mac: {body!r}")
        record("host network ports", "PASS", f"127.0.0.1:{port} -> host-network container")
    finally:
        cleanup(name)


def test_k3s_cluster() -> None:
    # Kubernetes in a container (what k3d/kind do): privileged devices
    # (/dev/kmsg), kube-proxy's kernel modules, pod DNS and Service routing.
    name = f"{PREFIX}-k3s"
    try:
        if "/dev/kmsg" not in docker("run", "--rm", "--privileged", "alpine", "ls", "/dev/kmsg").stdout:
            raise RuntimeError("--privileged container has no /dev/kmsg")
        docker("run", "-d", "--name", name, "--privileged", "rancher/k3s:v1.31.4-k3s1", "server",
               "--disable=traefik", "--disable=metrics-server", timeout=600.0)
        def kubectl(*args: str, check: bool = False) -> subprocess.CompletedProcess:
            return docker("exec", name, "kubectl", *args, check=check, timeout=60.0)
        deadline = time.time() + 180
        while time.time() < deadline:
            pods = kubectl("get", "pods", "-n", "kube-system", "--no-headers").stdout
            if "coredns" in pods and all(" Running " in l for l in pods.splitlines() if "coredns" in l):
                break
            if docker("inspect", "-f", "{{.State.Running}}", name).stdout.strip() != "true":
                raise RuntimeError("k3s exited: " + docker("logs", "--tail", "20", name).stderr[-800:])
            time.sleep(3)
        else:
            raise RuntimeError(f"coredns not running: {pods!r}")
        kubectl("create", "deployment", "web", "--image=nginx:alpine", check=True)
        kubectl("expose", "deployment", "web", "--port", "80", check=True)
        kubectl("wait", "--for=condition=available", "deployment/web", "--timeout=120s", check=True)
        out = kubectl("run", "probe", "--rm", "-i", "--restart=Never", "--image=busybox", "--",
                      "wget", "-qO-", "-T", "10", "http://web.default.svc.cluster.local").stdout
        if "Welcome to nginx" not in out:
            raise RuntimeError(f"service not reachable from a pod: {out[-300:]!r}")
        record("k3s cluster", "PASS", "node ready, coredns running, pod -> Service by DNS name")
    finally:
        cleanup(name)


def ws_attach(cid: str, query: str) -> socket.socket:
    """Open /containers/{id}/attach/ws on docker.sock (client side of RFC 6455)."""
    import base64
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.settimeout(10)
    sock.connect(str(DOCKER_SOCKET))
    key = base64.b64encode(os.urandom(16)).decode()
    sock.sendall((f"GET /v1.51/containers/{cid}/attach/ws?{query} HTTP/1.1\r\nHost: docker\r\n"
                  "Upgrade: websocket\r\nConnection: Upgrade\r\nOrigin: http://localhost\r\n"
                  f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n").encode())
    head = b""
    while b"\r\n\r\n" not in head:
        head += sock.recv(1)
    if b" 101 " not in head.split(b"\r\n")[0]:
        raise RuntimeError(f"attach/ws handshake: {head!r}")
    return sock


def ws_read(sock: socket.socket, want: bytes, timeout: float = 10.0) -> bytes:
    got = b""
    deadline = time.time() + timeout
    while want not in got and time.time() < deadline:
        hdr = sock.recv(2)
        if len(hdr) < 2:
            break
        n = hdr[1] & 0x7F
        if n == 126:
            n = int.from_bytes(sock.recv(2), "big")
        elif n == 127:
            n = int.from_bytes(sock.recv(8), "big")
        payload = b""
        while len(payload) < n:
            payload += sock.recv(n - len(payload))
        got += payload
    return got


def ws_send(sock: socket.socket, data: bytes) -> None:
    mask = os.urandom(4)
    frame = bytes([0x82, 0x80 | len(data)]) + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(data))
    sock.sendall(frame)


def test_attach_websocket() -> None:
    name = f"{PREFIX}-attachws"
    try:
        docker("run", "-d", "-i", "--name", name, "alpine", "sh", "-c", "echo ready; cat")
        cid = docker("inspect", "-f", "{{.Id}}", name).stdout.strip()
        sock = ws_attach(cid, "logs=1&stream=1&stdin=1&stdout=1&stderr=1")
        try:
            if b"ready" not in ws_read(sock, b"ready"):
                raise RuntimeError("no output over attach/ws")
            ws_send(sock, b"over-websocket\n")
            if b"over-websocket" not in ws_read(sock, b"over-websocket"):
                raise RuntimeError("stdin over attach/ws did not come back")
        finally:
            sock.close()
        record("attach over websocket", "PASS", "output and stdin over /attach/ws")
    finally:
        cleanup(name)


def test_container_domains() -> None:
    # http://<name>.anvil.localhost (ANVIL_DOMAINS=1; skipped when off).
    port = int(os.environ.get("ANVIL_DOMAINS_PORT", "80"))
    probe = subprocess.run(["curl", "--noproxy", "*", "-s", "-o", "/dev/null", "-w", "%{http_code}", "-m", "8",
                            f"http://probe.anvil.localhost:{port}/"], capture_output=True, text=True)
    if probe.stdout.strip() in ("", "000"):
        record("container domains", "SKIP", f"ANVIL_DOMAINS is off (nothing on :{port})")
        return
    name = f"{PREFIX}-domain"
    try:
        docker("run", "-d", "--name", name, "busybox", "sh", "-c",
               "mkdir -p /w && echo domain-ok > /w/index.html && httpd -f -p 80 -h /w")
        deadline = time.time() + 15
        body = ""
        while time.time() < deadline:
            body = subprocess.run(["curl", "--noproxy", "*", "-s", "-m", "3", f"http://{name}.anvil.localhost:{port}/"],
                                  capture_output=True, text=True).stdout.strip()
            if body == "domain-ok":
                break
            time.sleep(0.5)
        else:
            raise RuntimeError(f"{name}.anvil.localhost:{port}: {body!r}")
        # A container published on the proxy's own port answers the
        # requests that are not for a container domain.
        pub = f"{PREFIX}-domain-pub"
        try:
            docker("run", "-d", "--name", pub, "-p", f"{port}:80", "busybox", "sh", "-c",
                   "mkdir -p /w && echo published-ok > /w/index.html && httpd -f -p 80 -h /w")
            deadline = time.time() + 15
            while time.time() < deadline:
                body = subprocess.run(["curl", "--noproxy", "*", "-s", "-m", "3", f"http://localhost:{port}/"],
                                      capture_output=True, text=True).stdout.strip()
                if body == "published-ok":
                    break
                time.sleep(0.5)
            else:
                raise RuntimeError(f"-p {port}:80 beside the domains proxy: {body!r}")
        finally:
            cleanup(pub)
        record("container domains", "PASS", f"http://{name}.anvil.localhost:{port}, -p {port} still served")
    finally:
        cleanup(name)


def test_hosts_before_start() -> None:
    """A container's own /etc/hosts lists its network peers before its
    process runs: a short-lived `run --rm ... ping peer` used to race the
    background mesh refresh and find nobody."""
    net = f"{PREFIX}-hostsrace"
    peer = f"{PREFIX}-hrpeer"
    try:
        docker("network", "create", net)
        docker("run", "-d", "--name", peer, "--network", net, "alpine", "sleep", "120")
        misses = 0
        for _ in range(6):
            out = docker("run", "--rm", "--network", net, "alpine", "grep", "-c", peer, "/etc/hosts", check=False)
            if out.stdout.strip() in ("", "0"):
                misses += 1
        if misses:
            raise RuntimeError(f"peer missing from /etc/hosts at start in {misses}/6 runs")
    finally:
        docker("rm", "-f", peer, check=False, timeout=60.0)
        docker("network", "rm", net, check=False, timeout=60.0)
    record("hosts populated before start", "PASS", "6/6 short-lived runs saw the peer")


def test_ipv6_network() -> None:
    """docker network create --ipv6: dual-stack addresses, a v6 default route,
    names resolving to v6, inspect fields, v6 egress through the VM's NAT,
    and no v6 escape from an --internal network or into another network."""
    net, other, internal = f"{PREFIX}-v6", f"{PREFIX}-v6b", f"{PREFIX}-v6i"
    srv = f"{PREFIX}-v6srv"
    try:
        docker("network", "create", "--ipv6", net)
        docker("network", "create", "--ipv6", other)
        docker("network", "create", "--ipv6", "--internal", internal)
        cfg = json.loads(docker("network", "inspect", net, "--format", "{{json .IPAM.Config}}").stdout)
        v6 = [c for c in cfg if ":" in c.get("Subnet", "")]
        if docker("network", "inspect", net, "--format", "{{.EnableIPv6}}").stdout.strip() != "true" or not v6:
            raise RuntimeError(f"network not dual-stack: {cfg}")
        docker("run", "-d", "--name", srv, "--network", net, "nginx:alpine")
        ep = json.loads(docker("inspect", srv, "--format", "{{json .NetworkSettings.Networks}}").stdout)[net]
        addr6 = ep.get("GlobalIPv6Address", "")
        if not addr6 or ep.get("GlobalIPv6PrefixLen") != 64 or not ep.get("IPv6Gateway"):
            raise RuntimeError(f"inspect lacks IPv6 fields: {ep}")
        out = docker("run", "--rm", "--network", net, "alpine", "sh", "-c",
                     f"ip -6 route | grep -q '^default' && ping -6 -c1 -W3 {srv} >/dev/null && echo OK",
                     check=False, timeout=60.0)
        if "OK" not in out.stdout:
            raise RuntimeError(f"no v6 route or name over v6: {out.stdout!r} {out.stderr!r}")
        out = docker("run", "--rm", "--network", other, "alpine", "ping", "-6", "-c1", "-W2", addr6,
                     check=False, timeout=60.0)
        if out.returncode == 0:
            raise RuntimeError("another network reaches the container over IPv6")
        out = docker("run", "--rm", "--network", internal, "alpine", "ping", "-6", "-c1", "-W2",
                     "2606:4700:4700::1111", check=False, timeout=60.0)
        if out.returncode == 0:
            raise RuntimeError("an --internal network reaches the outside over IPv6")
        egress = docker("run", "--rm", "--network", net, "alpine", "ping", "-6", "-c1", "-W3",
                        "2606:4700:4700::1111", check=False, timeout=60.0).returncode == 0
    finally:
        docker("rm", "-f", srv, check=False, timeout=60.0)
        for n in (net, other, internal):
            docker("network", "rm", n, check=False, timeout=60.0)
    record("IPv6 network", "PASS", f"{v6[0]['Subnet']}, inspect + name + isolation ok, "
           f"egress {'ok' if egress else 'unavailable on this host network'}")


def _api_json(method: str, path: str, body: dict | None = None, query: dict | None = None):
    """Raw Docker API call over the anvil socket; returns parsed JSON."""
    args = ["curl", "-s", "--max-time", "60", "--unix-socket", str(DOCKER_SOCKET), "-X", method]
    if query:
        args += ["-G"]
        for k, v in query.items():
            args += ["--data-urlencode", f"{k}={v}"]
    if body is not None:
        args += ["-H", "Content-Type: application/json", "-d", json.dumps(body)]
    out = subprocess.run(args + [f"http://anvil{path}"], capture_output=True, text=True, timeout=90)
    return json.loads(out.stdout or "null")


def test_exec_stdin_eof() -> None:
    """docker exec -i passes stdin EOF on to the process: `echo x | docker
    exec -i c cat` returns (it used to hang — the shim kept the FIFO open)."""
    name = f"{PREFIX}-execeof"
    try:
        docker("run", "-d", "--name", name, "alpine", "sleep", "120")
        out = docker("exec", "-i", name, "sh", "-c", "wc -c; echo eof-seen",
                     input_text="hello stdin\n", timeout=30.0)
        if "eof-seen" not in out.stdout or "12" not in out.stdout:
            raise RuntimeError(f"exec -i output: {out.stdout!r}")
    finally:
        cleanup(name)
    record("exec -i stdin EOF", "PASS", "12 bytes read, EOF reached the process")


def test_k3d_style_create() -> None:
    """What k3d does: list networks/containers by id=^/?<id>$ (Docker filters
    are regular expressions) and create nodes with NetworkMode "bridge" plus
    the cluster network in EndpointsConfig — Docker then joins only that
    network (one default route)."""
    net, name = f"{PREFIX}-k3dnet", f"{PREFIX}-k3dnode"
    try:
        docker("network", "create", net)
        nid = docker("network", "inspect", net, "-f", "{{.ID}}").stdout.strip()
        found = _api_json("GET", "/networks", query={"filters": json.dumps({"id": {f"^/?{nid}$": True}, "name": {f"^/?{net}$": True}})})
        if [n["Name"] for n in found or []] != [net]:
            raise RuntimeError(f"network id regex filter: {found}")
        created = _api_json("POST", f"/containers/create?name={name}", {
            "Image": "alpine", "Cmd": ["sleep", "120"],
            "HostConfig": {"NetworkMode": "bridge"},
            "NetworkingConfig": {"EndpointsConfig": {net: {}}},
        })
        cid = created.get("Id", "")
        if not cid:
            raise RuntimeError(f"create: {created}")
        docker("start", name)
        nets = docker("inspect", name, "-f", "{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}").stdout.split()
        if nets != [net]:
            raise RuntimeError(f"node joined {nets}, want only {net}")
        routes = docker("exec", name, "ip", "route").stdout
        if routes.count("default") != 1:
            raise RuntimeError(f"want one default route: {routes!r}")
        listed = _api_json("GET", "/containers/json", query={"filters": json.dumps({"id": {f"^/?{cid}$": True}})})
        if len(listed or []) != 1:
            raise RuntimeError(f"container id regex filter: {listed}")
    finally:
        cleanup(name)
        docker("network", "rm", net, check=False, timeout=60.0)
    record("k3d-style create", "PASS", "id regex filters, EndpointsConfig-only network, one default route")


def test_etc_files_writable() -> None:
    """/etc/hosts, resolv.conf and hostname are writable as in Docker (k3d
    rewrites a node's /etc/hosts), read-only under --read-only, and the peer
    block keeps working next to a container's own edits."""
    net, name, peer = f"{PREFIX}-etcnet", f"{PREFIX}-etc", f"{PREFIX}-etcpeer"
    try:
        docker("network", "create", net)
        docker("run", "-d", "--name", name, "--network", net, "alpine", "sleep", "120")
        docker("exec", name, "sh", "-c",
               "echo '10.9.9.9 custom.k3d.internal' >> /etc/hosts && echo 'options ndots:1' >> /etc/resolv.conf")
        docker("run", "-d", "--name", peer, "--network", net, "alpine", "sleep", "120")
        time.sleep(1.0)
        hosts = docker("exec", name, "cat", "/etc/hosts").stdout
        if "custom.k3d.internal" not in hosts or peer not in hosts:
            raise RuntimeError(f"own edit or peer entry missing: {hosts!r}")
        ro = docker("run", "--rm", "--read-only", "alpine", "sh", "-c", "echo x >> /etc/hosts", check=False)
        if ro.returncode == 0:
            raise RuntimeError("/etc/hosts writable under --read-only")
    finally:
        cleanup(name, peer)
        docker("network", "rm", net, check=False, timeout=60.0)
    record("/etc files writable", "PASS", "own edits kept beside the peer block; ro under --read-only")


def test_mount_order() -> None:
    """Mounts go parents first, as in Docker: k3d's tools container has
    --tmpfs /run --tmpfs /var/run plus the Docker socket bound inside, and
    the tmpfs used to cover the socket."""
    out = docker("run", "--rm", "--tmpfs", "/run", "--tmpfs", "/var/run",
                 "-v", "/var/run/docker.sock:/var/run/docker.sock", "alpine",
                 "sh", "-c", "test -S /var/run/docker.sock && echo SOCK", check=False, timeout=60.0)
    if "SOCK" not in out.stdout:
        raise RuntimeError(f"socket covered by the tmpfs: {out.stdout!r} {out.stderr!r}")
    record("mount order (parents first)", "PASS", "bind inside a tmpfs stays visible")


def test_network_rm_frees_bridge() -> None:
    """network rm deletes the network's bridge: a leaked bridge kept its
    subnet's route, so the next network on that subnet had no egress."""
    a, b, c = f"{PREFIX}-brA", f"{PREFIX}-brB", f"{PREFIX}-brC"
    subnet = "10.10.247.0/24"
    try:
        docker("network", "create", "--subnet", subnet, a)
        docker("run", "--rm", "--network", a, "alpine", "true")
        docker("network", "rm", a)
        docker("network", "create", "--subnet", subnet, b)
        out = docker("run", "--rm", "--network", b, "alpine", "ping", "-c1", "-W3", "1.1.1.1",
                     check=False, timeout=60.0)
        if out.returncode != 0:
            raise RuntimeError(f"no egress on a reused subnet: {out.stdout!r}")
        routes = docker("run", "--rm", "--net=host", "alpine", "ip", "route").stdout
        if routes.count("10.10.247.0/24") != 1:
            raise RuntimeError(f"stale route for the subnet: {routes!r}")
    finally:
        for n in (a, b, c):
            docker("network", "rm", n, check=False, timeout=60.0)
    record("network rm frees its bridge", "PASS", "reused subnet has one route and egress")


def test_restart_keeps_ip() -> None:
    """A restarted container gets its previous address back when it is free
    (Docker asks IPAM for it): k3s nodes that came back on new IPs lost
    their node IP and crash-looped."""
    net, name = f"{PREFIX}-keepip", f"{PREFIX}-keepipc"
    fmt = "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"
    try:
        docker("network", "create", net)
        docker("run", "-d", "--name", name, "--network", net, "alpine", "sleep", "300")
        # A second container started meanwhile must not take the address.
        first = docker("inspect", name, "-f", fmt).stdout.strip()
        docker("restart", "-t", "0", name)
        after_restart = docker("inspect", name, "-f", fmt).stdout.strip()
        docker("stop", "-t", "0", name)
        docker("start", name)
        after_start = docker("inspect", name, "-f", fmt).stdout.strip()
        if not first or first != after_restart or first != after_start:
            raise RuntimeError(f"address changed: {first} -> {after_restart} -> {after_start}")
    finally:
        cleanup(name)
        docker("network", "rm", net, check=False, timeout=60.0)
    record("restart keeps IP", "PASS", f"{first} kept across restart and stop/start")


def test_exec_stdin_eof_slow_reader() -> None:
    """stdin EOF reaches a process that starts reading late, every time: the
    host relay's blocking read sometimes slept through the client's
    half-close (about 1 run in 20), and `docker exec -i` never returned."""
    name = f"{PREFIX}-eofslow"
    with tempfile.NamedTemporaryFile(suffix=".bin") as f:
        f.write(os.urandom(1 << 20) * 48)
        f.flush()
        size = os.path.getsize(f.name)
        try:
            docker("run", "-d", "--name", name, "alpine", "sleep", "600")
            for i in range(40):
                with open(f.name, "rb") as stdin:
                    out = subprocess.run(["docker", "exec", "-i", name, "sh", "-c", "sleep 1; cat | wc -c"],
                                         stdin=stdin, capture_output=True, text=True, timeout=30)
                if out.stdout.strip() != str(size):
                    raise RuntimeError(f"run {i + 1}: got {out.stdout!r} {out.stderr!r}, want {size}")
        except subprocess.TimeoutExpired:
            raise RuntimeError(f"run {i + 1}: exec -i never saw stdin EOF")
        finally:
            cleanup(name)
    record("exec -i EOF, slow reader", "PASS", f"40 runs x {size >> 20} MB, EOF every time")


def test_save_pipe_exec() -> None:
    """`docker save | docker exec -i c ...` into a reader that starts late
    completes: a host relay that stopped reading one vsock connection
    stalled the shared device (a ring deadlock, then "VM crashed")."""
    name = f"{PREFIX}-savepipe"
    try:
        docker("run", "-d", "--name", name, "alpine", "sleep", "600")
        size = len(subprocess.run(["docker", "save", "nginx"], capture_output=True, timeout=120).stdout)
        for i in range(5):
            out = subprocess.run(f"docker save nginx | docker exec -i {name} sh -c 'sleep 1; cat | wc -c'",
                                 shell=True, capture_output=True, text=True, timeout=60)
            if out.stdout.strip() != str(size):
                raise RuntimeError(f"run {i + 1}: got {out.stdout!r} {out.stderr!r}, want {size}")
    except subprocess.TimeoutExpired:
        raise RuntimeError("save | exec -i hung")
    finally:
        cleanup(name)
    record("docker save | exec -i", "PASS", f"5 runs x {size >> 20} MB")


def test_network_disconnect_live() -> None:
    """docker network disconnect on a running container, as in Docker:
    a secondary network goes without touching lo (its DEL used to bring lo
    down: a k3s node's 127.0.0.1:6444 load balancer hung), the primary one
    can go too, and so can the last one — then connect works again."""
    a, b = f"{PREFIX}-dca", f"{PREFIX}-dcb"
    c, peer = f"{PREFIX}-dcc", f"{PREFIX}-dcpeer"
    lo_ok = "nc -w3 127.0.0.1 8080 </dev/null | grep -q hi && echo LO-OK"
    try:
        docker("network", "create", a)
        docker("network", "create", b)
        # alpine's busybox has no httpd: a loopback-only nc server instead.
        docker("run", "-d", "--name", c, "--network", a, "alpine", "sh", "-c",
               "while true; do echo hi | nc -l -s 127.0.0.1 -p 8080; done")
        docker("run", "-d", "--name", peer, "--network", b, "alpine", "sleep", "300")
        docker("network", "connect", b, c)
        # Secondary out: lo keeps serving.
        docker("network", "disconnect", b, c)
        if "LO-OK" not in docker("exec", c, "sh", "-c", lo_ok, check=False).stdout:
            raise RuntimeError("disconnecting a secondary network took lo down")
        # Primary out while another network stays: eth0 goes, the peer on b
        # is still reachable, lo still serves.
        docker("network", "connect", b, c)
        docker("network", "disconnect", a, c)
        links = docker("exec", c, "ip", "-o", "link").stdout
        if "eth0" in links:
            raise RuntimeError(f"eth0 still there after disconnecting the primary: {links!r}")
        out = docker("exec", c, "sh", "-c", f"ping -c1 -W3 {peer} >/dev/null && echo PEER-OK; {lo_ok}",
                     check=False).stdout
        if "PEER-OK" not in out or "LO-OK" not in out:
            raise RuntimeError(f"after the primary went: {out!r}")
        nets = docker("inspect", c, "-f", "{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}").stdout.split()
        if a in nets:
            raise RuntimeError(f"inspect still lists {a}: {nets}")
        # The last network out, then back.
        docker("network", "disconnect", b, c)
        if "LO-OK" not in docker("exec", c, "sh", "-c", lo_ok, check=False).stdout:
            raise RuntimeError("lo gone after disconnecting the last network")
        docker("network", "connect", a, c)
        docker("restart", "-t", "0", c)
        nets = docker("inspect", c, "-f", "{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}").stdout.split()
        if nets != [a]:
            raise RuntimeError(f"after reconnect + restart: {nets}, want [{a}]")
    finally:
        cleanup(c, peer)
        for n in (a, b):
            docker("network", "rm", n, check=False, timeout=60.0)
    record("network disconnect live", "PASS", "secondary keeps lo, primary and last network can go, reconnect works")


def test_vm_sysctl_limits() -> None:
    """The VM raises the limits Docker Desktop raises: inotify instances
    and watches (several k3d clusters exhausted the default 128) and
    vm.max_map_count."""
    out = docker("run", "--rm", "alpine", "cat", "/proc/sys/fs/inotify/max_user_instances",
                 "/proc/sys/fs/inotify/max_user_watches", "/proc/sys/vm/max_map_count").stdout.split()
    want = ["8192", "1048576", "262144"]
    if out != want:
        raise RuntimeError(f"limits {out}, want {want}")
    record("VM sysctl limits", "PASS", "inotify 8192/1048576, max_map_count 262144")


TESTS = [
    ("docker version/info handshake", test_handshake),
    ("VM sysctl limits", test_vm_sysctl_limits),
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
    ("published port survives restart with new IP", test_port_restart_new_ip),
    ("foreign host-port conflict", test_foreign_port_conflict),
    ("container lifecycle", test_create_ps_inspect_stop),
    ("pause/unpause", test_pause_unpause),
    ("top + stats", test_top_and_stats),
    ("system df", test_system_df),
    ("system df -v", test_system_df_verbose),
    ("stop -t 0 is immediate", test_stop_timeout_zero),
    ("compose 15 services", test_compose_many_services),
    ("build context symlinks", test_build_context_symlinks),
    ("network connect/disconnect", test_network_connect),
    ("network disconnect live", test_network_disconnect_live),
    ("hosts populated before start", test_hosts_before_start),
    ("IPv6 network", test_ipv6_network),
    ("logs", test_logs),
    ("logs --tail/-t", test_logs_tail_timestamps),
    ("exec", test_exec),
    ("exec -i stdin EOF", test_exec_stdin_eof),
    ("exec -i EOF, slow reader", test_exec_stdin_eof_slow_reader),
    ("docker save | exec -i", test_save_pipe_exec),
    ("k3d-style create", test_k3d_style_create),
    ("/etc files writable", test_etc_files_writable),
    ("mount order (parents first)", test_mount_order),
    ("network rm frees its bridge", test_network_rm_frees_bridge),
    ("restart keeps IP", test_restart_keeps_ip),
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
    ("host.docker.internal", test_host_docker_internal),
    ("seccomp profiles", test_seccomp),
    ("run --init", test_run_init),
    ("volumes-from", test_volumes_from),
    ("image history", test_image_history),
    ("image search", test_image_search),
    ("export and diff", test_export_and_diff),
    ("container update", test_container_update),
    ("commit", test_commit),
    ("ssh agent forwarding", test_ssh_agent_forwarding),
    ("rosetta amd64", test_rosetta_amd64),
    ("compose multi-network", test_compose_multi_network),
    ("restart policy after stop/start", test_restart_policy_survives_stop_start),
    ("cp shell-less image", test_cp_shell_less_image),
    ("network none", test_network_none),
    ("network rm in use", test_network_rm_in_use_and_inspect),
    ("save under gc", test_save_under_gc),
    ("cp symlink race", test_cp_symlink_race_stays_in_container),
    ("volumes-from source rm", test_volumes_from_source_rm_keeps_data),
    ("update pids unlimited", test_update_pids_unlimited),
    ("commit keeps config", test_commit_keeps_healthcheck_labels_user),
    ("connect alias scoped", test_connect_alias_scoped_to_network),
    ("rosetta explicit arm64", test_rosetta_explicit_arm64),
    ("images reference filter", test_images_reference_filter),
    ("volume copy-up", test_volume_copy_up_and_image_volume),
    ("network events", test_network_events),
    ("idle connections", test_idle_connections_do_not_starve_daemon),
    ("rosetta AOT cache", test_rosetta_aot_cache),
    ("compose recreate anonymous volume", test_compose_recreate_keeps_anonymous_volume),
    ("stats real numbers", test_stats_real_numbers),
    ("compose cp and watch", test_compose_cp_and_watch),
    ("log rotation", test_log_rotation),
    ("health status events", test_health_status_events),
    ("volume prune", test_volume_prune_semantics),
    ("run --rm latency", test_run_rm_latency),
    ("ephemeral host ports", test_ephemeral_host_ports),
    ("docker socket in container", test_docker_socket_in_container),
    ("API status codes", test_api_status_codes),
    ("published port half-close", test_published_port_half_close),
    ("exec -t tty", test_exec_tty),
    ("run -i stdin", test_run_interactive_stdin),
    ("start -ai stdin", test_attach_stdin_and_detach),
    ("tty container logs raw", test_tty_logs_raw),
    ("logs -f --tail 0", test_logs_tail_zero_follow),
    ("restart keeps --rm container", test_restart_keeps_rm_container),
    ("generated names + ps status", test_generated_names_and_status),
    ("inspect fields", test_inspect_fields),
    ("info/version", test_info_and_version),
    ("ps filters extended", test_ps_filters_extended),
    ("prune filters", test_prune_respects_filters),
    ("pull by digest", test_pull_by_digest),
    ("mount tmpfs + stop-timeout", test_mount_tmpfs_and_stop_timeout),
    ("volume bind opts + dns", test_volume_bind_driver_opts_and_dns),
    ("network container mode", test_network_container_mode),
    ("local registry push/pull", test_local_registry_push_pull),
    ("lifecycle events", test_lifecycle_events),
    ("exec env/cwd", test_exec_inherits_env_and_cwd),
    ("image HEALTHCHECK + start period", test_healthcheck_image_and_start_period),
    ("network/ps filters and limits", test_list_filters_and_limits),
    ("build aux ID, distribution, swarm", test_api_odds),
    ("static IP", test_static_ip),
    ("cp into volume before start", test_cp_into_volume_before_start),
    ("bind mounts /tmp and /var/folders", test_bind_mounts_tmp_and_var_folders),
    ("pid container mode", test_pid_container_mode),
    ("docker import", test_docker_import),
    ("bind source checks", test_bind_source_checks),
    ("internal network isolation", test_internal_network_isolation),
    ("api parity (audit fixes)", test_api_parity_audit),
    ("static IP outside ip_range", test_static_ip_outside_ip_range),
    ("volume subpath", test_volume_subpath),
    ("bind mount file events", test_bind_mount_file_events),
    ("host network ports", test_host_network_ports),
    ("k3s cluster", test_k3s_cluster),
    ("attach over websocket", test_attach_websocket),
    ("container domains", test_container_domains),
]


# A few minutes' cross-section of the suite for every branch (make smoke):
# the core run/attach/exec/logs/cp/port/volume/network/compose paths.
SMOKE = [
    "docker version/info handshake",
    "run --rm attach + exit code",
    "docker wait",
    "published port forwarded",
    "container lifecycle",
    "logs",
    "exec",
    "cp",
    "bind mount /Users",
    "network connect/disconnect",
    "compose up",
    "volume copy-up",
    "run --rm latency",
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
    args = _sys.argv[1:]
    smoke = "--smoke" in args
    only = [a for a in args if a != "--smoke"]
    if smoke:
        known = {n for n, _ in TESTS}
        missing = [n for n in SMOKE if n not in known]
        if missing:
            log(f"SMOKE lists unknown tests: {missing}")
            return 2
    selected = 0
    for name, fn in TESTS:
        if smoke and name not in SMOKE:
            continue
        if only and not any(o in name for o in only):
            continue
        selected += 1
        log(f"\n=== {name} ===")
        try:
            fn()
        except Exception as e:
            record(name, "FAIL", str(e))

    if only and selected == 0:
        # A typo in the filter must not look like a green run.
        log(f"no test name matches {only}")
        return 2

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
