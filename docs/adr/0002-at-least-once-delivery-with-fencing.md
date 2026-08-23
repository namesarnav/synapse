# ADR 0002: At-least-once task delivery with fencing tokens

Status: accepted

## Context
A worker can crash after running a node and before recording the result, or stall long enough that its lease expires while it is still running. Exactly-once execution of side effects is impossible in general; the engine has to choose what to guarantee.

## Decision
Delivery is at-least-once. Every claim writes a fresh `lease_token`. `Begin`, `Heartbeat`, `Complete`, `Fail` and `Release` all check the token under the task row lock and drop the result when it does not match (`ErrLeaseLost`). A worker that lost its lease therefore cannot overwrite the outcome recorded by whoever holds the task now. Each task has a `delivery_count` and `max_deliveries`; a task that exhausts them is marked `dead` and its node fails.

Node executors that call the outside world receive a stable idempotency key (`execution:node:attempt`), which `http_request` can forward as a header, so a redelivered attempt can be de-duplicated by the receiver.

A redelivery after a crash keeps the same attempt number. A failure reported by the node creates the next attempt after the retry delay. Retry budgets are therefore not consumed by infrastructure failures.

## Consequences
- A node may run more than once. Executors must be idempotent or forward the key.
- A stale worker's result is discarded rather than merged, which keeps the execution log consistent.
- Redelivery is visible: `delivery_count` is stored and the load tests report redelivered tasks.
