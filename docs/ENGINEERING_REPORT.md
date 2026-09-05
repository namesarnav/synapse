# Synapse engineering report

Synapse is a durable workflow automation engine: a visual DAG editor in front of a Go backend that executes workflows through a PostgreSQL-backed lease queue. This report states what exists, what was measured, and what is not done. Numbers come from runs recorded in `docs/benchmarks/results/` or from commands listed below; nothing is estimated.

## 1. Final architecture

```
                 ┌─────────────┐   HTTP / WS    ┌─────────┐
   browser ────▶ │ nginx (web) │ ─────────────▶ │   API   │──┐ (scheduler optional in-process)
   webhook ────▶ └─────────────┘                └────┬────┘  │
                                                     │       ▼
                       Redis (optional, latency-only)│   PostgreSQL  ◀── source of truth: workflows, versions,
                                 ▲                   │       ▲           executions, tasks (the queue),
                                 └───── pub/sub ─────┤       │           execution_events, secrets, sessions
                                                     │   ┌───┴────┐
                                                     └──▶│ Worker │ × N   claim (SKIP LOCKED) → run → complete(token)
                                                         └────────┘
                                    Scheduler × N (idempotent, row-locked, any number may run): reap leases, wake delays, fire cron, sweep
```

- **PostgreSQL is the only authority.** The task queue, leases, execution state, events, replay lineage and secrets all live there. Redis is optional and carries only live event fan-out between processes; deleting it loses latency, not data (ADR 0008).
- **API, worker and scheduler are separate binaries** (`apps/api`, `apps/worker`, `apps/scheduler`). Workers execute tasks outside the API process. The API can run the scheduler in-process for small deployments.
- **A pure engine core** (`internal/engine`) decides what runs next given a graph and node states. The same code drives the in-memory `localrun` runner (used by tests and as a reference implementation) and the PostgreSQL runtime (ADR 0009).
- **Workflow versions are immutable.** Publishing creates a version; every execution pins one (ADR 0004).

Details: `docs/architecture/architecture.md`, `docs/adr/0001`–`0010`.

## 2. Repository structure

| Path | Contents |
| --- | --- |
| `apps/api`, `apps/worker`, `apps/scheduler` | the three Go entry points |
| `apps/web` | React 18 + Vite + TypeScript UI (React Flow editor, zustand, vitest) |
| `packages/protocol` | TypeScript types shared with the web app; a Go test checks them against the server's JSON |
| `internal/engine`, `workflow`, `localrun`, `expressions`, `cron` | graph model and validation, pure scheduler core, in-memory runner, expression language, cron |
| `internal/runtime`, `worker`, `scheduler`, `triggers`, `nodes` | durable execution, leases, replay, workers, cron/webhook triggers, node executors |
| `internal/api`, `auth`, `secrets`, `ratelimit`, `realtime` | HTTP/WS layer, sessions and roles, AES-GCM secrets, limiters, hub and Redis bus |
| `internal/persistence`, `migrations` | pgx pool, advisory-locked migrations (5 SQL files) |
| `internal/telemetry`, `tracing`, `logging` | Prometheus metrics, OTLP tracing, slog with request/execution context |
| `tests/integration`, `e2e`, `load`, `protocol`, `stack` | failure injection, webhook flow, load tools, contract test, process harness |
| `deployments/docker`, `deployments/kubernetes`, `docker-compose*.yml` | images, nginx, Prometheus config, compose stacks, manifests |
| `docs/` | architecture, expression language, 10 ADRs, benchmarks (raw JSON included) |

About 20.9k lines of Go (tests included) and 3.7k lines of TypeScript.

## 3. Implemented functionality

