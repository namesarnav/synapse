#!/usr/bin/env bash
# Creates a demo user and publishes the workflows in examples/ against a running stack,
# then triggers each one so the UI has history to show.
# usage: scripts/seed-demo.sh [api-url]    (default http://localhost:8080; the UI proxy at :8088 works too)
set -euo pipefail
cd "$(dirname "$0")/.."
API=${1:-${SYNAPSE_API:-http://localhost:8080}}
EMAIL=${DEMO_EMAIL:-demo@synapse.local}
PASSWORD=${DEMO_PASSWORD:-synapse-demo-password}
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

post() { curl -fsS -X POST "$API$1" -H 'Content-Type: application/json' ${TOKEN:+-H "Authorization: Bearer $TOKEN"} -d "${2:-{\}}"; }

login=$(post /api/v1/auth/login "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" 2>/dev/null ||
  post /api/v1/auth/register "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\",\"display_name\":\"Demo\"}")
TOKEN=$(jq -r .token <<<"$login")
WS=$(jq -r '.workspaces[0].id' <<<"$login")

publish() {
  local wf id
  wf=$(post "/api/v1/workspaces/$WS/workflows" "$(jq -c '{name, description, graph}' "$1")")
  id=$(jq -r .id <<<"$wf")
  post "/api/v1/workspaces/$WS/workflows/$id/publish" >/dev/null
  echo "$id"
}

order=$(publish examples/order-webhook.json)
batch=$(publish examples/batch-foreach.json)
cron=$(publish examples/every-minute.json)

hook=$(curl -fsS "$API/api/v1/workspaces/$WS/workflows/$order/webhooks" -H "Authorization: Bearer $TOKEN" | jq -r '.webhooks[0].id')
curl -fsS -X POST "$API/hooks/$hook" -H 'Content-Type: application/json' -d '{"customer":"Ada","amount":60,"qty":3}' >/dev/null
curl -fsS -X POST "$API/hooks/$hook" -H 'Content-Type: application/json' -d '{"customer":"Grace","amount":5,"qty":2}' >/dev/null
post "/api/v1/workspaces/$WS/workflows/$batch/run" >/dev/null

cat <<MSG
Seeded 3 workflows and 3 executions.
  login:    $EMAIL / $PASSWORD
  webhook:  POST $API/hooks/$hook
  UI:       ${UI_URL:-http://localhost:8088}
MSG
