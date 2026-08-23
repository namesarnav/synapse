# ADR 0001: PostgreSQL is the durable task queue

Status: accepted

## Context
Synapse must not lose work when a process dies. Workflow state, task state and the queue must change atomically: finishing a node and enqueuing its successors is one logical step. A separate broker (Redis lists, RabbitMQ, Kafka) would make that a distributed transaction or force an outbox.

## Decision
Tasks live in a `tasks` table in the same database as executions. Workers claim with `SELECT ... FOR UPDATE SKIP LOCKED` ordered by priority and `run_at`. Completing a task, advancing the execution and inserting successor tasks happen in one transaction under the execution row lock. `LISTEN/NOTIFY` on `synapse_tasks` wakes idle workers; polling remains as the fallback, so a lost notification only costs latency.

## Consequences
- Enqueue and state change are atomic; there is no dual-write and no outbox.
- Throughput is bounded by Postgres, not by a broker. Measured numbers are in `docs/benchmarks`.
- Queue depth, oldest ready task and dead tasks are plain SQL and are exported as metrics.
- Delayed retries and delays are `run_at` in the future, so they survive restarts for free.

## Alternatives considered
- Redis as the queue: fast, but loses the atomicity above and needs its own persistence story.
- Kafka or RabbitMQ: more moving parts than the problem needs at this scale, same dual-write problem.
