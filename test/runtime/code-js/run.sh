#!/usr/bin/env bash
# test/runtime/code-js/run.sh — synthetic code-js provider (RFC J) runtime
# suite. Deterministic by construction: code-js runs operator JS via goja, no
# LLM, no provider key, no Postgres. Validates that a `provider: code-js`
# agent drives the REAL loop + Appendix-B replay engine end-to-end:
#
#   - the Memory multi-op meta-tool dispatches over the wire (set/incr/get/
#     list) — incr + list were UNREACHABLE from code-js before the meta-tool
#     op-passthrough fix, so this is their end-to-end regression
#   - replay reaches the same state across the loop's tool-call turns (the
#     value read by get() is the value written by set() on an earlier turn)
#   - state persists across run boundaries (run 2 sees run 1's counter)
#   - the synthetic provider emits a provider.code_hash (asserted via the
#     boot log / run; the unit test pins the span, here we assert the run
#     completes cleanly through the real provider registration)
#
# Same test/runtime/<feature>/ convention as memory-core/.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-codejs.XXXXXX)"
PORT=18941
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
BASE="http://127.0.0.1:$PORT"
DB="$TEST_DIR/data/loomcycle.db"
USER_ID="codejs-user"
fail() { echo "FAIL ✗ — $1"; exit 1; }

# Extract concatenated EventText deltas from an SSE capture.
sse_text() {
  grep -A1 '^event: text$' "$1" | grep '^data:' | sed 's/^data: //' | \
    python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" 2>/dev/null || true
}
# stop_reason from the EventDone frame.
sse_stop() {
  grep -A1 '^event: done$' "$1" | grep '^data:' | sed 's/^data: //' | \
    python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('stop_reason',''))
  except: pass" 2>/dev/null | tail -1 || true
}
post_run() {
  local out="$1" prompt="$2" agent="${3:-kvflow}"
  curl -fsS -N -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -H "Accept: text/event-stream" -d @- "$BASE/v1/runs" <<EOF > "$out"
{"agent":"$agent","user_id":"$USER_ID","segments":[{"role":"user","content":[{"type":"trusted-text","text":"$prompt"}]}]}
EOF
}

echo "[1/6] build + boot (code-js enabled, code root = $SCRIPT_DIR/agent_code)"
go build -o bin/loomcycle ./cmd/loomcycle
LOOMCYCLE_MOCK_ENABLED=1 \
LOOMCYCLE_CODE_AGENTS_ENABLED=1 \
LOOMCYCLE_CODE_AGENTS_ROOT="$SCRIPT_DIR/agent_code" \
LOOMCYCLE_DATA_DIR="$TEST_DIR/data" \
LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$PORT" \
LOOMCYCLE_AUTH_TOKEN="$TOKEN" \
  ./bin/loomcycle --config "$SCRIPT_DIR/loomcycle.yaml" > "$TEST_DIR/boot.log" 2>&1 &
PID=$!
READY=0
for i in $(seq 1 50); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && { READY=1; break; }
  kill -0 "$PID" 2>/dev/null || { echo "boot failed:"; cat "$TEST_DIR/boot.log"; exit 1; }
  sleep 0.2
done
[[ "$READY" == "1" ]] || { echo "not ready:"; cat "$TEST_DIR/boot.log"; exit 1; }
grep -qE "code-js|code_agents|code agents" "$TEST_DIR/boot.log" && echo "      code-js provider registered"

echo "[2/6] run 1 — write phase (set purple, incr run_count→1, get, list)"
post_run "$TEST_DIR/run1.sse" "go"
R1=$(sse_text "$TEST_DIR/run1.sse"); S1=$(sse_stop "$TEST_DIR/run1.sse")
echo "      final_text: $R1"
echo "      stop_reason: $S1"
[[ "$S1" == "end_turn" ]] || fail "run 1 stop_reason=$S1, want end_turn"
echo "$R1" | grep -q "color=purple" || fail "run 1 did not read back the value it set (replay broken?): $R1"
echo "$R1" | grep -q "count=1"      || fail "run 1 incr did not return 1 (incr unreachable from code-js?): $R1"
echo "$R1" | grep -q "user_keys=1"  || fail "run 1 list did not see 1 user key (list unreachable from code-js?): $R1"

echo "[3/6] run 2 — persistence phase (counter must advance to 2)"
post_run "$TEST_DIR/run2.sse" "go"
R2=$(sse_text "$TEST_DIR/run2.sse"); S2=$(sse_stop "$TEST_DIR/run2.sse")
echo "      final_text: $R2"
[[ "$S2" == "end_turn" ]] || fail "run 2 stop_reason=$S2, want end_turn"
echo "$R2" | grep -q "count=2" || fail "run 2 counter did not persist+advance to 2: $R2"

echo "[4/6] storage inspection"
if command -v sqlite3 >/dev/null 2>&1 && [[ -f "$DB" ]]; then
  sqlite3 "$DB" "SELECT scope, scope_id, key, value FROM memory ORDER BY scope, key;" | sed 's/^/        /'
  CNT=$(sqlite3 "$DB" "SELECT value FROM memory WHERE scope='agent' AND key='run_count';" 2>/dev/null || echo "")
  [[ "$CNT" == "2" ]] || fail "stored run_count=$CNT, want 2"
else
  echo "      (sqlite3 not available or DB missing — skipping row inspection)"
fi

echo "[5/6] run 3 — a code-js parent reads its child's outcome as an object"
post_run "$TEST_DIR/run3.sse" "go" spawnflow
R3=$(sse_text "$TEST_DIR/run3.sse"); S3=$(sse_stop "$TEST_DIR/run3.sse")
echo "      final_text: $R3"
[[ "$S3" == "end_turn" ]] || fail "run 3 stop_reason=$S3, want end_turn"
# The mock reports no output tokens for a generic agent, so that count is
# checked as a number that is there; the input count is checked as positive.
for want in "type=object" "status=completed" "has_ids=1" "model=mock-generic" "provider=mock" \
            "out_tokens_type=number" "in_tokens_positive=1" "answer_has_header=0" "answer_nonempty=1" "plain_has_header=1"; do
  echo "$R3" | grep -q "$want" || fail "run 3: want '$want' in what the parent read from its child: $R3"
done

echo "[6/6] no errors on the SSE stream"
if grep -q '^event: error$' "$TEST_DIR/run1.sse" "$TEST_DIR/run2.sse" "$TEST_DIR/run3.sse"; then
  fail "an EventError appeared on a code-js run stream"
fi

echo "PASS ✓ — code-js drove the real loop+replay; Memory set/incr/get/list all reachable and persistent; a spawn's object result carries the child's status, answer and usage"
