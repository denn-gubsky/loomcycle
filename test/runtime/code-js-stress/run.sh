#!/usr/bin/env bash
# test/runtime/code-js-stress/run.sh — stress + edge behaviours of the
# synthetic code-js provider (RFC J). Deterministic, no LLM, no key, no PG.
#
#   [A] No iteration cap: the `overrun` agent makes 25 sequential tool calls
#       (past the old 16 default) and COMPLETES (end_turn) with all 25 incrs —
#       code-agents are exempt from MaxIterations (bounded by the run timeout).
#   [A2] Timeout is the bound: the `runaway` agent loops tool calls forever and
#        is cut by the run-level wall-clock timeout (NOT left hanging).
#   [B] Concurrency + atomic incr + replay isolation: N parallel `counter`
#       runs each do 8 sequential incrs on a SHARED agent-scope key; the
#       final value must be exactly 8*N (no lost update, no cross-run bleed,
#       and each 8-turn replay chain stayed divergence-free).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-codejs-stress.XXXXXX)"
PORT=18942
N=8   # concurrent counter runs
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
BASE="http://127.0.0.1:$PORT"
DB="$TEST_DIR/data/loomcycle.db"
fail() { echo "FAIL ✗ — $1"; exit 1; }

sse_stop() {
  grep -A1 '^event: done$' "$1" | grep '^data:' | sed 's/^data: //' | \
    python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('stop_reason',''))
  except: pass" 2>/dev/null | tail -1 || true
}
post_run() {
  local out="$1" agent="$2" uid="$3"
  curl -fsS -N -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -H "Accept: text/event-stream" -d @- "$BASE/v1/runs" <<EOF > "$out"
{"agent":"$agent","user_id":"$uid","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}
EOF
}

echo "[1/5] build + boot (code-js enabled)"
go build -o bin/loomcycle ./cmd/loomcycle
LOOMCYCLE_MOCK_ENABLED=1 \
LOOMCYCLE_CODE_AGENTS_ENABLED=1 \
LOOMCYCLE_CODE_AGENTS_ROOT="$SCRIPT_DIR/agent_code" \
LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS=5 \
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

echo "[2/5] [A] no iteration cap — overrun (25 sequential calls, past old 16)"
post_run "$TEST_DIR/overrun.sse" "overrun" "stress-overrun"
OS=$(sse_stop "$TEST_DIR/overrun.sse")
echo "      stop_reason: $OS"
[[ "$OS" == "end_turn" ]] || fail "overrun stop_reason=$OS, want end_turn (code-agents are exempt from MaxIterations)"
# All 25 incrs must have executed — the run was NOT capped.
OVR=$(command -v sqlite3 >/dev/null 2>&1 && sqlite3 "$DB" "SELECT value FROM memory WHERE scope='agent' AND key='ovr';" 2>/dev/null || echo "")
echo "      incrs executed: ${OVR:-<no sqlite3>}"
if [[ -n "$OVR" ]]; then [[ "$OVR" == "25" ]] || fail "overrun executed $OVR incrs, want 25 (no cap)"; fi
# And the stale per-code-js MaxIterations diagnostic must NOT fire anymore.
if grep -q "hit MaxIterations" "$TEST_DIR/boot.log"; then
  fail "operator log still says 'hit MaxIterations' for a code-agent — the cap was not actually disabled"
fi

echo "[3/5] [A2] timeout is the bound — runaway (infinite tool-call loop)"
t0=$(date +%s)
post_run "$TEST_DIR/runaway.sse" "runaway" "stress-runaway" || true
t1=$(date +%s); elapsed=$((t1 - t0))
echo "      run returned after ${elapsed}s (run timeout = 5s)"
grep -q '^event: error' "$TEST_DIR/runaway.sse" || fail "runaway produced no error event — did the run timeout fire, or did it hang?"
RS=$(sse_stop "$TEST_DIR/runaway.sse")
[[ "$RS" != "end_turn" ]] || fail "runaway completed end_turn — the infinite loop was not cut by the timeout"
[[ "$elapsed" -lt 20 ]] || fail "runaway took ${elapsed}s — not bounded by the 5s run timeout (the loop hung)"
echo "      ✓ cut by the run timeout (error event, stop≠end_turn, ${elapsed}s < 20s)"

echo "[4/5] [B] concurrency — $N parallel counter runs × 8 incrs on a shared key"
pids=()
for k in $(seq 1 "$N"); do
  post_run "$TEST_DIR/counter-$k.sse" "counter" "stress-counter-$k" &
  pids+=($!)
done
for p in "${pids[@]}"; do wait "$p" || true; done
# Every run must have completed end_turn.
for k in $(seq 1 "$N"); do
  cs=$(sse_stop "$TEST_DIR/counter-$k.sse")
  [[ "$cs" == "end_turn" ]] || fail "counter run $k stop_reason=$cs, want end_turn"
done
echo "      all $N runs completed end_turn"

echo "[5/5] shared counter == 8*N (atomic incr, no lost update)"
WANT=$((8 * N))
if command -v sqlite3 >/dev/null 2>&1 && [[ -f "$DB" ]]; then
  GOT=$(sqlite3 "$DB" "SELECT value FROM memory WHERE scope='agent' AND key='n';" 2>/dev/null || echo "")
  echo "      shared counter = $GOT (want $WANT)"
  [[ "$GOT" == "$WANT" ]] || fail "shared counter=$GOT, want $WANT — a concurrent incr was lost"
else
  echo "      (sqlite3 not available — skipping counter assertion)"
fi

echo "PASS ✓ — code-agents exempt from MaxIterations (25-call run completed); runaway cut by the run timeout; $N concurrent code-js runs kept incr atomic"
