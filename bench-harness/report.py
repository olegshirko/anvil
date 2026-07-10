#!/usr/bin/env python3
"""Собирает results/<ts>.csv в сравнительную markdown-таблицу.
Использование: report.py results/20260710-120000.csv > results/latest.md
"""
import csv
import sys
from collections import defaultdict

def main():
    if len(sys.argv) != 2:
        print("Usage: report.py <csv>", file=sys.stderr)
        sys.exit(1)

    rows = defaultdict(dict)  # rows[backend][(phase,metric)] = value
    with open(sys.argv[1]) as f:
        reader = csv.DictReader(f)
        for r in reader:
            key = (r["phase"], r["metric"])
            rows[r["backend"]][key] = int(float(r["value_ms"]))

    metrics = [
        ("cold_start", "daemon_ready", "Cold start: daemon ready"),
        ("cold_start", "compose_up_healthy", "Cold start: compose up (all healthy)"),
        ("resume", "daemon_ready", "Resume: daemon ready"),
        ("resume", "compose_up_healthy", "Resume: compose up (all healthy)"),
        ("steady_state", "idle_rss_mb", "Idle RSS (MB)"),
    ]

    backends = list(rows.keys())
    # vz-runner первым, если есть — это наш продукт, README читает сверху вниз
    backends.sort(key=lambda b: (b != "vz-runner", b))

    print("| Metric | " + " | ".join(backends) + " |")
    print("|---|" + "---|" * len(backends))

    for phase, metric, label in metrics:
        cells = []
        values = {}
        for b in backends:
            v = rows[b].get((phase, metric))
            values[b] = v
            cells.append(f"{v} ms" if v is not None and metric != "idle_rss_mb"
                          else (f"{v} MB" if v is not None else "—"))

        # bold the best (lowest) value among those present
        present = {b: v for b, v in values.items() if v is not None}
        if present:
            best_backend = min(present, key=present.get)
            best_idx = backends.index(best_backend)
            cells[best_idx] = f"**{cells[best_idx]}**"

        print(f"| {label} | " + " | ".join(cells) + " |")

    # speedup summary relative to Docker Desktop, if present
    if "docker-desktop" in rows and "vz-runner" in rows:
        base = rows["docker-desktop"].get(("cold_start", "compose_up_healthy"))
        ours = rows["vz-runner"].get(("cold_start", "compose_up_healthy"))
        if base and ours:
            print()
            print(f"vz-runner cold-start-to-ready is **{base/ours:.1f}x faster** "
                  f"than Docker Desktop on this workload.")
        base_r = rows["docker-desktop"].get(("resume", "compose_up_healthy"))
        ours_r = rows["vz-runner"].get(("resume", "compose_up_healthy"))
        if base_r and ours_r:
            print(f"vz-runner resume-to-ready is **{base_r/ours_r:.1f}x faster** "
                  f"than a warm Docker Desktop restart on this workload.")

if __name__ == "__main__":
    main()
