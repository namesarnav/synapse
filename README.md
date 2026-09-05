# Synapse

A distributed workflow automation engine with a visual DAG editor. Workflows are versioned graphs
of nodes (webhooks, schedules, HTTP calls, branching, delays, loops, sub-workflows) that run on a
pool of stateless workers. PostgreSQL is the only authoritative store: it holds workflow versions,
the task queue, leases, execution history and secrets. Redis is used for ephemeral WebSocket
fan-out only, and everything works (slightly slower to push, never wrong) without it.

- **Go** backend: API, worker and scheduler binaries; no workflow framework, no `eval`.
- **React + Vite + TypeScript** UI: drag-and-drop editor, live execution view, node-level debugger, replay.
- **Durable execution**: leases with fencing tokens, retries with backoff, durable delays,
  deadlines, cancellation, idempotency keys, foreach / sub-workflow children, crash recovery.
- **Custom expression language** (`{{ nodes.fetch.body.items[0].id }}`), parsed and type-checked at
  publish time. See [docs/expressions.md](docs/expressions.md).
- **Auth and secrets**: argon2id passwords, opaque sessions, workspace roles, AES-256-GCM secrets,
  HMAC-signed webhooks, SSRF-guarded HTTP node.
- **Observability**: Prometheus metrics, OpenTelemetry traces (API and worker spans share one
  trace), structured logs carrying `trace_id` / `span_id`.

## Quick start

Everything in containers (UI on http://localhost:8088, API on :8080, Prometheus :9090, Jaeger :16686):

```sh
make docker-up
scripts/seed-demo.sh            # demo user, three example workflows, a few runs
# log in as demo@synapse.local / synapse-demo-password
make docker-down
```

Local development (Postgres on :55432, Redis on :56379, web dev server on :5174):

```sh
make dev            # infra + api (with scheduler) + 2 workers + vite
make web-install    # first time only
```

## Layout

| path | contents |
| --- | --- |
| `apps/api`, `apps/worker`, `apps/scheduler` | the three binaries |
| `apps/web` | React UI (`packages/protocol` holds the shared event/type definitions) |
| `internal/engine` | pure DAG scheduling core, no I/O |
| `internal/runtime` | durable execution: queue, leases, retries, events, replay |
| `internal/expressions` | lexer, parser, checker and evaluator for the expression language |
| `internal/nodes`, `internal/worker` | node executors and the worker loop |
| `internal/api`, `internal/auth`, `internal/realtime` | HTTP API, sessions and roles, WebSocket hub |
| `internal/triggers`, `internal/scheduler`, `internal/secrets`, `internal/telemetry`, `internal/tracing` | triggers and cron, timers and reaping, secrets, metrics, traces |
| `migrations/` | SQL migrations, applied on API boot under an advisory lock |
| `tests/` | integration, failure-injection, end-to-end and load tests |
| `deployments/` | Dockerfiles, nginx and Prometheus config, Kubernetes manifests |
| `docs/` | architecture, ADRs, benchmarks, expression reference |

## Example workflows

`examples/` holds three importable workflows: an order webhook with a condition, a delay and an
HTTP path; a foreach batch; and a cron schedule. `scripts/seed-demo.sh` publishes them and triggers runs.

## Testing

```sh
make dev-infra       # throwaway Postgres + Redis
make test            # unit tests (DB tests skip when Postgres is down)
make integration     # failure injection: killed workers, lost leases, dropped notifications
make e2e             # webhook -> workers -> events through the public API, incl. an API restart
make race            # entire Go suite under the race detector
make web-test        # vitest
make chaos           # kill -9 a worker mid-task and watch its work get re-delivered
```

`SYNAPSE_REQUIRE_DB=1` turns "skip when there is no database" into a failure; CI sets it.

## Performance

Measured on a 4-core laptop with the load generator, API, workers and PostgreSQL all on the same
host (so absolute figures are a lower bound); full method, raw JSON and caveats are in
[docs/benchmarks/README.md](docs/benchmarks/README.md). Reproduce with `make load-test`.

## Design

[docs/architecture/architecture.md](docs/architecture/architecture.md) describes the components and
the life of an execution. Decisions and their trade-offs are recorded as ADRs in
[docs/adr](docs/adr): Postgres as the durable queue, at-least-once delivery with fencing, leases
and the reaper, immutable workflow versions, the expression language, event persistence, replay,
Redis as ephemeral infrastructure, the pure engine core and secret encryption.

## Configuration

All settings are environment variables (see `internal/config/config.go`). The important ones:

| variable | meaning |
| --- | --- |
| `SYNAPSE_DATABASE_URL` | PostgreSQL connection string (required) |
| `SYNAPSE_MASTER_KEY` | 32 bytes, hex or base64; encrypts secrets (required in prod) |
| `SYNAPSE_REDIS_URL` | optional; enables low-latency WebSocket fan-out |
| `SYNAPSE_RUN_SCHEDULER` | API runs timers and reaping in-process (default true; false when the scheduler runs separately) |
| `SYNAPSE_WORKER_CAPACITY` | concurrent tasks per worker |
| `SYNAPSE_LEASE_DURATION`, `SYNAPSE_HEARTBEAT_INTERVAL`, `SYNAPSE_MAX_DELIVERIES` | lease and redelivery tuning |
| `SYNAPSE_HTTP_ALLOW_PRIVATE` | let the HTTP node reach private addresses (off by default) |
| `SYNAPSE_METRICS_TOKEN` | bearer token required on `/metrics` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | enables tracing, e.g. `http://jaeger:4318` |

## Deploying

Docker Compose: [docker-compose.yml](docker-compose.yml) (it falls back to a well-known demo master key; set `SYNAPSE_MASTER_KEY` for anything real). Kubernetes: [deployments/kubernetes](deployments/kubernetes)
(kustomize; API, scheduler, workers with an HPA, web, Postgres for evaluation, Ingress).

The full write-up (guarantees, failure semantics, measured results, limitations) is in [docs/ENGINEERING_REPORT.md](docs/ENGINEERING_REPORT.md).

## Known limitations

- At-least-once delivery: a node whose worker dies after its side effect but before recording the
  result runs again. Give side-effecting HTTP calls an idempotency key.
- The workspace WebSocket has no replay (clients re-fetch the list after a reconnect), and without Redis worker-driven events reach it through a 2 s database poll.
- Throughput on one Postgres is bounded by its commit rate; see the benchmarks for the measured plateau.
- The Kubernetes manifests are checked for well-formedness but have not been applied to a live cluster.
