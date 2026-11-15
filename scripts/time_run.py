#!/usr/bin/env python3
"""Time bare `docker run` start latency against an anvil instance.

Runs from the project root:
    python3 scripts/time_run.py                 # dev instance, 20 runs
    python3 scripts/time_run.py --runs 50 --context anvil

It does:
1. 3 warmup runs (discarded: page cache, shim, containerd warm paths)
2. N timed `docker run --rm <image> <cmd>` runs, median/percentiles
3. parses the [start-trace] lines the debug guest-agent writes into
   guest-agent.log (share root = project dir) and prints the per-phase
   breakdown (microseconds between marks, median across runs)

Comparable figures (Kiri README, `alpine true` via API / CLI, median of 30):
Kiri 2.55/14.2 ms, Docker Desktop 213/243 ms, OrbStack 380/442 ms.
"""

import argparse
import re
import statistics
import subprocess
import time
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parent.parent


def trace_log_for(context: str) -> Path:
    """Named instances (anvil-dev, anvil-ci...) keep their share root in the
    state dir; the plain instance shares the project directory."""
    m = re.fullmatch(r"anvil-(\S+)", context)
    if m:
        return Path.home() / f".anvil-vz-{m.group(1)}" / ".anvil-run" / "guest-agent.log"
    return PROJECT_ROOT / ".anvil-run" / "guest-agent.log"

TRACE_RE = re.compile(
    r"\[start-trace\] (\S+) (\S+) (\S+) at=(\d+)(?: d=(\d+))?"
)
TRACE_EXIT_RE = re.compile(
    r"\[start-trace\] wait (\S+) exit after=(\d+)us"
)


def run_once(context: str, image: str, cmd: str) -> float:
    t0 = time.perf_counter()
    subprocess.run(
        ["docker", "--context", context, "run", "--rm", image, cmd],
        check=True, capture_output=True,
    )
    return (time.perf_counter() - t0) * 1000.0


def trace_lines_since(trace_log: Path, offset: int) -> list[str]:
    if not trace_log.exists():
        return []
    text = trace_log.read_text(errors="replace")
    return [l for l in text[offset:].splitlines() if "[start-trace]" in l]


def fmt_us(us: float) -> str:
    return f"{us / 1000:.2f} ms" if us >= 1000 else f"{us:.0f} µs"


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--runs", type=int, default=20)
    ap.add_argument("--warmup", type=int, default=3)
    ap.add_argument("--image", default="alpine")
    ap.add_argument("--cmd", default="true")
    ap.add_argument("--context", default="anvil-dev")
    ap.add_argument("--trace-log", type=Path, default=None,
                    help="guest-agent.log to parse (default: resolved from --context)")
    args = ap.parse_args()
    trace_log = args.trace_log or trace_log_for(args.context)

    # Verify the engine answers before timing anything.
    subprocess.run(
        ["docker", "--context", args.context, "version", "--format", "{{.Server.Version}}"],
        check=True, capture_output=True,
    )

    for _ in range(args.warmup):
        run_once(args.context, args.image, args.cmd)

    samples: list[float] = []
    phases: dict[str, list[float]] = {}  # phase -> [d in µs]
    exit_waits: list[float] = []
    for _ in range(args.runs):
        offset = trace_log.stat().st_size if trace_log.exists() else 0
        samples.append(run_once(args.context, args.image, args.cmd))
        for line in trace_lines_since(trace_log, offset):
            if m := TRACE_EXIT_RE.search(line):
                exit_waits.append(int(m.group(2)))
                continue
            m = TRACE_RE.search(line)
            if m and m.group(5) is not None:
                phases.setdefault(m.group(3), []).append(int(m.group(5)))

    samples.sort()
    n = len(samples)
    print(f"\n{args.runs} timed `docker run --rm {args.image} {args.cmd}` ({args.context}):")
    print(f"  median {statistics.median(samples):.1f} ms | min {samples[0]:.1f} ms | "
          f"max {samples[-1]:.1f} ms | p10 {samples[n // 10]:.1f} ms | p90 {samples[(9 * n) // 10]:.1f} ms")

    if exit_waits:
        print(f"\n  /wait exit observed after: median {fmt_us(statistics.median(exit_waits))}")

    if phases:
        print("\n  guest-agent phases (median µs between marks, in call order):")
        for phase, vals in phases.items():
            print(f"    {phase:16s} {fmt_us(statistics.median(vals)):>10s}   (n={len(vals)})")
    else:
        print(f"\n  no [start-trace] lines in {trace_log} — cold-boot the dev "
              "instance with make dev-rebuild so ANVIL_DEBUG is on")


if __name__ == "__main__":
    main()
