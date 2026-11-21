#!/usr/bin/env bash
# test/runtime/calc/run.sh — the Calc default end to end through a booted
# server. Deterministic: code-js agents (no LLM, no provider key). Checks that:
#   - an agent whose definition never names Calc calls it, and 0.1 + 0.2 is
#     exactly 0.3;
#   - an agent with tools: [-Calc] has no Calc binding;
#   - a sub-agent spawned through the Agent tool holds Calc;
#   - Context op=agents and `loomcycle agents list` count / show Calc for the
#     default agent and not for the -Calc one;
#   - a run paused mid-run, whose runtime is then killed, is resumed by the
#     next boot and calls Calc again on the toolset the resume rebuilds.
# The server runs under `env -i` with a scratch HOME and an explicit --config,
# so no operator config, preset or key leaks in. The binary is built into the
# test dir and removed on exit.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-calc.XXXXXX)"
BIN="$TEST_DIR/loomcycle"
PORT=18961
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  [[ -n "${RUNPID:-}" ]] && { kill "$RUNPID" 2>/dev/null || true; }
  rm -f "$BIN"
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
BASE="http://127.0.0.1:$PORT"
mkdir -p "$TEST_DIR/home" "$TEST_DIR/data"
fail() { echo "FAIL ✗ — $1"; exit 1; }

