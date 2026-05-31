#!/usr/bin/env bash
# test/runtime/webhooks/run.sh — Input Webhooks (RFC H) runtime suite.
#
# Deterministic (mock provider, sqlite). Exercises the receiver end-to-end
# over the wire, including the trust boundaries:
#   1. WebhookDef create (POST /v1/_webhookdef) — spawn delivery, hmac auth
#   2. a correctly-signed delivery (GitHub sha256= envelope) → 202 + run_id,
#      and the spawned run completes
#   3. a WRONG signature → 401, no run spawned
#   4. an oversized body (> body_size_limit_bytes) → 400
#   5. a rate-limited burst → 429
#   6. a replay (same delivery id) → deduped, not a second run
#
# No real provider, no API key. Run: ./test/runtime/webhooks/run.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-webhooks.XXXXXX)"
PORT=18932
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
SECRET="whsecret-$(date +%s)"
DB="$TEST_DIR/data/loomcycle.db"
BASE="http://127.0.0.1:$PORT"
adm() { curl -fsS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" "$@"; }
fail() { echo "FAIL ✗ — $1"; exit 1; }
# GitHub-style HMAC-SHA256 over the raw body.
sign() { printf '%s' "$1" | openssl dgst -sha256 -hmac "$SECRET" -hex | sed 's/^.*= //'; }
# Deliver: $1=body $2=extra curl args... ; prints "HTTP_CODE\nbody"
deliver() {
  local body="$1"; shift
  curl -s -o "$TEST_DIR/deliver.body" -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" "$@" \
    --data-binary "$body" "$BASE/v1/_webhooks/gh"
}

echo "[1/8] build"
go build -o bin/loomcycle ./cmd/loomcycle

echo "[2/8] boot (webhooks enabled, secret allowlisted)"
LOOMCYCLE_MOCK_ENABLED=1 \
LOOMCYCLE_WEBHOOKS_ENABLED=1 \
LOOMCYCLE_WH_SECRET="$SECRET" \
LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST="LOOMCYCLE_WH_SECRET" \
LOOMCYCLE_DATA_DIR="$TEST_DIR/data" \
LOOMCYCLE_AGENTS_ROOT="$SCRIPT_DIR/agents" \
LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$PORT" \
LOOMCYCLE_AUTH_TOKEN="$TOKEN" \
  ./bin/loomcycle --config "$SCRIPT_DIR/loomcycle.yaml" > "$TEST_DIR/boot.log" 2>&1 &
PID=$!
for i in $(seq 1 50); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && break
  kill -0 "$PID" 2>/dev/null || { echo "boot failed"; cat "$TEST_DIR/boot.log"; exit 1; }
  sleep 0.2
done

echo "[3/8] create webhookdef (spawn, hmac, GitHub envelope, rate burst=2, body cap 512)"
CREATE=$(adm -X POST "$BASE/v1/_webhookdef" -d '{
  "op":"create","name":"gh",
  "overlay":{
    "enabled":true,"delivery":"spawn","agent":"responder",
    "auth":{"kind":"hmac","header":"X-Hub-Signature-256","signing_secret_env":"LOOMCYCLE_WH_SECRET",
            "delivery_id_header":"X-Delivery-Id"},
    "rate_limit":{"requests_per_minute":60,"burst":2},
    "body_size_limit_bytes":512,
    "payload_mapping":{"goal":"$.goal"}
  }
}')
echo "$CREATE" > "$TEST_DIR/create.json"
echo "$CREATE" | grep -q '"def_id"' || fail "webhookdef create failed: $CREATE"
adm "$BASE/v1/_webhookdef/names" | grep -q "gh" || fail "webhook name not listed"

echo "[4/8] correctly-signed delivery → 202 + run"
BODY='{"goal":"do-the-thing"}'
CODE=$(deliver "$BODY" -H "X-Hub-Signature-256: sha256=$(sign "$BODY")" -H "X-Delivery-Id: d-ok-1")
[[ "$CODE" = "202" ]] || fail "signed delivery code=$CODE (want 202): $(cat "$TEST_DIR/deliver.body")"
grep -q '"run_id"' "$TEST_DIR/deliver.body" || fail "202 missing run_id: $(cat "$TEST_DIR/deliver.body")"
# Await the spawned run to complete.
OK=0; for i in $(seq 1 50); do
  [[ "$(sqlite3 "$DB" "SELECT count(*) FROM runs WHERE status='completed';" 2>/dev/null || echo 0)" -ge 1 ]] && { OK=1; break; }
  sleep 0.2
done
[[ "$OK" = 1 ]] || fail "spawned run did not complete"

echo "[5/8] WRONG signature → 401, no new run"
RUNS_BEFORE=$(sqlite3 "$DB" "SELECT count(*) FROM runs;")
CODE=$(deliver "$BODY" -H "X-Hub-Signature-256: sha256=deadbeef" -H "X-Delivery-Id: d-bad")
[[ "$CODE" = "401" ]] || fail "bad-sig code=$CODE (want 401)"
sleep 1
RUNS_AFTER=$(sqlite3 "$DB" "SELECT count(*) FROM runs;")
[[ "$RUNS_BEFORE" = "$RUNS_AFTER" ]] || fail "bad-sig spawned a run ($RUNS_BEFORE→$RUNS_AFTER)"

echo "[6/8] oversized body (> 512) → 400"
BIG="{\"goal\":\"$(head -c 600 < /dev/zero | tr '\0' 'A')\"}"
CODE=$(deliver "$BIG" -H "X-Hub-Signature-256: sha256=$(sign "$BIG")" -H "X-Delivery-Id: d-big")
[[ "$CODE" = "400" ]] || fail "oversized code=$CODE (want 400)"

echo "[7/8] replay same delivery id (VALID signature) → deduped, no 2nd run"
# Re-send d-ok-1's exact body+signature. Because the signature is VALID
# (identical to the accepted [4] delivery), a non-2xx here can ONLY be the
# replay guard firing — not a signature failure — so this genuinely proves
# dedup, not an accidental rejection.
# NOTE (discovered by this suite): the replay guard returns 401
# "unauthorized" (server.go:195 — deliberately "opaque, same as a sig
# failure"). That is arguably misleading for a valid-but-duplicate delivery
# (a 200/409 idempotent ack is the usual webhook contract, and the replay
# path is only reachable AFTER signature verification, so no unauthorized
# attacker is being protected). Flagged for the maintainer; this suite
# asserts the SHIPPED behaviour so a future change to it is a conscious one.
RUNS_BEFORE=$(sqlite3 "$DB" "SELECT count(*) FROM runs;")
CODE=$(deliver "$BODY" -H "X-Hub-Signature-256: sha256=$(sign "$BODY")" -H "X-Delivery-Id: d-ok-1")
echo "  replay code=$CODE body=$(cat "$TEST_DIR/deliver.body")"
[[ "$CODE" = "401" ]] || fail "replay (valid sig) code=$CODE; expected the replay guard to reject (currently 401)"
sleep 1
RUNS_AFTER=$(sqlite3 "$DB" "SELECT count(*) FROM runs;")
[[ "$RUNS_BEFORE" = "$RUNS_AFTER" ]] || fail "replay spawned a duplicate run ($RUNS_BEFORE→$RUNS_AFTER)"

echo "[8/8] rate-limit burst → at least one 429"
GOT429=0
for n in 1 2 3 4 5; do
  b="{\"goal\":\"rl-$n\"}"
  c=$(deliver "$b" -H "X-Hub-Signature-256: sha256=$(sign "$b")" -H "X-Delivery-Id: d-rl-$n")
  [[ "$c" = "429" ]] && { GOT429=1; break; }
done
[[ "$GOT429" = 1 ]] || fail "rate limiter never returned 429 over a burst of 5 (burst=2)"

echo "PASS ✓ — webhookdef CRUD, signed→run, bad-sig→401, oversized→400, replay-dedup, rate-limit→429"
