#!/usr/bin/env bash
# Runs the benchmark suite recorded in docs/benchmarks. Every number in that
# document comes from the JSON files this script writes.
set -euo pipefail
cd "$(dirname "$0")/../.."
run() { tests/load/run.sh "$@"; }

# Worker scaling on a closed burst.
for w in 1 2 4; do SUFFIX=-burst run chain "$w" 16 3000 >/dev/null; done
# Other shapes.
SUFFIX=-burst run fanout 2 16 1500 >/dev/null
SUFFIX=-burst run http 2 16 2000 >/dev/null
# Webhook ingress (POST /hooks/{id}) instead of the authenticated run endpoint.
SUFFIX=-burst run webhook 2 16 3000 >/dev/null
SUFFIX=-rate100 run webhook 2 16 3000 -rate 100 >/dev/null
# Steady open-loop arrival below saturation: latency under load.
SUFFIX=-rate100 run chain 2 16 3000 -rate 100 >/dev/null
SUFFIX=-rate300 run chain 2 16 6000 -rate 300 >/dev/null

# Same burst against a Postgres with default durability (fsync on).
docker rm -f synapse-pg-durable >/dev/null 2>&1 || true
docker run -d --name synapse-pg-durable -e POSTGRES_USER=synapse -e POSTGRES_PASSWORD=synapse -p 55433:5432 postgres:16-alpine \
  postgres -c max_connections=300 >/dev/null
trap 'docker rm -f synapse-pg-durable >/dev/null 2>&1 || true' EXIT
for _ in $(seq 1 60); do docker exec synapse-pg-durable pg_isready -U synapse >/dev/null 2>&1 && break; sleep 1; done
PGADMIN='postgres://synapse:synapse@localhost:55433/postgres?sslmode=disable' SUFFIX=-burst-durable run chain 2 16 3000 >/dev/null
tests/load/ws.sh # WebSocket event latency, with and without Redis
echo "results in docs/benchmarks/results"
