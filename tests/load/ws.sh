#!/usr/bin/env bash
# Measures event-to-WebSocket latency (see tests/load/wslat) with and without Redis.
# usage: tests/load/ws.sh    Environment: PGADMIN, REDIS (default dev redis), OUT.
set -euo pipefail
cd "$(dirname "$0")/../.."
PGADMIN=${PGADMIN:-postgres://synapse:synapse@localhost:55432/postgres?sslmode=disable}
REDIS=${REDIS:-redis://localhost:56379/0}
DBURL=${PGADMIN/\/postgres?/\/synapse_wslat?}
OUT=${OUT:-docs/benchmarks/results}
BIN=$(mktemp -d)
mkdir -p "$OUT"
go build -o "$BIN/api" ./apps/api
go build -o "$BIN/worker" ./apps/worker
go build -o "$BIN/wslat" ./tests/load/wslat

pids=()
stack_down() { kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; pids=(); }
trap 'stack_down; rm -rf "$BIN"' EXIT

# stack <redis-url or empty>: fresh database, api (+scheduler) and 2 workers
stack() {
  stack_down
  psql "$PGADMIN" -qc "DROP DATABASE IF EXISTS synapse_wslat WITH (FORCE)" -c "CREATE DATABASE synapse_wslat"
  export SYNAPSE_DATABASE_URL=$DBURL SYNAPSE_LOG_LEVEL=error SYNAPSE_REDIS_URL=$1
  export SYNAPSE_MASTER_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  SYNAPSE_HTTP_ADDR=:18080 "$BIN/api" & pids+=($!)
  for i in 1 2; do SYNAPSE_WORKER_ID=ws-w$i SYNAPSE_WORKER_HTTP_ADDR=:$((18100 + i)) "$BIN/worker" & pids+=($!); done
  for _ in $(seq 1 50); do curl -sf localhost:18080/health/ready >/dev/null && break; sleep 0.2; done
}
label() { echo "$1; 2 workers, api+scheduler in one process, postgres $(psql "$PGADMIN" -Atc 'SHOW server_version') (fsync=$(psql "$PGADMIN" -Atc 'SHOW fsync')), all on one host"; }

stack "$REDIS"
"$BIN/wslat" -db "$DBURL" -mode workspace -subscribers 1   -n 300 -rate 20 -label "$(label 'redis on')" -out "$OUT/ws-workspace-redis-s1.json" >/dev/null
"$BIN/wslat" -db "$DBURL" -mode workspace -subscribers 100 -n 300 -rate 20 -label "$(label 'redis on')" -out "$OUT/ws-workspace-redis-s100.json" >/dev/null
"$BIN/wslat" -db "$DBURL" -mode execution -n 200 -rate 20 -label "$(label 'redis on')" -out "$OUT/ws-execution-redis.json" >/dev/null
stack ""
"$BIN/wslat" -db "$DBURL" -mode workspace -subscribers 1 -n 300 -rate 20 -label "$(label 'redis off')" -out "$OUT/ws-workspace-noredis-s1.json" >/dev/null
"$BIN/wslat" -db "$DBURL" -mode execution -n 200 -rate 20 -label "$(label 'redis off')" -out "$OUT/ws-execution-noredis.json" >/dev/null
echo "results in $OUT/ws-*.json"