sse_text() {
  grep -A1 '^event: text$' "$1" | grep '^data:' | sed 's/^data: //' | \
    python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" 2>/dev/null || true
}
sse_run_id() {
  grep -A1 '^event: agent$' "$1" | grep '^data:' | head -1 | sed 's/^data: //' | \
    python3 -c "import sys,json; print(json.loads(sys.stdin.read()).get('run_id',''))" 2>/dev/null || true
}
post_run() {
  curl -fsS -N -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -H "Accept: text/event-stream" -d @- "$BASE/v1/runs" <<JSON > "$1"
{"agent":"$2","user_id":"calc-user","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}
JSON
}
admin() { curl -fsS -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d "$2" "$BASE$1"; }
# ISOLATED is the environment the binary runs with: nothing from the
# operator's. `env` execs the binary, so a backgrounded `env …` has the
# server's own pid in $! (a backgrounded shell function would not, and
# killing its subshell would orphan the server on the port).
ISOLATED=(env -i PATH="/usr/bin:/bin:/usr/sbin:/sbin" HOME="$TEST_DIR/home"
  LOOMCYCLE_MOCK_ENABLED=1
  LOOMCYCLE_CODE_AGENTS_ROOT="$SCRIPT_DIR/agent_code"
  LOOMCYCLE_DATA_DIR="$TEST_DIR/data"
  LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$PORT"
  LOOMCYCLE_AUTH_TOKEN="$TOKEN")
boot() {
  # A server left on the port would answer the readiness probe in our place.
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && fail "port $PORT already answers; stop that server first"
  "${ISOLATED[@]}" "$BIN" --config "$SCRIPT_DIR/loomcycle.yaml" > "$1" 2>&1 &
  PID=$!
  for _ in $(seq 1 100); do
    curl -fsS "$BASE/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$PID" 2>/dev/null || { echo "boot failed:"; cat "$1"; exit 1; }
    sleep 0.2
  done
  echo "not ready:"; cat "$1"; exit 1
}

echo "[1/7] build + boot (isolated: env -i, HOME=$TEST_DIR/home)"
go build -o "$BIN" ./cmd/loomcycle
boot "$TEST_DIR/boot1.log"

echo "[2/7] calcflow: Calc without naming it; Context op=agents"
post_run "$TEST_DIR/flow.sse" calcflow
R=$(sse_text "$TEST_DIR/flow.sse"); echo "      $R"
# calcflow holds Skill, Context and Calc (the three runtime defaults); nocalc
# holds Skill and Context.
for want in "text=0.3" "exact=true" "calcflow_tools=3" "nocalc_tools=2"; do
  echo "$R" | grep -q -- "$want" || fail "calcflow: want '$want' in: $R"
done

echo "[3/7] nocalc: tools [-Calc] has no Calc"
post_run "$TEST_DIR/nocalc.sse" nocalc
R=$(sse_text "$TEST_DIR/nocalc.sse"); echo "      $R"
echo "$R" | grep -q "calc_binding=undefined" || fail "nocalc holds Calc: $R"
echo "$R" | grep -q "context_binding=function" || fail "nocalc lost Context too: $R"

echo "[4/7] calcparent: a sub-agent holds Calc"
post_run "$TEST_DIR/parent.sse" calcparent
R=$(sse_text "$TEST_DIR/parent.sse"); echo "      $R"
echo "$R" | grep -q "child_status=completed" || fail "the child did not complete: $R"
echo "$R" | grep -q "child_calc=1267650600228229401496703205376" || fail "the child had no Calc or got a wrong 2^100: $R"

echo "[5/7] loomcycle agents list shows the effective tools"
"${ISOLATED[@]}" "$BIN" agents list --config "$SCRIPT_DIR/loomcycle.yaml" > "$TEST_DIR/agents.txt" 2>&1 || { cat "$TEST_DIR/agents.txt"; fail "agents list failed"; }
tools_of() { awk -v a="$1" '$0==a {f=1; next} f && /tools *:/ {sub(/.*tools *: */, ""); print; exit}' "$TEST_DIR/agents.txt"; }
echo "      calcflow: $(tools_of calcflow)   nocalc: $(tools_of nocalc)   calcchild: $(tools_of calcchild)"
tools_of calcflow  | grep -qw Calc || fail "agents list: calcflow does not show Calc"
tools_of calcchild | grep -qw Calc || fail "agents list: calcchild does not show Calc"
tools_of nocalc | tr ',' '\n' | grep -qx Calc && fail "agents list: nocalc shows Calc"

echo "[6/7] calcresume: pause mid-run, kill the runtime, resume on the next boot"
# Its stream breaks when the runtime is killed; that curl error is expected.
post_run "$TEST_DIR/resume1.sse" calcresume 2>/dev/null &
RUNPID=$!
sleep 1
P=$(admin /v1/_pause '{"timeout_ms":15000}')
echo "      pause: $P"
echo "$P" | grep -q '"paused_runs_count":1' || fail "pause did not park the run: $P"
RUN_ID=$(sse_run_id "$TEST_DIR/resume1.sse")
[[ -n "$RUN_ID" ]] || fail "no run_id on the paused run's stream"
# A crash, not a shutdown: the row stays paused for the next boot to find.
kill -9 "$PID"; wait "$PID" 2>/dev/null || true; PID=""
kill "$RUNPID" 2>/dev/null || true; RUNPID=""
boot "$TEST_DIR/boot2.log"
STATUS=""
for _ in $(seq 1 100); do
  STATUS=$(curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/v1/runs/$RUN_ID" | \
    python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || true)
  [[ "$STATUS" == "completed" || "$STATUS" == "failed" || "$STATUS" == "cancelled" ]] && break
  sleep 0.2
done
[[ "$STATUS" == "completed" ]] || { grep -i resume "$TEST_DIR/boot2.log" | head; fail "the resumed run ended $STATUS, want completed"; }
curl -fsS -N --max-time 10 -H "Authorization: Bearer $TOKEN" -H "Accept: text/event-stream" \
  "$BASE/v1/runs/$RUN_ID/stream?from_seq=0" > "$TEST_DIR/resume2.sse" || true
R=$(sse_text "$TEST_DIR/resume2.sse"); echo "      $R"
echo "$R" | grep -q "before=42 after=0.3" || fail "the resumed run did not call Calc again: $R"

echo "[7/7] no errors on the run streams"
if grep -q '^event: error$' "$TEST_DIR"/flow.sse "$TEST_DIR"/nocalc.sse "$TEST_DIR"/parent.sse "$TEST_DIR"/resume2.sse; then
  fail "an EventError appeared on a run stream"
fi

echo "PASS ✓ — Calc is on by default (0.1 + 0.2 = 0.3 exact), -Calc removes it, a sub-agent and a resumed run hold it, and the listings show it"
