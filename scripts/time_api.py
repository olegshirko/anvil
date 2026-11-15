#!/usr/bin/env python3
"""Time bare Docker API create/wait/start/exit/rm against an anvil socket.

Bypasses the docker CLI entirely: the full sequence is proxy + vsock +
guest-agent cost — comparable to Kiri's published 2.55 ms (same op, API).

/docker run ordering quirk: the CLI calls /wait before /start and the
engine must answer /wait's headers at once. So /wait goes out on its own
connection (headers only), /start on a second one, and only then does the
wait body (the exit) complete.

    python3 scripts/time_api.py                     # dev instance
    python3 scripts/time_api.py --sock ~/.anvil-vz/docker.sock
"""

import argparse
import json
import re
import socket
import statistics
import time
from pathlib import Path


class APIConn:
    """A single keep-alive connection; the engine never EOFs mid-body, so
    responses are read by Content-Length or chunked framing."""

    def __init__(self, sock_path: str) -> None:
        self.path = sock_path
        self.s: socket.socket | None = None
        self.buf = b""

    def _connect(self) -> None:
        self.s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.s.connect(self.path)
        self.buf = b""

    def _recv(self) -> bytes:
        b = self.s.recv(65536)
        if not b:
            raise ConnectionError("engine closed the connection")
        return b

    def _read_until(self, sep: bytes) -> bytes:
        while sep not in self.buf:
            self.buf += self._recv()
        head, _, self.buf = self.buf.partition(sep)
        return head

    def send(self, method: str, path: str, body=None) -> None:
        if self.s is None:
            self._connect()
        data = json.dumps(body).encode() if body is not None else b""
        hdr = (f"{method} {path} HTTP/1.1\r\nHost: docker\r\n"
               f"Content-Type: application/json\r\nContent-Length: {len(data)}\r\n\r\n").encode()
        self.s.sendall(hdr + data)

    def read_response(self) -> bytes:
        """Read one full response body (headers must not have been consumed)."""
        head = self._read_until(b"\r\n\r\n")
        headers = head.decode("latin-1").lower()
        out = self.buf
        if "transfer-encoding: chunked" in headers:
            while True:
                size_line = self._read_until(b"\r\n")
                size = int(size_line.split(b";")[0], 16)
                while len(self.buf) < size + 2:
                    self.buf += self._recv()
                chunk, self.buf = self.buf[:size], self.buf[size + 2:]
                if size == 0:
                    break
                out += chunk
        elif m := re.search(r"content-length: (\d+)", headers):
            want = int(m.group(1))
            while len(self.buf) < want:
                self.buf += self._recv()
            out, self.buf = self.buf[:want], self.buf[want:]
        else:
            # No framing at all: read until EOF (hijacked streams aside,
            # plain responses always carry one of the framings above).
            while True:
                try:
                    self.buf += self._recv()
                except ConnectionError:
                    break
            out, self.buf = self.buf, b""
        return out

    def read_headers_only(self) -> None:
        head = self._read_until(b"\r\n\r\n")
        _ = head

    def close(self) -> None:
        if self.s is not None:
            self.s.close()
            self.s = None


def api_req(sock_path: str, method: str, path: str, body=None) -> bytes:
    c = APIConn(sock_path)
    try:
        c.send(method, path, body)
        return c.read_response()
    finally:
        c.close()


class WaitHandle:
    """/wait armed on its own connection: headers return at once (the CLI
    proceeds to /start), the body completes when the container exits."""

    def __init__(self, sock_path: str, path: str) -> None:
        self.conn = APIConn(sock_path)
        self.conn.send("POST", path)
        self.conn.read_headers_only()
        self.done = False

    def finish(self) -> None:
        """Block until the exit is observed, then release the connection."""
        if not self.done:
            self.conn.read_response()
            self.done = True
        self.conn.close()


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--sock", default=str(Path.home() / ".anvil-vz-dev" / "docker.sock"))
    ap.add_argument("--runs", type=int, default=15)
    ap.add_argument("--image", default="alpine")
    args = ap.parse_args()
    sock = args.sock

    def hijack_open(path: str) -> APIConn:
        """Open a hijacked stream (docker attach): after the 101 response the
        connection is a raw byte stream."""
        c = APIConn(sock)
        data = b""
        hdr = (f"POST {path} HTTP/1.1\r\nHost: docker\r\n"
               f"Connection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n").encode()
        c._connect()
        c.s.sendall(hdr)
        head = c._read_until(b"\r\n\r\n")
        c.hijacked = "101" in head.split(b"\r\n")[0].decode("latin-1")
        return c

    def full_op(steps: bool = False) -> float:
        """The docker CLI's own flow: create, attach (hijack, held open),
        arm /wait, start, read attach until EOF, inspect, rm."""
        marks: list[tuple[str, float]] = []
        t0 = time.perf_counter()
        mark = lambda name: marks.append((name, (time.perf_counter() - t0) * 1000))

        r = api_req(sock, "POST", "/v1.51/containers/create",
                    {"Image": args.image, "Cmd": ["true"], "HostConfig": {"AutoRemove": False}})
        cid = json.loads(r)["Id"]
        mark("create")
        a = hijack_open(f"/v1.51/containers/{cid}/attach?stream=1&stdout=1&stderr=1")
        mark("attach-open")
        w = WaitHandle(sock, f"/v1.51/containers/{cid}/wait?condition=next-exit")
        mark("wait-headers")
        api_req(sock, "POST", f"/v1.51/containers/{cid}/start")
        mark("start")
        # attach stream runs until the container's output ends
        try:
            while True:
                b = a.s.recv(65536)
                if not b:
                    break
        except (ConnectionError, OSError):
            pass
        mark("attach-eof")
        w.finish()
        mark("wait-exit")
        api_req(sock, "GET", f"/v1.51/containers/{cid}/json")
        mark("inspect")
        api_req(sock, "DELETE", f"/v1.51/containers/{cid}?force=true&v=false")
        mark("rm")
        a.close()
        if steps:
            print("    " + "  ".join(f"{n}={t:.1f}" for n, t in marks))
        return marks[-1][1]

    full_op()  # warm
    print("first run, step by step:")
    full_op(steps=True)
    vals = sorted(full_op() for _ in range(args.runs))
    print(f"{args.runs} runs, CLI-shaped flow via API on {sock}:")
    print(f"  full op: median {statistics.median(vals):.1f} ms | "
          f"min {vals[0]:.1f} ms | max {vals[-1]:.1f} ms")
    vals = []
    for _ in range(args.runs):
        t0 = time.perf_counter()
        api_req(sock, "GET", "/_ping")
        vals.append((time.perf_counter() - t0) * 1000)
    print(f"  GET /_ping roundtrip: median {statistics.median(vals):.2f} ms "
          f"(host proxy + vsock + guest HTTP overhead per call)")


if __name__ == "__main__":
    main()
