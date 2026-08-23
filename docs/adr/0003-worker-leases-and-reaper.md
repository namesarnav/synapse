# ADR 0003: Worker leases, heartbeats and the reaper

Status: accepted

## Context
Workers die without notice (OOM kill, node loss, network partition). The queue must notice and hand their work to someone else without a coordinator holding state in memory.

## Decision
- A claim sets `lease_expires_at`. Workers heartbeat all their leases on an interval well below the lease duration; the heartbeat also refreshes `workers.last_seen_at`.
- The scheduler's reaper requeues tasks whose lease expired or whose worker was marked dead, and fails tasks that exhausted `max_deliveries`. It also fails tasks that outlive their node timeout plus a grace period, which catches a node that hangs while its worker still heartbeats.
- Every reaper and scheduler operation is idempotent and uses row locks. Any number of scheduler instances can run concurrently; they need no leader election.
- Workers drain on SIGTERM: they stop claiming, let in-flight tasks finish within `SYNAPSE_SHUTDOWN_TIMEOUT`, then release what is left without counting a delivery.

## Consequences
- Recovery time after a crash is bounded by the lease duration plus the reaper interval.
- A partitioned worker keeps running its node; the fencing token (ADR 0002) makes its late result harmless.
- Short leases recover faster but cost more heartbeat traffic.
