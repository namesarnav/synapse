#!/usr/bin/env python3
"""Prints the results tables for docs/benchmarks from the recorded JSON files."""
import glob, json, os, sys

d = sys.argv[1] if len(sys.argv) > 1 else "docs/benchmarks/results"
load = [json.load(open(f)) | {"file": os.path.basename(f)} for f in sorted(glob.glob(d + "/*.json")) if not os.path.basename(f).startswith("ws-")]
print("| run | executions | tasks | submit/s | executions/s | tasks/s | submit p50/p95/p99 (ms) | end-to-end p50/p95/p99 (ms) | failed |")
print("| --- | ---: | ---: | ---: | ---: | ---: | --- | --- | ---: |")
for r in load:
    s, e = r["submit_latency_ms"], r["end_to_end_ms"]
    print(f"| `{r['file'][:-5]}` | {r['accepted']} | {r['tasks']} | {r['submit_per_s']:.0f} | {r['completed_per_s']:.0f} | {r['tasks_per_s']:.0f} | "
          f"{s['p50']:.0f} / {s['p95']:.0f} / {s['p99']:.0f} | {e['p50']:.0f} / {e['p95']:.0f} / {e['p99']:.0f} | {r['failed'] + r['unfinished']} |")

ws = [json.load(open(f)) | {"file": os.path.basename(f)} for f in sorted(glob.glob(d + "/ws-*.json"))]
if ws:
    print()
    print("| run | subscribers | events received | p50 / p95 / p99 / max (ms) |")
    print("| --- | ---: | ---: | --- |")
    for r in ws:
        a = r["events_all"]
        subs = r["subscribers"] if r["mode"] == "workspace" else "1 per execution"
        print(f"| `{r['file'][:-5]}` | {subs} | {a['count']} | {a['p50_ms']:.1f} / {a['p95_ms']:.1f} / {a['p99_ms']:.1f} / {a['max_ms']:.1f} |")