- Registration, login, opaque sessions (Bearer or cookie with CSRF), argon2id, workspaces with viewer/member/admin/owner roles.
- Visual editor: palette, drag-and-drop, edge validation, node inspector, versions, publish, run, webhook URL panel, minimap. Graph validation is server-side (cycles, unreachable nodes, bad expressions, bad cron, bad references).
- Execution: concurrent branches, conditions, merge, foreach with bounded child concurrency, sub-workflows with a depth limit, durable delays, retries with backoff, per-node timeouts, stop node, cancellation.
- Expression language written from scratch (no eval, no framework): parser, evaluator, `{{ }}` templates, functions, limits on depth, size and steps (`docs/expressions.md`).
- Triggers: manual, webhook (HMAC-signed, rate-limited, body-size-limited, idempotency keys), cron schedules.
- Node types: `transform`, `http_request` (SSRF guard), `log`, `email`, plus the inline control nodes.
- Replay: full replay and replay from a chosen node, as a new linked execution (ADR 0007).
- Secrets: AES-256-GCM at rest, referenced from configs, redacted from stored node output and events (ADR 0010).
- Real-time: per-execution and workspace WebSockets with resumable event ids, bounded fan-out, and DB tailing so delivery does not depend on Redis (ADR 0006).
- Observability: Prometheus metrics on API and worker, structured JSON logs, OTLP traces that continue from API into worker via a persisted `traceparent`.
- Deployment: Docker images, a compose stack (api, 3 workers, scheduler, web, Postgres, Redis, Prometheus, Jaeger), Kubernetes manifests, CI workflow, seed and chaos scripts.

## 4. Important algorithms

- **Claiming.** `SELECT … FOR UPDATE SKIP LOCKED` on ready tasks, ordered by priority and `run_at`, writing a fresh lease token and expiry in the same statement. `NOTIFY` wakes idle workers so polling is only a fallback.
- **Fencing.** Every state-changing worker call (`begin`, `heartbeat`, `complete`) carries the lease token; a stale token gets `ErrLeaseLost` and its result is dropped.
- **Advance under the execution lock.** Completing a task locks the execution row, records the result, asks the pure engine for newly ready nodes, enqueues them and writes events in one transaction. Two sibling completions serialise, so exactly one decides the execution is finished.
- **Reaping.** The scheduler requeues tasks whose lease expired or whose worker stopped heartbeating, incrementing `delivery_count`; past `max_deliveries` the node fails instead of looping.
- **Inline nodes.** Conditions, merges, delays, foreach and sub-workflow starts are evaluated by the engine inside the same transaction, so they never touch the queue.
- **Event fan-out.** Events are inserted in the writing transaction, then offered to an in-process hub with bounded per-client queues (overflow ⇒ `resync` message and close). Redis relays events between processes; a DB tail (per execution, and one shared tail for the workspace stream with dedupe by event id) makes the stream complete even when pushes are lost.
- **Replay from node N.** Copies recorded outputs of every node not downstream of N as seeded successes, schedules N, links `replay_of`.

## 5. Distributed-systems guarantees

- **Delivery is at-least-once with fenced completion:** a task can be executed more than once (worker dies after a side effect), but only one completion is ever accepted per attempt and a node's result is recorded once.
- **Execution state changes are atomic** with their events and any successor tasks (single transaction under the row lock).
- **No lost executions on API or worker crash:** work is in PostgreSQL before the API answers 202 (or before a webhook is acknowledged).
- **Delays survive restarts** (stored as `run_at`, woken by whichever scheduler is running).
- **Idempotency:** run and webhook requests accept an idempotency key (`Idempotency-Key`, plus `X-Synapse-Delivery` / `X-Github-Delivery` on webhooks); a repeat returns the original execution.
- **Ordering:** events have a monotonic id per table; clients resume with `?after=`. There is no ordering guarantee across unrelated executions.
- Not provided: exactly-once side effects, multi-region operation, PostgreSQL high availability.

## 6. Failure semantics

| Failure | Behaviour | Tested by |
| --- | --- | --- |
| Worker killed (SIGKILL) mid-task | lease expires, task redelivered to another worker, completes once | `TestKilledWorkerTaskIsRedeliveredAndCompletesOnce` (real processes) |
| API killed mid-run | executions continue on workers, none lost | `TestAPIKilledMidRunLosesNoExecutions` |
| Graceful worker or API shutdown | in-flight task finishes, leases released | `TestWorkerGracefulShutdownFinishesInFlightTask`, `TestAPIGracefulShutdown` |
| PostgreSQL restart | pools reconnect, work resumes | `TestPostgresRestartIsSurvived` |
| Late completion from a presumed-dead worker | rejected by fencing token | `internal/runtime` tests |
| Retries exhausted / delivery cap reached | node fails, execution fails with the recorded error | `internal/runtime`, `internal/worker` tests |
| Redis unavailable | live push degrades to DB tail (about 2 s); no data loss | `internal/api` WS tests, `tests/load/ws.sh` |
| Slow WebSocket client | told to `resync` and disconnected; never blocks the hub | `internal/realtime` tests |
| Queue over `SYNAPSE_MAX_QUEUE_DEPTH` | new starts get 503 with `Retry-After` | `internal/api` tests |

