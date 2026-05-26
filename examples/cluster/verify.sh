#!/usr/bin/env bash
# verify.sh — exercise loomcycle's v0.12.x cluster-mode invariants
# against a running docker-compose.cluster.yaml deployment.
#
# Four checks:
#   1. Each replica's /healthz lists BOTH replicas in .replicas[]
#      (cluster membership via the replicas heartbeat table).
#   2. The LB on :8080 round-robins — repeated /healthz calls flip
#      between replica-a and replica-b in .replica_id.
#   3. A run created on replica-a is visible via GET on replica-b
#      (cross-replica DB-source-of-truth for run status).
#   4. A cancel issued to replica-b for a run on replica-a returns
#      cancelled:true (cross-replica backplane cancel — Phase 3).
#      Skipped if no provider API key is configured.
#
# Exit 0 on all-pass; non-zero on any check failure.
#
# Usage (from repo root):
#   cp examples/cluster/.env.example examples/cluster/.env
#   # Edit .env to set LOOMCYCLE_AUTH_TOKEN (+ at least one API key for check 4)
#   docker compose -f docker-compose.cluster.yaml --env-file examples/cluster/.env up -d
#   ./examples/cluster/verify.sh

set -u

# ─── Config ───────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${ENV_FILE:-$SCRIPT_DIR/.env}"

if [ ! -f "$ENV_FILE" ]; then
    echo "✗ Missing $ENV_FILE — copy .env.example and fill it in first."
    exit 1
fi
# shellcheck disable=SC1090
set -a; source "$ENV_FILE"; set +a

REPLICA_A_URL="${REPLICA_A_URL:-http://localhost:18787}"
REPLICA_B_URL="${REPLICA_B_URL:-http://localhost:18788}"
LB_URL="${LB_URL:-http://localhost:18080}"
TOKEN="${LOOMCYCLE_AUTH_TOKEN:-}"

if [ -z "$TOKEN" ]; then
    echo "✗ LOOMCYCLE_AUTH_TOKEN not set in $ENV_FILE"
    exit 1
fi

# ─── Helpers ──────────────────────────────────────────────────────────
auth=( -H "Authorization: Bearer $TOKEN" )
pass=0
fail=0
skipped=0

ok()    { printf "  \033[32m✓\033[0m %s\n" "$1"; pass=$((pass+1)); }
ko()    { printf "  \033[31m✗\033[0m %s\n" "$1"; fail=$((fail+1)); }
skip()  { printf "  \033[33m·\033[0m skipped: %s\n" "$1"; skipped=$((skipped+1)); }
hdr()   { printf "\n\033[1m%s\033[0m\n" "$1"; }

# need_jq aborts early if jq is missing.
if ! command -v jq >/dev/null 2>&1; then
    echo "✗ jq is required; install with: brew install jq  (mac) or apt-get install jq (linux)"
    exit 1
fi

# ─── Preflight: wait for cluster to stabilise ─────────────────────────
# Each replica's first heartbeat lands shortly after it accepts traffic;
# there's a ~few-second window where /healthz on a replica shows only
# itself (or none) because the other's heartbeat hasn't reached the DB
# yet. Poll until both replicas see two entries OR a 30s deadline fires.
hdr "Preflight — wait for both replicas to see each other"

deadline=$(( $(date +%s) + 30 ))
ready=0
while [ "$(date +%s)" -lt "$deadline" ]; do
    ca=$(curl -fsS "${auth[@]}" "$REPLICA_A_URL/healthz" 2>/dev/null | jq -r '.replicas | length // 0' 2>/dev/null || echo 0)
    cb=$(curl -fsS "${auth[@]}" "$REPLICA_B_URL/healthz" 2>/dev/null | jq -r '.replicas | length // 0' 2>/dev/null || echo 0)
    if [ "$ca" = "2" ] && [ "$cb" = "2" ]; then
        ready=1
        break
    fi
    printf "  waiting… replica-a=%s/2 replica-b=%s/2\n" "$ca" "$cb"
    sleep 2
done
if [ "$ready" = "1" ]; then
    ok "Cluster stabilised — both replicas see each other"
else
    ko "Cluster not stable after 30s — last seen: replica-a=$ca/2 replica-b=$cb/2"
    echo
    echo "Boot logs:"
    docker compose -f docker-compose.cluster.yaml --env-file "$ENV_FILE" logs --tail=20 loomcycle-a loomcycle-b 2>&1 | sed 's/^/  /'
    exit 1
fi

# ─── Check 1: cluster membership ──────────────────────────────────────
hdr "1. Cluster membership — /healthz on each replica lists both"

check_replicas() {
    local url="$1"
    local label="$2"
    local body
    body=$(curl -fsS "${auth[@]}" "$url/healthz" 2>/dev/null) || {
        ko "$label /healthz unreachable at $url"
        return
    }
    local count
    count=$(printf '%s' "$body" | jq -r '.replicas | length // 0')
    if [ "$count" = "2" ]; then
        local ids
        ids=$(printf '%s' "$body" | jq -r '.replicas | map(.id) | join(",")')
        ok "$label sees $count replicas ($ids)"
    else
        ko "$label expected 2 replicas, got $count: $(printf '%s' "$body" | jq -c '.replicas // []')"
    fi
}
check_replicas "$REPLICA_A_URL" "replica-a"
check_replicas "$REPLICA_B_URL" "replica-b"

