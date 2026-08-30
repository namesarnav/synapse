#!/usr/bin/env bash
# Runs one load scenario against a freshly built local stack.
# usage: tests/load/run.sh <scenario> <workers> <capacity> <executions> [extra loadgen flags]
# Environment: PGADMIN (admin URL, default dev postgres), OUT (result dir).
set -euo pipefail
cd "$(dirname "$0")/../.."
scenario=${1:?scenario}; workers=${2:-2}; capacity=${3:-16}; n=${4:-2000}; shift $(( $# > 4 ? 4 : $# ))
PGADMIN=${PGADMIN:-postgres://synapse:synapse@localhost:55432/postgres?sslmode=disable}
DBNAME=synapse_load
DBURL=${PGADMIN/\/postgres?/\/$DBNAME?}
OUT=${OUT:-docs/benchmarks/results}
BIN=$(mktemp -d)
mkdir -p "$OUT"

go build -o "$BIN/api" ./apps/api
go build -o "$BIN/worker" ./apps/worker
go build -o "$BIN/loadgen" ./tests/load/loadgen

psql "$PGADMIN" -qc "DROP DATABASE IF EXISTS $DBNAME WITH (FORCE)" -c "CREATE DATABASE $DBNAME"

pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; rm -rf "$BIN"; }
trap cleanup EXIT

export SYNAPSE_WEBHOOK_RATE=1000000 SYNAPSE_WEBHOOK_BURST=1000000
export SYNAPSE_DATABASE_URL=$DBURL SYNAPSE_HTTP_ALLOW_PRIVATE=true SYNAPSE_LOG_LEVEL=error SYNAPSE_REDIS_URL=""
export SYNAPSE_MASTER_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
SYNAPSE_HTTP_ADDR=:18080 "$BIN/api" & pids+=($!)
for i in $(seq 1 "$workers"); do
  SYNAPSE_WORKER_ID=load-w$i SYNAPSE_WORKER_CAPACITY=$capacity SYNAPSE_WORKER_HTTP_ADDR=:$((18100 + i)) "$BIN/worker" & pids+=($!)
done
for _ in $(seq 1 50); do curl -sf localhost:18080/health/ready >/dev/null && break; sleep 0.2; done

pgver=$(psql "$PGADMIN" -Atc "SHOW server_version")
label="$workers worker(s) x capacity $capacity, api+scheduler in one process, postgres $pgver (fsync=$(psql "$PGADMIN" -Atc 'SHOW fsync'), synchronous_commit=$(psql "$PGADMIN" -Atc 'SHOW synchronous_commit')), redis off, all processes on one host"
"$BIN/loadgen" -db "$DBURL" -scenario "$scenario" -n "$n" -label "$label" -out "$OUT/${scenario}-w${workers}c${capacity}${SUFFIX:-}.json" "$@"
