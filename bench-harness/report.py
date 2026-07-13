#!/usr/bin/env python3
"""Update aggregate results/latest.csv and regenerate results/latest.md.

Usage: report.py results/20260710-120000.csv

Logic:
- latest.csv keeps the last known value for each backend × phase × metric.
- A new run only updates backends present in that run; other results are kept.
  This lets you remeasure one backend (e.g. docker-desktop) without losing data.
- latest.md is a single summary table generated from latest.csv.
"""
import csv
import sys
import os
from collections import defaultdict

RESULTS_DIR = os.path.join(os.path.dirname(__file__), "results")
LATEST_CSV = os.path.join(RESULTS_DIR, "latest.csv")
LATEST_MD = os.path.join(RESULTS_DIR, "latest.md")


def _render_table(aggregate):
    backends = sorted(
        set(r["backend"] for r in aggregate),
        key=lambda b: (b != "vz-runner", b),
    )

    metrics = [
        ("cold_start", "daemon_ready", "Cold start: daemon ready"),
        ("cold_start", "compose_up_healthy", "Cold start: compose up (all healthy)"),
        ("resume", "daemon_ready", "Resume: daemon ready"),
        ("resume", "compose_up_healthy", "Resume: compose up (all healthy)"),
        ("steady_state", "idle_rss_mb", "Idle RSS (MB)"),
    ]

    rows = defaultdict(dict)
    for r in aggregate:
        try:
            rows[r["backend"]][(r["phase"], r["metric"])] = int(float(r["value_ms"]))
        except ValueError:
            rows[r["backend"]][(r["phase"], r["metric"])] = r["value_ms"]

    lines = ["| Metric | " + " | ".join(backends) + " |",
             "|---|" + "---|" * len(backends)]

    for phase, metric, label in metrics:
        cells = []
        values = {}
        for b in backends:
            v = rows[b].get((phase, metric))
            values[b] = v
            unit = " MB" if metric == "idle_rss_mb" else " ms"
            cells.append(f"{v}{unit}" if v is not None else "—")

        present = {b: v for b, v in values.items() if v is not None}
        if present:
            best_backend = min(present, key=present.get)
            best_idx = backends.index(best_backend)
            cells[best_idx] = f"**{cells[best_idx]}**"

        lines.append(f"| {label} | " + " | ".join(cells) + " |")

    return "\n".join(lines), backends, rows


def main():
    if len(sys.argv) != 2:
        print("Usage: report.py <csv>", file=sys.stderr)
        sys.exit(1)

    run_csv = sys.argv[1]
    os.makedirs(RESULTS_DIR, exist_ok=True)

    aggregate = []
    if os.path.exists(LATEST_CSV):
        with open(LATEST_CSV) as f:
            aggregate = list(csv.DictReader(f))

    with open(run_csv) as f:
        run_rows = list(csv.DictReader(f))

    # Overwrite only backends that appear in this run.
    run_backends = set(r["backend"] for r in run_rows)
    aggregate = [r for r in aggregate if r["backend"] not in run_backends]
    aggregate.extend(run_rows)

    with open(LATEST_CSV, "w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=["backend", "phase", "metric", "value_ms"])
        writer.writeheader()
        writer.writerows(aggregate)

    table, backends, rows = _render_table(aggregate)
    md_content = "# Anvil bench harness results\n\n" + table + "\n"

    if "docker-desktop" in backends and "vz-runner" in backends:
        base = rows["docker-desktop"].get(("cold_start", "compose_up_healthy"))
        ours = rows["vz-runner"].get(("cold_start", "compose_up_healthy"))
        if base and ours:
            md_content += f"\nvz-runner cold-start-to-ready is **{base/ours:.1f}x faster** " \
                          f"than Docker Desktop on this workload.\n"
        base_r = rows["docker-desktop"].get(("resume", "compose_up_healthy"))
        ours_r = rows["vz-runner"].get(("resume", "compose_up_healthy"))
        if base_r and ours_r:
            md_content += f"vz-runner resume-to-ready is **{base_r/ours_r:.1f}x faster** " \
                          f"than a warm Docker Desktop restart on this workload.\n"

    with open(LATEST_MD, "w") as f:
        f.write(md_content)

    print("Updated aggregate: " + LATEST_CSV)
    print("Markdown table: " + LATEST_MD)


if __name__ == "__main__":
    main()
