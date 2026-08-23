# ADR 0009: A pure engine core shared by the runtime and the local runner

Status: accepted

## Context
Scheduling decisions (which nodes are ready, which are skipped, when an execution is done) are the most subtle part of the system. They should be testable without a database or clock.

## Decision
`internal/engine` is pure: given a graph index and node statuses it returns the next decision. Inline nodes (triggers, condition, merge, delay, foreach, sub-workflow, stop) are evaluated by `engine.EvalInline` inside the execution transaction; worker nodes (transform, http_request, log, email) are queued. `internal/localrun` drives the same core in memory for the CLI and tests, and the durable runtime in `internal/runtime` wraps it with persistence. Both are tested against the same scenarios.

## Consequences
- Branching, joins, skip propagation and error routing are covered by fast unit tests.
- The durable runtime only adds persistence, locking and delivery.