## 7. Testing performed

Run on 2026-09-20 at the final commit, PostgreSQL 16 in Docker:

- **Go:** 205 top-level tests across `internal/...` and `tests/...`; `SYNAPSE_REQUIRE_DB=1 go test -race ./internal/... ./tests/...` passes with the race detector, including the integration (process-level failure injection) and end-to-end (webhook → worker → result) packages.
- **Coverage (statement, merged across all Go tests via `-coverpkg=./internal/...`): 79.9% overall.** Per package: api 81.0, auth 80.0, cron 96.0, engine 80.1, expressions 72.8, localrun 94.6, logging 88.0, nodes 86.6, persistence 75.0, ratelimit 100, realtime 87.5, runtime 80.2, scheduler 84.1, secrets 87.7, telemetry 97.3, tracing 64.3, triggers 80.3, worker 88.4, workflow 87.6. `internal/app` and `internal/config` have no tests of their own. Coverage is a coarse signal; the failure tests matter more than the percentage.
- **Web:** 54 vitest tests; `tsc --noEmit` and eslint clean; production build succeeds.
- **Static analysis:** `gofmt`, `go vet`, `staticcheck` clean (four unused-code findings and one style finding were fixed during the audit); `govulncheck` reports no reachable vulnerabilities (a grpc advisory was fixed by upgrading to v1.83.2; one unmaintained `x/crypto/openpgp` advisory applies to a package the code does not import).
- **`npm audit` (production deps):** 2 moderate advisories in `react-router` 6.x (open redirect via backslash in `<Link>`/`useNavigate`, and `deserializeErrors` in SSR hydration). The app has no SSR and passes only static internal paths to `<Link>`; the fix is a breaking major upgrade to v7 that I did not make. Listed under limitations.
- **Docker:** all images build; the compose stack was brought up and verified: UI through nginx, Prometheus scraping 5 targets, Jaeger showing one trace across API and worker, seed script producing runs.
- **Browser smoke test:** headless Chrome against the compose stack: login, dashboard, workflow list, editor with a 7-node graph, executions, execution detail, settings, all with zero console errors and zero HTTP errors. This found that the React Flow stylesheet was never imported (broken node layout and a white minimap); fixed. The script is not in the repo.
- **Chaos demo:** `scripts/chaos-demo.sh` kills workers under load and shows every execution completing.

Not done: no real Kubernetes cluster run; no browser test suite in CI (the smoke test above was manual); no fuzzing; no long soak test.

## 8. Measured benchmark results

Host: Intel i5-7300HQ (4 logical CPUs), 15 GB, linux/amd64, go1.27.1, PostgreSQL 16 in Docker with `fsync=off` for the main runs. The load generator, API, workers and database all share those 4 cores. One run per configuration, no variance estimate. Full tables, methodology and caveats: `docs/benchmarks/README.md`.

| Measurement | Result |
| --- | --- |
| Sustained throughput, chain workflow (3 tasks), 2 workers | 148 executions/s, 443 tasks/s (plateau; 1 worker: 408, 4 workers: 441 tasks/s) |
| Latency below saturation, 100 executions/s offered | end-to-end p50 / p95 / p99 = 17 / 30 / 44 ms |
| Webhook ingress at 100/s | submit p50 / p95 / p99 = 4 / 7 / 11 ms; end-to-end 17 / 28 / 45 ms |
| Overload (300/s offered, ~2× capacity) | all 6000 completed, 0 failed; p50 latency 22.8 s (queue growth) |
| Durability cost (`fsync=on`) | 195 tasks/s vs 443 (about 2.3× slower) |
| WebSocket push latency, Redis on | p50 2.9–5.1 ms, p99 5.8–17.4 ms; 100 subscribers × 900 events = 90000 deliveries, none missing |
| WebSocket, Redis off | execution stream median 2003 ms (the 2 s DB tail interval); workspace stream complete (900/900), worker-committed events median about 1 s |
| Expression eval / parse | 547 ns / 4.6 µs per op |
| Claim + begin + complete, one task (serial, real Postgres) | 4.1 ms |

I did not profile the throughput plateau, so I do not claim which resource limits it. Horizontal scaling across hosts is not demonstrated.

## 9. Known limitations

