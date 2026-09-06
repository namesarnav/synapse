# Synapse: Distributed Workflow Automation Engine

Go, PostgreSQL, Redis, React, TypeScript, Docker, Kubernetes, Prometheus, OpenTelemetry

- Built **Synapse**, a distributed workflow engine (Go, PostgreSQL, React/TypeScript), using `SKIP LOCKED` leases with fencing tokens and transactional state advancement. It sustained 443 tasks/s (148 executions/s) on a single 4-core laptop, with zero failures across 6,000 executions at 2× capacity.

- Achieved end-to-end p50/p95/p99 of 17/30/44 ms at 100 executions/s, and a 4 ms serial claim-begin-complete cycle. I did this by running the queue, execution advance and event log in PostgreSQL, with workers as separate processes, so no broker is needed.

- Proved crash recovery with real-process failure injection (SIGKILL workers, killed API, Postgres restart), where fenced completions kept each task's result recorded exactly once. I ran 205 Go tests under the race detector, at 79.9% statement coverage.

- Designed a real-time event pipeline using a persisted event log, bounded WebSocket fan-out, and a shared DB tail with Redis as an optional accelerator. Push latency was p50 3-5 ms at 100 concurrent subscribers (90,000 deliveries, none lost), and with Redis off delivery stayed complete, only slower (about 2 s).

- Wrote a from-scratch expression language (parser, evaluator, templates, no `eval`), an SSRF-guarded HTTP node, AES-256-GCM secrets, and node-level replay. I put a React Flow visual DAG editor in front of it, plus Docker/Kubernetes deployment and Prometheus and OpenTelemetry tracing (verified end to end in the compose stack).

<!-- Benchmarks: single host (Intel i5-7300HQ, 4 cores, load generator, API, workers and Postgres sharing the same cores); the throughput figure used a relaxed-durability Postgres (fsync=off), 195 tasks/s with fsync=on. -->
