#!/usr/bin/env python3
"""Per-step medians of the CLI-shaped Docker API flow.

The single-run trace in time_api.py shows one sample, which is dominated by
outliers on a loaded host. This repeats the whole flow and reports the median
of every step, so the remaining latency is attributed to phases rather than to
whichever run was sampled.

    python3 scripts/time_api_steps.py --runs 20
"""

import argparse
import json
import re
import socket
import statistics
import statistics as st
import time
from pathlib import Path

from time_api import APIConn, WaitHandle, api_req


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--sock", default=str(Path.home() / ".anvil-vz-dev" / "docker.sock"))
    ap.add_argument("--runs", type=int, default=20)
    ap.add_argument("--image", default="alpine")
    args = ap.parse_args()
    sock = args.sock

    steps = ["create", "attach-open", "wait-headers", "start",
             "attach-eof", "wait-exit", "inspect", "rm"]
    samples: dict[str, list[float]] = {s: [] for s in steps}
    totals: list[float] = []

    for run in range(args.runs + 2):  # two warm-ups
        marks: dict[str, float] = {}
        t0 = time.perf_counter()

        def mark(name: str) -> None:
            marks[name] = (time.perf_counter() - t0) * 1000

        r = api_req(sock, "POST", "/v1.51/containers/create",
                    {"Image": args.image, "Cmd": ["true"], "HostConfig": {"AutoRemove": False}})
        cid = json.loads(r)["Id"]
        mark("create")
        c = APIConn(sock)
        c.send("POST", f"/v1.51/containers/{cid}/attach?stream=1&stdout=1&stderr=1")
        mark("attach-open")
        w = WaitHandle(sock, f"/v1.51/containers/{cid}/wait?condition=next-exit")
        mark("wait-headers")
        api_req(sock, "POST", f"/v1.51/containers/{cid}/start")
        mark("start")
        try:
            while c.read_response():
                pass
        except Exception:
            pass
        mark("attach-eof")
        w.finish()
        mark("wait-exit")
        api_req(sock, "GET", f"/v1.51/containers/{cid}/json")
        mark("inspect")
        api_req(sock, "DELETE", f"/v1.51/containers/{cid}?force=true&v=false")
        mark("rm")
        c.close()

        if run >= 2:
            totals.append(marks["rm"])
            prev = 0.0
            for s in steps:
                samples[s].append(marks[s] - prev)
                prev = marks[s]

    print(f"{args.runs} runs (`{args.image} true`), per-step medians (cumulative at right):")
    cum = 0.0
    for s in steps:
        v = statistics.median(samples[s])
        cum += v
        print(f"  {s:14s} {v:7.1f} ms   (cum {cum:6.1f})")
    print(f"  {'TOTAL':14s} {statistics.median(totals):7.1f} ms median | "
          f"min {min(totals):.1f} | p90 {sorted(totals)[int(len(totals) * 0.9)]:.1f}")


if __name__ == "__main__":
    main()