- At-least-once delivery: side effects can repeat after a worker crash; users need idempotency keys on external calls.
- One PostgreSQL bounds throughput; there is no sharding and no HA setup provided.
- Redis-off WebSocket latency is bounded by the 2 s poll. The workspace stream has no replay; clients refetch after reconnect.
- The `dbCollector` metrics run `GROUP BY` queries on every scrape; cost on a very large `executions` table was not measured.
- The logging redactor hook exists but is not wired into the binaries. Secret values are redacted from stored node output and events, and code does not log node data, but a future log line could leak one.
- The API does not serve the web build; nginx does.
- Kubernetes manifests are unapplied to a live cluster and only checked for well-formedness.
- Compose health checks for the API are minimal.
- `react-router` 6.x advisories (see section 7).
- No SSO, API-key scopes or audit log.
- The `email` node uses a mock mailer; there is no SMTP delivery.

## 10. Future improvements

- Load test across multiple hosts and profile the 440 tasks/s plateau (batching claims, fewer statements per completion, connection pool tuning).
- Table partitioning and retention for `execution_events` and finished executions.
- Wire the log redactor; add an audit log.
- Upgrade to react-router 7; add browser tests (Playwright) to CI.
- Apply and test the Kubernetes manifests on a real cluster, add a PodDisruptionBudget and HPA on queue depth.
- Per-node-type concurrency limits are supported in the worker; expose them in the UI.

## 11. Exact local startup instructions

Everything in containers (UI at http://localhost:8088, API :8080, Prometheus :9090, Jaeger :16686):

```sh
make docker-up
scripts/seed-demo.sh http://localhost:8088   # demo user, 3 example workflows, a few runs
# log in as demo@synapse.local / synapse-demo-password
make docker-down
```

Local development (Postgres :55432, Redis :56379, web :5174):

```sh
make web-install    # first time only
make dev            # infra + API (with scheduler) + 2 workers + vite dev server
```

## 12. Exact test commands

```sh
make dev-infra                                              # Postgres + Redis for integration tests
make test                                                   # unit tests (DB tests skip if Postgres is down)
SYNAPSE_REQUIRE_DB=1 go test -count=1 ./internal/... ./tests/...   # everything, DB required
make race                                                   # same suite with the race detector
make integration                                            # process-level failure injection
make e2e                                                    # webhook → worker → result
make lint                                                   # gofmt, vet, staticcheck, tsc, eslint
cd apps/web && npm test                                     # frontend tests
go run golang.org/x/vuln/cmd/govulncheck@latest ./...       # vulnerability scan
```

## 13. Exact benchmark commands

```sh
make dev-infra
make benchmark                    # Go micro-benchmarks
make load-test                    # tests/load/suite.sh: writes docs/benchmarks/results/*.json (including WebSocket runs)
python3 tests/load/table.py       # regenerate the results table
tests/load/run.sh chain 2 16 3000 -rate 100     # one scenario: name, workers, capacity, executions, optional rate
scripts/chaos-demo.sh             # worker-kill recovery
```

## What to understand deeply before discussing this project

1. **Why a Postgres queue is defensible here.** `SKIP LOCKED` gives contention-free claims; the same transaction that finishes a task also advances the execution and writes events, so there is no dual-write between a broker and a database. The price is the measured commit-rate ceiling, and `fsync` matters (2.3×). Know ADR 0001 and be ready to say when you would switch to a broker.
2. **Leases and fencing tokens.** A lease alone does not stop a slow worker from finishing late; the token check at `complete` does. Be able to walk through a paused worker that resumes after redelivery.
3. **At-least-once versus effectively-once.** Exactly-once is not claimed. What you do have: fenced completion, idempotency keys at the edges, and honest documentation of repeated side effects.
4. **Advance under the execution row lock.** This is what makes concurrent branch completion, merge and cancellation correct. Know what would break without it (two workers both deciding the join is ready, or both finishing the execution).
5. **The pure engine core.** Because the scheduling decision is a function of graph plus node states, it is tested exhaustively in memory, and the durable runtime is a thin transactional shell around it.
6. **Events as the source of truth for real-time.** Push paths (hub, Redis) are optimisations over a log that clients can replay by id. The benchmark table shows this directly: with Redis off nothing is lost, only latency changes. The workspace-stream gap found by the first benchmark run (600/900 events) and its fix is a good story about measuring before claiming.
7. **Replay as new linked execution.** History is never rewritten; seeded outputs are reused so a replay from node N does not re-run upstream work.
8. **What the benchmarks do and do not say.** They show a plateau on one shared 4-core laptop and a latency profile below it. They do not show multi-host scaling or the cause of the plateau.
