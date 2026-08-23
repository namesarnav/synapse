# Benchmarks

Every number here comes from a run recorded in `docs/benchmarks/results/`. Nothing is estimated or copied from another system.

## Environment

| | |
| --- | --- |
| CPU | Intel Core i5-7300HQ @ 2.50 GHz, 4 logical CPUs |
| Memory | 15 GB |
| OS / arch | linux/amd64 |
| Go | go1.27.1 |
| PostgreSQL | 16.15 in Docker (`docker-compose.dev.yml`: `fsync=off`, `synchronous_commit=off`) |
| Redis | not used in load runs |
| Code | commit `5516e7e` plus the benchmark files added in this change |

Everything runs on this one laptop: the load generator, API (with the scheduler in-process), the workers and PostgreSQL share the same 4 cores. Absolute numbers are therefore a lower bound for a real deployment, and they say more about this host than about the design. Unrelated containers from other projects were also running on the host; their load was not measured.

## Micro-benchmarks

`micro.txt` is the raw `go test -bench` output (`-benchtime 2s`).

| benchmark | what it measures | ns/op | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: |
| `expressions.BenchmarkEval` | evaluate a parsed expression with arithmetic, a call and a ternary | 547 | 128 | 8 |
| `expressions.BenchmarkParse` | parse the same expression | 4577 | 3608 | 31 |
| `engine.BenchmarkResolve` | one scheduling decision over a 200-node graph, half finished | 26886 | 16712 | 209 |
| `realtime.BenchmarkDispatch/subs=1` | fan one event out to 1 subscriber | 187 | 0 | 0 |
| `realtime.BenchmarkDispatch/subs=100` | fan one event out to 100 subscribers | 19094 | 0 | 0 |
| `realtime.BenchmarkDispatch/subs=1000` | fan one event out to 1000 subscribers | 200615 | 0 | 0 |
| `runtime.BenchmarkStartExecution` | create an execution (real PostgreSQL, one connection at a time) | 3138326 | 21441 | 401 |
| `runtime.BenchmarkClaimBeginComplete` | claim, begin and complete one task (real PostgreSQL) | 4121349 | 27125 | 483 |

The two runtime benchmarks are dominated by PostgreSQL round trips (a handful of statements per transaction) and are serial, so they measure per-operation latency, not throughput.

## Load tests

The load generator (`tests/load/loadgen`) registers a user, publishes a workflow, submits executions through the real HTTP API, and waits for them to finish. Timings are read back from the database (`executions.created_at` to `finished_at`), so end-to-end latency does not depend on client polling.

Scenarios:

- `chain`: trigger → transform → transform → transform (3 worker tasks per execution).
- `fanout`: trigger → 5 parallel transforms → merge (5 worker tasks).
- `http`: trigger → `http_request` against an in-process echo server (1 worker task).

Two arrival modes:

- **burst** (closed): submit all executions as fast as 32 concurrent clients can, then wait for the queue to drain. End-to-end latency here includes time waiting in the queue behind everything else submitted, so it is a drain time, not a service time.
- **rate** (open loop): submit at a fixed rate (`-rate`), which is the realistic view of latency at a given load.

All runs used workers with capacity 16.

| run | executions | tasks | submit/s | executions/s | tasks/s | submit p50/p95/p99 (ms) | end-to-end p50/p95/p99 (ms) | failed |
| --- | ---: | ---: | ---: | ---: | ---: | --- | --- | ---: |
| `chain-w1c16-burst` | 3000 | 9000 | 483 | 136 | 408 | 63 / 94 / 134 | 16195 / 16320 / 16327 | 0 |
| `chain-w2c16-burst-durable` | 3000 | 9000 | 208 | 65 | 195 | 147 / 192 / 401 | 33978 / 36380 / 36633 | 0 |
| `chain-w2c16-burst` | 3000 | 9000 | 415 | 148 | 443 | 75 / 104 / 138 | 13901 / 14699 / 14760 | 0 |
| `chain-w2c16-rate100` | 3000 | 9000 | 100 | 100 | 300 | 5 / 8 / 12 | 17 / 30 / 44 | 0 |
| `chain-w2c16-rate300` | 6000 | 18000 | 299 | 148 | 443 | 25 / 52 / 98 | 22823 / 25345 / 25565 | 0 |
| `chain-w4c16-burst` | 3000 | 9000 | 320 | 147 | 441 | 98 / 135 / 180 | 12344 / 13512 / 13611 | 0 |
| `fanout-w2c16-burst` | 1500 | 7500 | 291 | 93 | 466 | 105 / 148 / 243 | 7610 / 10669 / 10951 | 0 |
| `http-w2c16-burst` | 2000 | 2000 | 426 | 306 | 306 | 72 / 103 / 180 | 2235 / 2754 / 2788 | 0 |

`submit/s` is the API acceptance rate, `executions/s` and `tasks/s` are completions over the whole submit-plus-drain window. All runs finished every execution with no failures and no redelivered tasks.

## Reading the results

- **Throughput plateaus at about 440 tasks/s on this host** (about 148 chain executions/s), and adding workers does not move it: 1 worker gave 408 tasks/s, 2 gave 443 and 4 gave 441. One worker with 16 slots already keeps the queue busy. I did not profile the plateau, so I cannot say which resource limits it. It is consistent with the 4 shared cores being saturated by PostgreSQL, the API and the workers together, which fits the serial per-task cost of roughly 4 ms in `BenchmarkClaimBeginComplete`. Horizontal scaling across separate hosts is not demonstrated by these runs.
- **Below saturation latency is low.** At a steady 100 chain executions/s, end-to-end latency was p50 17 ms, p95 30 ms and p99 44 ms (three sequential tasks).
- **Above saturation the queue grows without loss.** At 300/s offered (about twice the sustainable rate) the system still completed all 6000 executions with zero failures, but latency grew to p50 22.8 s, because the queue is unbounded by design up to `SYNAPSE_MAX_QUEUE_DEPTH`. Backpressure returns 503 with `Retry-After` beyond that limit; the load runs did not reach it.
- **Durability costs about 2.3x.** The same 3000-execution burst against a stock PostgreSQL (`fsync=on`, `synchronous_commit=on`) completed 195 tasks/s versus 443 with the dev configuration. Numbers in the other rows should be read as "fast disk semantics"; the durable row is the conservative one.
- `http` is bounded by the executor round trip and is faster per execution because it is one task, not by any special path.

## Limitations

- Single host, shared CPU, one run per configuration. There is no variance estimate; differences of a few percent (for example submit rate between the 1 and 4 worker runs) are within what I would expect from noise on a laptop.
- The load generator competes with the system under test for CPU.
- Redis and WebSocket fan-out were not part of the load runs. Fan-out cost is covered only by `BenchmarkDispatch`.
- The metrics gauges that count rows by status run a `GROUP BY` per scrape; they were not enabled in load runs and their cost on a large `executions` table was not measured.

## Reproducing

```sh
make dev-infra        # PostgreSQL on :55432, Redis on :56379
make benchmark        # micro-benchmarks, raw output on stdout
make load-test        # runs tests/load/suite.sh, writes docs/benchmarks/results/*.json
python3 tests/load/table.py   # regenerate the table above from the JSON files
```

A single scenario: `tests/load/run.sh chain 2 16 3000` (scenario, workers, capacity, executions), with `-rate N` for open-loop arrivals. `PGADMIN=... tests/load/run.sh` targets a different PostgreSQL; the suite's durable run starts a throwaway `postgres:16-alpine` container on port 55433 and removes it afterwards. The result file records hardware, git revision and the exact configuration.
