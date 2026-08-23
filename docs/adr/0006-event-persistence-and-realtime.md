# ADR 0006: Execution events are persisted; real-time is a cache on top

Status: accepted

## Context
The UI shows live progress, and a browser can disconnect and return. Anything that is only delivered over pub/sub is lost to a client that was away or slow.

## Decision
Every state change appends a row to `execution_events` (monotonic `id` per database) in the same transaction as the change. That table is the source of truth. After commit the runtime hands the events to a hub that fans out to WebSocket clients through bounded per-subscriber queues. A subscriber that falls behind is sent `resync` and closed (1013); it reconnects with `?after=<last_event_id>` and the server replays from the table. The WebSocket handler also tails the table every two seconds, so a dropped hub message or a missing Redis never loses an event. Redis (ADR 0008) only carries events between processes to cut latency.

## Consequences
- Clients converge to the same state whether or not real-time delivery worked.
- Publishing never blocks on a slow client.
- The events table grows with activity and needs a retention policy in production.
