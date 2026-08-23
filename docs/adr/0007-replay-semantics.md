# ADR 0007: Replay creates a new linked execution

Status: accepted

## Context
Users need to re-run a failed or finished execution, sometimes from a specific node, without altering history.

## Decision
Replay never mutates the source. It starts a new execution of the same pinned version with `replay_of` pointing at the source. A full replay reuses the original trigger payload. A replay from node N copies the recorded outputs of every node that is not N or downstream of N into the new execution as already-succeeded ("seeded") nodes, records `replay_source_node`, and schedules N. Only finished executions can be replayed. Replays accept an idempotency key so a double click yields one execution.

## Consequences
- The source stays a faithful record; the lineage is queryable (`GET /executions?replay_of=`).
- Seeded outputs come from the source's stored, already redacted results, so secrets are not resurfaced.
- Side effects of the re-run nodes happen again; the debugger labels replays clearly.
