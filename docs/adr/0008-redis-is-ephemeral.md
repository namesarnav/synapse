# ADR 0008: Redis is used only for ephemeral pub/sub

Status: accepted

## Context
Redis is attractive for queues and state, but anything stored only in Redis is lost on a restart unless persistence is configured and trusted.

## Decision
Redis carries one thing: a pub/sub channel (`synapse:events`) that forwards committed execution events between processes so API replicas can push them to their WebSocket clients quickly. It holds no queue, no locks and no state. If Redis is absent or down, the system runs unchanged; clients see events from the database tail within the tail interval (ADR 0006). Redis is therefore not part of the readiness check.

## Consequences
- Losing Redis costs latency, never correctness.
- Operating Synapse needs one durable dependency (Postgres) plus an optional cache.
