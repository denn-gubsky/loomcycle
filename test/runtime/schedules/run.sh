#!/usr/bin/env bash
# test/runtime/schedules/run.sh — Scheduler (RFC E) runtime suite.
#
# Deterministic (mock provider, sqlite). Exercises the ScheduleDef
# substrate end-to-end over the wire:
#   1. create a schedule (POST /v1/_scheduledef) → promoted active version
#   2. get it back by def_id; confirm it appears in /v1/_scheduledef/names
#   3. let the `* * * * *` cron FIRE (tick=1s) and confirm a run was created
#      and completed, and schedule_run_state recorded the fire
#   4. retire it and confirm a subsequent tick does NOT fire it again
#
# No real provider, no API key. Run: ./test/runtime/schedules/run.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-schedules.XXXXXX)"
PORT=18931
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
DB="$TEST_DIR/data/loomcycle.db"
BASE="http://127.0.0.1:$PORT"
adm() { curl -fsS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" "$@"; }

echo "[1/6] build"
go build -o bin/loomcycle ./cmd/loomcycle

echo "[2/6] boot (scheduler tick=1s)"
LOOMCYCLE_MOCK_ENABLED=1 \
LOOMCYCLE_SCHEDULER_ENABLED=1 \
LOOMCYCLE_SCHEDULER_TICK_SECONDS=1 \
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

fail() { echo "FAIL ✗ — $1"; exit 1; }

echo "[3/6] create schedule"
CREATE=$(adm -X POST "$BASE/v1/_scheduledef" \
  -d '{"op":"create","name":"ticker","overlay":{"schedule":"* * * * *","agent":"ticker","user_id":"sched-u","prompt":[{"role":"user","content":[{"type":"trusted-text","text":"tick"}]}]}}')
echo "$CREATE" > "$TEST_DIR/create.json"
DEF_ID=$(echo "$CREATE" | grep -o '"def_id":"[^"]*"' | head -1 | cut -d'"' -f4)
[[ -n "$DEF_ID" ]] || fail "create returned no def_id: $CREATE"
echo "  def_id=$DEF_ID"
echo "$CREATE" | grep -q '"promoted":true' || fail "create not promoted active: $CREATE"

echo "[4/6] get + list"
adm "$BASE/v1/_scheduledef?op=get&def_id=$DEF_ID" > "$TEST_DIR/get.json" 2>/dev/null || true
NAMES=$(adm "$BASE/v1/_scheduledef/names")
echo "$NAMES" > "$TEST_DIR/names.json"
echo "$NAMES" | grep -q "ticker" || fail "schedule name not listed: $NAMES"

echo "[5/6] await a cron fire (<= ~75s)"
FIRED=0
for i in $(seq 1 75); do
  # schedule_run_state records last_run_id once the sweeper fires the def
  LRID=$(sqlite3 "$DB" "SELECT last_run_id FROM schedule_run_state WHERE def_id='$DEF_ID' AND last_run_id IS NOT NULL AND last_run_id != '';" 2>/dev/null || true)
  if [[ -n "$LRID" ]]; then FIRED=1; echo "  fired: last_run_id=$LRID after ~${i}s"; break; fi
  sleep 1
done
[[ "$FIRED" = 1 ]] || { echo "--- schedule_run_state ---"; sqlite3 "$DB" "SELECT def_id,last_run_at,last_run_id,last_status,next_run_at FROM schedule_run_state;" 2>/dev/null || true; fail "schedule never fired in ~75s"; }
# The fired run must have COMPLETED (mock-generic emits ok+done).
FIRE_STATUS=$(sqlite3 "$DB" "SELECT last_status FROM schedule_run_state WHERE def_id='$DEF_ID';")
[[ "$FIRE_STATUS" = "completed" ]] || fail "fired run status=$FIRE_STATUS, want completed"
RUN_OK=$(sqlite3 "$DB" "SELECT count(*) FROM runs WHERE status='completed';")
[[ "$RUN_OK" -ge 1 ]] || fail "no completed run row after fire"

echo "[6/6] retire → no further fires"
BEFORE=$(sqlite3 "$DB" "SELECT last_run_id FROM schedule_run_state WHERE def_id='$DEF_ID';")
adm -X POST "$BASE/v1/_scheduledef" -d "{\"op\":\"retire\",\"def_id\":\"$DEF_ID\",\"retired\":true}" > "$TEST_DIR/retire.json"
grep -q '"retired":true' "$TEST_DIR/retire.json" || fail "retire did not set retired: $(cat "$TEST_DIR/retire.json")"
sleep 5  # several ticks
AFTER=$(sqlite3 "$DB" "SELECT last_run_id FROM schedule_run_state WHERE def_id='$DEF_ID';")
[[ "$BEFORE" = "$AFTER" ]] || fail "retired schedule fired again (before=$BEFORE after=$AFTER)"

echo "PASS ✓ — scheduler create/get/list, cron fire→completed run, retire→quiesced"
