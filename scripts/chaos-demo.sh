#!/usr/bin/env bash
# Kills a worker with SIGKILL while it holds leased tasks and shows the work being recovered.
# Needs the dev Postgres (make dev-infra), jq, psql and python3.
set -euo pipefail
cd "$(dirname "$0")/.."
PGADMIN=${PGADMIN:-postgres://synapse:synapse@localhost:55432/postgres?sslmode=disable}
DBURL=${PGADMIN/\/postgres?/\/synapse_chaos?}
API_PORT=18180
ECHO_PORT=18199
API=http://localhost:$API_PORT
N=${N:-8}
BIN=$(mktemp -d)
pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; rm -rf "$BIN"; }
trap cleanup EXIT

go build -o "$BIN/api" ./apps/api
go build -o "$BIN/worker" ./apps/worker
psql "$PGADMIN" -qc "DROP DATABASE IF EXISTS synapse_chaos WITH (FORCE)" -c "CREATE DATABASE synapse_chaos"
q() { psql "$DBURL" -Atc "$1"; }

# a deliberately slow upstream so tasks are in flight when the worker dies
python3 - "$ECHO_PORT" <<'PY' &
import sys, time, http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        time.sleep(5)
        try:
            self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers()
            self.wfile.write(b'{"ok":true}')
        except OSError:
            pass  # the killed worker's connection is gone
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
pids+=($!)

export SYNAPSE_DATABASE_URL=$DBURL SYNAPSE_HTTP_ALLOW_PRIVATE=true SYNAPSE_LOG_LEVEL=warn SYNAPSE_REDIS_URL=""
export SYNAPSE_MASTER_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
export SYNAPSE_LEASE_DURATION=4s SYNAPSE_HEARTBEAT_INTERVAL=1s SYNAPSE_WORKER_DEAD_AFTER=4s SYNAPSE_POLL_INTERVAL=200ms
SYNAPSE_HTTP_ADDR=:$API_PORT "$BIN/api" & pids+=($!)
declare -A wpid
for i in 1 2; do
  SYNAPSE_WORKER_ID=chaos-w$i SYNAPSE_WORKER_CAPACITY=8 SYNAPSE_WORKER_HTTP_ADDR=:$((18190 + i)) "$BIN/worker" & wpid[$i]=$!; pids+=($!)
done
for _ in $(seq 1 50); do curl -sf "$API/health/ready" >/dev/null && break; sleep 0.2; done

post() { curl -fsS -X POST "$API$1" -H 'Content-Type: application/json' ${TOKEN:+-H "Authorization: Bearer $TOKEN"} -d "${2:-{\}}"; }
login=$(post /api/v1/auth/register '{"email":"chaos@synapse.local","password":"chaos-demo-password","display_name":"Chaos"}')
TOKEN=$(jq -r .token <<<"$login"); WS=$(jq -r '.workspaces[0].id' <<<"$login")
graph=$(jq -nc --arg url "http://127.0.0.1:$ECHO_PORT/slow" '{nodes:[
  {id:"start",type:"manual_trigger",name:"Start",config:{}},
  {id:"slow",type:"http_request",name:"Slow call",config:{method:"GET",url:$url}},
  {id:"done",type:"transform",name:"Done",config:{output:{ok:"{{ nodes.slow.body.ok }}"}}}],
  edges:[{id:"e1",source:"start",target:"slow"},{id:"e2",source:"slow",target:"done"}]}')
wf=$(post "/api/v1/workspaces/$WS/workflows" "$(jq -nc --argjson g "$graph" '{name:"chaos",description:"slow upstream",graph:$g}')" | jq -r .id)
post "/api/v1/workspaces/$WS/workflows/$wf/publish" >/dev/null

echo "== starting $N executions across 2 workers"
for _ in $(seq 1 "$N"); do post "/api/v1/workspaces/$WS/workflows/$wf/run" >/dev/null; done
for _ in $(seq 1 50); do
  [ "$(q "SELECT count(*) FROM tasks WHERE status='leased' AND node_type='http_request'")" -ge "$N" ] && break; sleep 0.2
done
echo "== leased tasks per worker:"; q "SELECT leased_by, count(*) FROM tasks WHERE status='leased' GROUP BY 1 ORDER BY 1"

echo "== kill -9 chaos-w1 (pid ${wpid[1]})"
kill -9 "${wpid[1]}"

echo "== waiting for lease expiry, reaper and re-delivery"
for _ in $(seq 1 120); do
  left=$(q "SELECT count(*) FROM executions WHERE status NOT IN ('succeeded','failed','cancelled')")
  [ "$left" = 0 ] && break; sleep 0.5
done
echo "== execution statuses:"; q "SELECT status, count(*) FROM executions GROUP BY 1"
echo "== tasks that were delivered more than once (recovered from the dead worker):"
q "SELECT node_id, attempt, delivery_count, status, leased_by FROM tasks WHERE delivery_count > 1 ORDER BY created_at"
[ "$(q "SELECT count(*) FROM executions WHERE status='succeeded'")" = "$N" ] && echo "OK: all $N executions succeeded" || { echo "FAIL: not every execution succeeded"; exit 1; }
