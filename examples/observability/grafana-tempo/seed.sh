#!/usr/bin/env bash
# seed.sh — drive the running stack with synthetic traffic so the
# dashboards populate with realistic shapes. Run AFTER `docker compose
# up -d` succeeds. Idempotent; safe to re-run for more data.
#
# Per RFC observability-profiles.md Decision 5 — sample data lives
# as a reproducible seed script, not a binary trace tarball.

set -euo pipefail

TOKEN="${LOOMCYCLE_AUTH_TOKEN:-demo-token-change-me}"
BASE="${LOOMCYCLE_URL:-http://localhost:8787}"
RUNS="${SEED_RUNS:-30}"
USERS=("alice" "bob" "carol" "dan")
PROMPTS=(
  "Briefly explain what a substrate runtime is."
  "Remember that my favorite colour is blue."
  "List three benefits of OTEL distributed tracing."
  "Summarise the Bauhaus movement in two sentences."
  "What's the capital of Estonia?"
)

echo "→ Verifying stack is reachable at $BASE"
if ! curl -fsS "$BASE/healthz" >/dev/null 2>&1; then
  echo "ERROR: $BASE/healthz unreachable. Run \`docker compose up -d\` first." >&2
  exit 1
fi

echo "→ Firing $RUNS runs across ${#USERS[@]} users / ${#PROMPTS[@]} prompts"
for ((i=1; i<=RUNS; i++)); do
  user="${USERS[$((RANDOM % ${#USERS[@]}))]}"
  prompt="${PROMPTS[$((RANDOM % ${#PROMPTS[@]}))]}"
  agent_id="seed-$i-$(date +%s)"

  curl -fsS -o /dev/null \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -X POST "$BASE/v1/runs" \
    --data-binary "$(cat <<EOF
{
  "agent": "demo",
  "user_id": "$user",
  "agent_id": "$agent_id",
  "user_tier": "default",
  "segments": [
    {"role": "user", "content": [{"type": "trusted-text", "text": "$prompt"}]}
  ]
}
EOF
)" &

  # Stagger to spread out arrivals — let traces accumulate gradually.
  if (( i % 5 == 0 )); then
    wait
    sleep 1
    echo "  $i/$RUNS"
  fi
done

wait
echo
echo "✓ Seed complete. Open http://localhost:3001 → Dashboards → Loomcycle → \"Loomcycle overview\"."
echo "  Traces appear in Tempo within ~30s. Spanmetrics counters need 2 scrape intervals (~30s) to populate."
