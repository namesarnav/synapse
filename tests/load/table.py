#!/usr/bin/env python3
"""Prints the results table for docs/benchmarks from the recorded JSON files."""
import glob, json, os, sys

d = sys.argv[1] if len(sys.argv) > 1 else "docs/benchmarks/results"
rows = [json.load(open(f)) | {"file": os.path.basename(f)} for f in sorted(glob.glob(d + "/*.json"))]
print("| run | executions | tasks | submit/s | executions/s | tasks/s | submit p50/p95/p99 (ms) | end-to-end p50/p95/p99 (ms) | failed |")
print("| --- | ---: | ---: | ---: | ---: | ---: | --- | --- | ---: |")
for r in rows:
    s, e = r["submit_latency_ms"], r["end_to_end_ms"]
    print(f"| `{r['file'][:-5]}` | {r['accepted']} | {r['tasks']} | {r['submit_per_s']:.0f} | {r['completed_per_s']:.0f} | {r['tasks_per_s']:.0f} | "
          f"{s['p50']:.0f} / {s['p95']:.0f} / {s['p99']:.0f} | {e['p50']:.0f} / {e['p95']:.0f} / {e['p99']:.0f} | {r['failed'] + r['unfinished']} |")
