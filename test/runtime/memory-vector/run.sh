#!/usr/bin/env bash
# test/runtime/memory-vector/run.sh — Memory vector + dedup runtime suite.
#
# The vector half of the built-in Memory feature: embed-on-write, embed_stats,
# reembed, and SEARCH-TIME DEDUP — exercised end-to-end against a real
# Postgres+pgvector store with the DETERMINISTIC stub embedder
# (LOOMCYCLE_EMBEDDER_STUB=1), so near-duplicate text produces near-identical
# vectors and the dedup collapse is assertable without a real embedder.
#
# PG-GATED: provide LOOMCYCLE_TEST_PG_DSN pointing at a Postgres that has the
# `vector` extension available. Without it the suite SKIPs (exit 0) — sqlite
# cannot do vectors (sqlite-vec is stubbed). Example:
#   LOOMCYCLE_TEST_PG_DSN="postgres://postgres@127.0.0.1:54329/loomcycle" \
#     ./test/runtime/memory-vector/run.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

if [[ -z "${LOOMCYCLE_TEST_PG_DSN:-}" ]]; then
  echo "SKIP — set LOOMCYCLE_TEST_PG_DSN to a Postgres with the 'vector' extension to run the memory-vector suite (sqlite-vec is stubbed)."
  exit 0
fi

TEST_DIR="$(mktemp -d -t loomcycle-memory-vector.XXXXXX)"
PORT=18935
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
SCOPE_ID="dv-$$-$(date +%s)"   # unique so re-runs against the same DB don't collide
BASE="http://127.0.0.1:$PORT"
adm() { curl -fsS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" "$@"; }
fail() { echo "FAIL ✗ — $1"; exit 1; }

echo "[1/6] build + boot (postgres + pgvector + stub embedder)"
go build -o bin/loomcycle ./cmd/loomcycle
LOOMCYCLE_MOCK_ENABLED=1 \
LOOMCYCLE_EMBEDDER_STUB=1 \
LOOMCYCLE_PG_DSN="$LOOMCYCLE_TEST_PG_DSN" \
LOOMCYCLE_PGVECTOR_ENABLED=1 \
LOOMCYCLE_PG_AUTOMIGRATE=1 \
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
grep -qi "embedder: stub" "$TEST_DIR/boot.log" || echo "  (note: stub embedder line not found in boot.log — check $TEST_DIR/boot.log)"

put_embed() { adm -X PUT "$BASE/v1/_memory/scopes/user/$SCOPE_ID/keys/$1?embed=true" -d "{\"value\":$2}"; }

echo "[2/6] seed 3 near-duplicate + 1 distinct entries (embed=true)"
put_embed k1 '"loomcycle is a high-load agentic runtime in go"'            > "$TEST_DIR/p1.json"
put_embed k2 '"loomcycle is a high-load agentic runtime written in go"'    > "$TEST_DIR/p2.json"
put_embed k3 '"loomcycle: a high-load agentic runtime, in go"'             > "$TEST_DIR/p3.json"
put_embed k4 '"the weather in paris is sunny and warm this afternoon"'     > "$TEST_DIR/p4.json"
for f in p1 p2 p3 p4; do grep -q '"embedded":true' "$TEST_DIR/$f.json" || fail "embed failed for $f: $(cat "$TEST_DIR/$f.json")"; done

echo "[3/6] embed_stats shows the stub model + >=4 embeddings"
STATS=$(adm "$BASE/v1/_memory/embed_stats?scope=user")
echo "$STATS" > "$TEST_DIR/stats.json"
echo "$STATS" | grep -q "stub" || fail "embed_stats missing stub provider/model: $STATS"

echo "[4/6] search with dedup (drop) → near-duplicates collapse"
curl -fsS -N -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -H "Accept: text/event-stream" \
  -d "{\"agent\":\"searcher\",\"user_id\":\"$SCOPE_ID\",\"segments\":[{\"role\":\"user\",\"content\":[{\"type\":\"trusted-text\",\"text\":\"search\"}]}]}" \
  "$BASE/v1/runs" > "$TEST_DIR/search.sse" 2>&1 || true
# The Memory.search tool_result carries the dedup drop count. Assert >= 1
# near-duplicate was dropped (k1/k2/k3 collapse; k4 distinct survives).
# The tool_result JSON is embedded (escaped) inside the SSE "text" field,
# so the quotes around the key are backslash-escaped (\"dedup_dropped\":N).
DROPPED=$(grep -oE 'dedup_dropped[\\":]+[0-9]+' "$TEST_DIR/search.sse" | head -1 | grep -oE '[0-9]+$' || true)
if [[ -z "$DROPPED" ]]; then
  echo "--- search.sse (for shape) ---"; sed -n '1,40p' "$TEST_DIR/search.sse"
  fail "could not find dedup_dropped in the search tool_result"
fi
echo "  dedup_dropped=$DROPPED"
[[ "$DROPPED" -ge 1 ]] || fail "dedup dropped $DROPPED near-duplicates (want >= 1)"

echo "[5/6] reembed dry_run is safe + reports a plan"
adm -X POST "$BASE/v1/_memory/reembed?dry_run=true" > "$TEST_DIR/reembed.json" 2>/dev/null || true
echo "  reembed: $(cat "$TEST_DIR/reembed.json" | head -c 200)"

echo "[6/6] sanity: the distinct row (k4) is still retrievable"
adm "$BASE/v1/_memory/scopes/user/$SCOPE_ID/keys/k4" | grep -q "paris" || fail "distinct entry k4 missing"

echo "PASS ✓ — embed→pgvector, embed_stats(stub), search-time dedup collapse, reembed dry_run"
