# ADR 0004: Immutable workflow versions; executions are pinned

Status: accepted

## Context
Editing a workflow must not change what a running or historical execution means. Replay and debugging need the exact graph that ran.

## Decision
Publishing writes an immutable row in `workflow_versions` (graph as JSONB plus a content hash) and points `workflows.published_version_id` at it. Publishing an unchanged graph returns the existing version. An execution stores `version_id` at creation and only ever reads that graph. Drafts are edited in place on the workflow row and never run.

## Consequences
- Old executions stay explainable and replayable after any number of edits.
- Rollback is republishing an earlier version's graph.
- Storage grows with the number of distinct published versions, which is small next to execution data.