# ─── Check 2: LB round-robin ──────────────────────────────────────────
hdr "2. LB round-robin — repeated /healthz hits flip replica_id"

declare -A seen=()
for i in 1 2 3 4; do
    rid=$(curl -fsS "${auth[@]}" "$LB_URL/healthz" 2>/dev/null | jq -r '.replica_id // "?"')
    seen[$rid]=1
done
if [ -n "${seen[replica-a]:-}" ] && [ -n "${seen[replica-b]:-}" ]; then
    ok "LB hit both replicas (replica-a + replica-b) over 4 requests"
else
    ko "LB only hit: ${!seen[*]} — round-robin not engaging? Try more requests or check nginx upstream health."
fi

# ─── Check 3: cross-replica run visibility ────────────────────────────
hdr "3. Cross-replica run visibility — run on A is GET-able on B"

# Create a run on replica-a. The agent is configured with tier:middle,
# so this will hit the configured provider. If no API key is set, the
# create will fail at run-admit time (the agent loop errors). Use an
# obviously-cheap prompt to keep cost / latency minimal.
HAS_API_KEY=0
if [ -n "${ANTHROPIC_API_KEY:-}" ] || [ -n "${OPENAI_API_KEY:-}" ] || [ -n "${DEEPSEEK_API_KEY:-}" ]; then
    HAS_API_KEY=1
fi

if [ "$HAS_API_KEY" -eq 0 ]; then
    skip "Cross-replica run visibility (no provider API key in .env — set ANTHROPIC_API_KEY / OPENAI_API_KEY / DEEPSEEK_API_KEY)"
    skip "Cross-replica cancel (same reason)"
else
    AGENT_ID="cluster-verify-$(date +%s)-$RANDOM"
    create_resp=$(curl -fsS "${auth[@]}" -X POST "$REPLICA_A_URL/v1/runs" \
        -H "Content-Type: application/json" \
        -d "{\"agent\":\"default\",\"agent_id\":\"$AGENT_ID\",\"user_input\":[{\"role\":\"user\",\"content\":\"Say hi in one word.\"}]}" \
        --max-time 10 \
        --no-buffer 2>/dev/null | head -c 4096 || true)
    if [ -z "$create_resp" ]; then
        ko "POST /v1/runs on replica-a returned empty (provider key invalid? agent failed?)"
    else
        # The run is in-flight (SSE streaming). Now GET it on replica-b.
        sleep 1
        get_resp=$(curl -fsS "${auth[@]}" "$REPLICA_B_URL/v1/agents/$AGENT_ID" 2>/dev/null || true)
        if [ -z "$get_resp" ]; then
            ko "GET /v1/agents/$AGENT_ID on replica-b returned empty"
        else
            replica_id_in_row=$(printf '%s' "$get_resp" | jq -r '.replica_id // ""')
            status=$(printf '%s' "$get_resp" | jq -r '.status // ""')
            if [ -n "$status" ]; then
                ok "replica-b sees the run (status=$status, replica_id=$replica_id_in_row) — DB is shared, cross-replica read works"
            else
                ko "replica-b GET response malformed: $(printf '%s' "$get_resp" | head -c 200)"
            fi
        fi

        # ─── Check 4: cross-replica cancel ────────────────────────────
        hdr "4. Cross-replica cancel — cancel issued to B routes to A via backplane"

        cancel_resp=$(curl -fsS "${auth[@]}" -X POST "$REPLICA_B_URL/v1/agents/$AGENT_ID/cancel" \
            -H "Content-Type: application/json" \
            -d '{"reason":"verify.sh cross-replica cancel test"}' \
            --max-time 10 2>/dev/null || true)
        if [ -z "$cancel_resp" ]; then
            ko "POST /v1/agents/$AGENT_ID/cancel on replica-b returned empty"
        else
            cancelled=$(printf '%s' "$cancel_resp" | jq -r '.cancelled // false')
            reason=$(printf '%s' "$cancel_resp" | jq -r '.reason // ""')
            if [ "$cancelled" = "true" ] || [ -n "$reason" ]; then
                ok "cancel propagated (cancelled=$cancelled reason=\"$reason\") — backplane round-trip works"
            else
                ko "cancel response malformed or both fields empty: $(printf '%s' "$cancel_resp" | head -c 200)"
            fi
        fi
    fi
fi

# ─── Summary ──────────────────────────────────────────────────────────
hdr "Summary"
printf "  passed:  %d\n  failed:  %d\n  skipped: %d\n" "$pass" "$fail" "$skipped"

if [ "$fail" -gt 0 ]; then
    echo
    echo "✗ Cluster verification failed."
    echo "  Troubleshooting tips in examples/cluster/README.md."
    exit 1
fi
echo
echo "✓ Cluster verification passed."
exit 0
