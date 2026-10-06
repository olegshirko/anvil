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

OURS = "vz-runner"

# (phase, metric, table label). Lower is better for every row.
METRICS = [
    ("cold_start", "daemon_ready", "Cold start: daemon ready"),
    ("cold_start", "compose_up_healthy", "Cold start: compose up (all healthy)"),
    ("resume", "daemon_ready", "Resume: daemon ready"),
    ("resume", "compose_up_healthy", "Resume: compose up (all healthy)"),
    ("steady_state", "idle_rss_mb", "Idle RSS (MB)"),
    ("ops", "run_rm", "Ops: docker run --rm"),
    ("ops", "stop_t0", "Ops: stop -t 0"),
    ("ops", "compose_down", "Ops: compose down"),
    ("ops", "compose15_up", "Ops: compose up 15 services"),
    ("ops", "compose15_down", "Ops: compose down 15 services"),
]


def _render_table(aggregate):
    backends = sorted(
        set(r["backend"] for r in aggregate),
        key=lambda b: (b != OURS, b),
    )
    metrics = METRICS

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


def _compare(ours, theirs, is_memory):
    """Phrase ours vs theirs (lower is better) without rounding a loss into a win."""
    if ours < theirs:
        word = "less memory than" if is_memory else "faster than"
        ratio = theirs / ours
    else:
        word = "more memory than" if is_memory else "slower than"
        ratio = ours / theirs
    if f"{ratio:.1f}" == "1.0":
        return "about the same as"
    return f"{ratio:.1f}× {word}"


def _render_summary(backends, rows):
    """One line per metric: vz-runner against every other backend measured."""
    others = [b for b in backends if b != OURS]
    if OURS not in backends or not others:
        return ""

    def num(b, key):
        v = rows[b].get(key)
        return v if isinstance(v, int) and v > 0 else None

    lines = []
    for phase, metric, label in METRICS:
        ours = num(OURS, (phase, metric))
        if ours is None:
            continue
        is_memory = metric == "idle_rss_mb"
        unit = "MB" if is_memory else "ms"
        parts = []
        for b in others:
            theirs = num(b, (phase, metric))
            if theirs is None:
                continue
            parts.append(f"{_compare(ours, theirs, is_memory)} {b} ({theirs} {unit})")
        if parts:
            lines.append(f"- **{label}** — {OURS} {ours} {unit}: " + "; ".join(parts))
    if not lines:
        return ""
    return f"\n{OURS} compared with each backend (lower is better):\n\n" + "\n".join(lines) + "\n"


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

    md_content += _render_summary(backends, rows)

    with open(LATEST_MD, "w") as f:
        f.write(md_content)

    print("Updated aggregate: " + LATEST_CSV)
    print("Markdown table: " + LATEST_MD)


if __name__ == "__main__":
    main()
