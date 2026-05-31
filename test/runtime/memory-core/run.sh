#!/usr/bin/env bash
# test/runtime/memory-core/run.sh — built-in Memory runtime suite, the
# NON-vector half (deterministic, sqlite, no embedder, no Postgres).
#
# Drives the live wire:
#   1. admin K/V CRUD — PUT a user-scope entry, GET it back, list keys,
#      DELETE it, GET → 404 (the durable K/V promise over HTTP)
#   2. agent-scope round-trip via the same admin surface
#   3. MemoryBackendDef substrate CRUD over /v1/_memorybackenddef, including
#      the trust-boundary validations hardened in the memory review:
#        - shared_key_with_prefix with an EMPTY prefix_pattern is REFUSED
#          (the cross-tenant-leak fix) — and a {tenant_id} one is accepted
#        - a mem9 base_url that is not http(s) is REFUSED (the SSRF fix)
#
# The vector/dedup half lives in test/runtime/memory-vector/ (needs PG +
# the stub embedder). Run: ./test/runtime/memory-core/run.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-memory-core.XXXXXX)"
PORT=18934
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
BASE="http://127.0.0.1:$PORT"
adm() { curl -fsS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" "$@"; }
# code-only request (for negative assertions)
code() { curl -s -o "$TEST_DIR/last.body" -w "%{http_code}" -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" "$@"; }
fail() { echo "FAIL ✗ — $1"; exit 1; }

echo "[1/6] build + boot"
go build -o bin/loomcycle ./cmd/loomcycle
LOOMCYCLE_MOCK_ENABLED=1 \
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

echo "[2/6] admin K/V: PUT user/alice/fav → GET"
adm -X PUT "$BASE/v1/_memory/scopes/user/alice/keys/fav" -d '{"value":{"color":"blue"}}' > "$TEST_DIR/put.json"
GOT=$(adm "$BASE/v1/_memory/scopes/user/alice/keys/fav")
echo "$GOT" > "$TEST_DIR/get.json"
echo "$GOT" | grep -q '"blue"' || fail "GET did not return stored value: $GOT"

echo "[3/6] list keys shows it; then DELETE → GET 404"
adm "$BASE/v1/_memory/scopes/user/alice/keys" | grep -q "fav" || fail "key not listed"
adm -X DELETE "$BASE/v1/_memory/scopes/user/alice/keys/fav" > /dev/null
C=$(code "$BASE/v1/_memory/scopes/user/alice/keys/fav")
[[ "$C" = "404" ]] || fail "GET after DELETE code=$C (want 404)"

echo "[4/6] agent-scope round-trip"
adm -X PUT "$BASE/v1/_memory/scopes/agent/keeper/keys/counter" -d '{"value":{"n":1}}' > /dev/null
adm "$BASE/v1/_memory/scopes/agent/keeper/keys/counter" | grep -q '"n":1' || fail "agent-scope readback failed"

echo "[5/6] MemoryBackendDef: inprocess create + tenancy-empty-prefix REFUSED"
adm -X POST "$BASE/v1/_memorybackenddef" -d '{"op":"create","name":"local","overlay":{"kind":"inprocess"}}' | grep -q '"def_id"' \
  || fail "inprocess MemoryBackendDef create failed"
adm "$BASE/v1/_memorybackenddef/names" | grep -q "local" || fail "backend name not listed"
# HIGH fix: shared_key_with_prefix with empty prefix_pattern must be refused.
C=$(code -X POST "$BASE/v1/_memorybackenddef" -d '{"op":"create","name":"leaky","overlay":{"kind":"mem9","config":{"base_url":"https://m.example.com","api_key_env":"LOOMCYCLE_M_KEY"},"tenancy_strategy":{"kind":"shared_key_with_prefix"}}}')
grep -q "tenant_id" "$TEST_DIR/last.body" || fail "empty-prefix shared_key_with_prefix was NOT refused (code=$C body=$(cat "$TEST_DIR/last.body"))"
# A valid {tenant_id} prefix is accepted.
adm -X POST "$BASE/v1/_memorybackenddef" -d '{"op":"create","name":"shared","overlay":{"kind":"mem9","config":{"base_url":"https://m.example.com","api_key_env":"LOOMCYCLE_M_KEY"},"tenancy_strategy":{"kind":"shared_key_with_prefix","prefix_pattern":"t-{tenant_id}::"}}}' | grep -q '"def_id"' \
  || fail "valid {tenant_id} prefix was wrongly refused"

echo "[6/6] MemoryBackendDef: mem9 non-http base_url REFUSED (SSRF fix)"
# The non-http(s) scheme is the rejection trigger; the path is irrelevant,
# kept generic so a secret scanner's generic-password detector doesn't
# false-positive on a system-file-path literal.
code -X POST "$BASE/v1/_memorybackenddef" -d '{"op":"create","name":"badurl","overlay":{"kind":"mem9","config":{"base_url":"file:///tmp/x","api_key_env":"LOOMCYCLE_M_KEY"}}}' > /dev/null
grep -qiE "http|url|base_url" "$TEST_DIR/last.body" || fail "file:// base_url was NOT refused: $(cat "$TEST_DIR/last.body")"
# Make sure it really was an error result, not an accepted def.
grep -q '"def_id"' "$TEST_DIR/last.body" && fail "file:// base_url was accepted (got a def_id)"

echo "PASS ✓ — memory K/V CRUD + MemoryBackendDef CRUD with tenancy + SSRF rejections"
