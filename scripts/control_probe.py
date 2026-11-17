#!/usr/bin/env python3
"""Send one control request to an anvil instance's control socket.

    python3 scripts/control_probe.py status --sock ~/.anvil-vz-dev/control.sock

The wire format is a 4-byte big-endian length prefix followed by JSON, the
same framing the daemon and the guest share. Used to check what the guest
answers without going through the CLI (whose `status` maps to "health").
"""

import argparse
import json
import socket
import struct
import sys
from pathlib import Path


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("cmd", nargs="?", default="status")
    ap.add_argument("--sock", default=str(Path.home() / ".anvil-vz-dev" / "control.sock"))
    ap.add_argument("--timeout", type=float, default=15.0)
    ap.add_argument("--internal", action="store_true",
                    help="mark the request as the daemon's own probe (no client accounting)")
    args = ap.parse_args()

    req: dict = {"cmd": args.cmd}
    if args.internal:
        req["internalRequest"] = True
    body = json.dumps(req).encode()
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(args.timeout)
    s.connect(args.sock)
    s.sendall(struct.pack(">I", len(body)) + body)

    head = b""
    while len(head) < 4:
        chunk = s.recv(4 - len(head))
        if not chunk:
            sys.exit("connection closed before the length prefix")
        head += chunk
    want = struct.unpack(">I", head)[0]
    data = b""
    while len(data) < want:
        chunk = s.recv(want - len(data))
        if not chunk:
            break
        data += chunk
    print(data.decode(errors="replace"))


if __name__ == "__main__":
    main()
