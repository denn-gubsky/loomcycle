#!/usr/bin/env bash
# test/runtime/system-channels/run.sh — v0.8.6 system-channels runtime smoke.
#
# Three exercises in one run:
#
#   [A] Heartbeat cadence — `_system/heartbeat-1s` ticks once/sec.
#       After 3s of uptime, expect 2..4 messages in storage.
#
#   [B] Admin endpoint — curl publish to `_system/alarms/info`.
#       Expect 200 + row with published_by_user_id="_admin".
#
#   [C] Agent deferred publish — scheduler-bot publishes to `findings`
#       with deliver_at=now+2s. Immediate subscribe returns 0 messages;
#       after 2.5s subscribe returns 1 message.
#
# Usage:
#   GEMINI_API_KEY=... ./test/runtime/system-channels/run.sh
# OR:
#   set -a; source .env.local; set +a; ./test/runtime/system-channels/run.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-system-channels-test.XXXXXX)"
trap 'cleanup' EXIT INT TERM

cleanup() {
  if [[ -n "${LOOMCYCLE_PID:-}" ]]; then
    kill "$LOOMCYCLE_PID" 2>/dev/null || true
    wait "$LOOMCYCLE_PID" 2>/dev/null || true
  fi
  echo
  echo "Test dir kept for inspection: $TEST_DIR"
}

export LOOMCYCLE_DATA_DIR="$TEST_DIR/data"
export LOOMCYCLE_AGENTS_ROOT="$SCRIPT_DIR/agents"
export LOOMCYCLE_LISTEN_ADDR="127.0.0.1:18791"
export LOOMCYCLE_AUTH_TOKEN="test-token-$(date +%s)"
if [[ -z "${GEMINI_API_KEY:-}" ]]; then
  echo "ERROR: GEMINI_API_KEY not set. Source .env.local first:" >&2
  echo "  set -a; source .env.local; set +a" >&2
  exit 1
fi
if ! command -v python3 &>/dev/null; then
  echo "ERROR: python3 is required for SSE delta extraction." >&2
  exit 1
fi
if ! command -v sqlite3 &>/dev/null; then
  echo "ERROR: sqlite3 CLI is required for storage inspection." >&2
  exit 1
fi

mkdir -p "$TEST_DIR/data"
BOOT_LOG="$TEST_DIR/boot.log"
USER_ID="test-user-systemch"

# ─── 1. Build fresh binary ──────────────────────────────────────────
echo "[1/7] Building bin/loomcycle..."
go build -o bin/loomcycle ./cmd/loomcycle

# ─── 2. Boot loomcycle in background ────────────────────────────────
echo "[2/7] Booting loomcycle at $LOOMCYCLE_LISTEN_ADDR (config: $SCRIPT_DIR/loomcycle.yaml)..."
./bin/loomcycle --config "$SCRIPT_DIR/loomcycle.yaml" > "$BOOT_LOG" 2>&1 &
LOOMCYCLE_PID=$!

READY=0
for i in $(seq 1 50); do
  if curl -fsS "http://$LOOMCYCLE_LISTEN_ADDR/healthz" > /dev/null 2>&1; then
    echo "      ready after ~$((i * 200))ms"
    READY=1
    break
  fi
  if ! kill -0 "$LOOMCYCLE_PID" 2>/dev/null; then
    echo "ERROR: loomcycle exited during boot. Boot log:" >&2
    cat "$BOOT_LOG" >&2
    exit 1
  fi
  sleep 0.2
done

if [[ "$READY" != "1" ]]; then
  echo "ERROR: loomcycle did not become ready within ~10 s. Boot log so far:" >&2
  cat "$BOOT_LOG" >&2
  exit 1
fi

echo "[3/7] Boot-log highlights:"
grep -E "agents:|system channels:|heartbeat" "$BOOT_LOG" | sed 's/^/      /'
echo

# ─── 4. Exercise A: heartbeat cadence ───────────────────────────────
# Heartbeat goroutine starts at boot — the cadence has been ticking
# for boot-time + this wait. We measure rate (rows/s elapsed) rather
# than absolute count to avoid coupling to boot duration.
echo "[4/7] Exercise A — heartbeat cadence (waiting 3s for ~3 heartbeats)..."
HEARTBEAT_START_TS=$(date +%s)
sleep 3.2

DB="$TEST_DIR/data/loomcycle.db"
HEARTBEAT_COUNT=$(sqlite3 "$DB" "SELECT COUNT(*) FROM channel_messages WHERE channel='_system/heartbeat-1s';" 2>/dev/null || echo "0")
ELAPSED=$(($(date +%s) - HEARTBEAT_START_TS))
echo "      heartbeat rows after $ELAPSED+ seconds runtime = $HEARTBEAT_COUNT (expect >=2)"

# Verify one heartbeat payload's shape.
SAMPLE=$(sqlite3 "$DB" "SELECT payload FROM channel_messages WHERE channel='_system/heartbeat-1s' LIMIT 1;" 2>/dev/null || echo "")
echo "      sample payload: $SAMPLE"

# ─── 5. Exercise B: admin endpoint publish ──────────────────────────
echo
echo "[5/7] Exercise B — admin endpoint publish to _system/alarms/info..."
ADMIN_RESP=$(curl -sS -w "\nHTTP_CODE:%{http_code}" \
  -X POST \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"payload": {"severity": "info", "msg": "test alarm"}}' \
  "http://$LOOMCYCLE_LISTEN_ADDR/v1/_channels/_system/alarms/info")
echo "      admin response: $ADMIN_RESP"
ADMIN_CODE=$(echo "$ADMIN_RESP" | grep HTTP_CODE: | sed 's/HTTP_CODE://')
echo "      admin status: $ADMIN_CODE"

ADMIN_ROW=$(sqlite3 "$DB" "SELECT published_by_user_id FROM channel_messages WHERE channel='_system/alarms/info' LIMIT 1;" 2>/dev/null || echo "")
echo "      alarm row published_by_user_id: $ADMIN_ROW"

# ─── 6. Exercise C: agent deferred publish ──────────────────────────
# A 30-second deferral makes the visibility window observable
# regardless of agent latency. We don't ask the agent to subscribe
# back — direct sqlite + Store comparison is more reliable.
echo
echo "[6/7] Exercise C — agent deferred publish (deliver_at=now+30s)..."

DELIVER_AT=$(python3 -c "
import datetime, time
t = datetime.datetime.utcfromtimestamp(time.time() + 30)
print(t.strftime('%Y-%m-%dT%H:%M:%SZ'))
")
echo "      deliver_at = $DELIVER_AT"

PROMPT="Publish exactly one message to channel \"findings\" with value {\"key\":\"deferred-test\"} and deliver_at=\"$DELIVER_AT\". Then write your one-line DONE summary including the message_id and visible_at."

RUN1_SSE="$TEST_DIR/run1-publish.sse"
curl -fsS -N \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d @- "http://$LOOMCYCLE_LISTEN_ADDR/v1/runs" <<EOF > "$RUN1_SSE"
{
  "agent": "scheduler-bot",
  "user_id": "$USER_ID",
  "segments": [
    {"role": "user", "content": [{"type": "trusted-text", "text": $(python3 -c "import json,sys; print(json.dumps(sys.argv[1]))" "$PROMPT")}]}
  ]
}
EOF

# Inspect the row's visible_at vs published_at via sqlite. Both
# stored as unix-nano INTEGER. If visible_at > published_at, the
# deferred publish worked. We pull both for the verdict comparison.
ROW=$(sqlite3 "$DB" "SELECT published_at, visible_at, published_by_user_id FROM channel_messages WHERE channel='findings' LIMIT 1;" 2>/dev/null)
echo "      findings row (published_at|visible_at|user): $ROW"

DEFERRED_ID=$(sqlite3 "$DB" "SELECT id FROM channel_messages WHERE channel='findings' LIMIT 1;" 2>/dev/null)

# Compute visible_at - published_at in seconds. Should be ~30.
DELTA_S=$(sqlite3 "$DB" "SELECT (visible_at - published_at) / 1000000000 FROM channel_messages WHERE channel='findings' LIMIT 1;" 2>/dev/null)
echo "      visible_at - published_at = ${DELTA_S}s (expect ~30)"

# Tool-response should have included visible_at in the JSON. The
# tool_result wraps the inner JSON as a string, so we double-decode.
TOOL_VISIBLE_AT=$(python3 -c "
import json, re
with open('$RUN1_SSE') as f:
  for line in f:
    if line.startswith('data: ') and '\"tool_result\"' in line:
      try:
        outer = json.loads(line[6:].strip())
        inner = json.loads(outer.get('text', '{}'))
        if 'visible_at' in inner:
          print(inner['visible_at'])
          break
      except Exception:
        continue
")
echo "      tool_result visible_at: $TOOL_VISIBLE_AT"

# ─── 7. Storage inspection + verdict ────────────────────────────────
echo
echo "[7/7] Storage inspection ($DB):"
echo "      channel_messages summary by channel:"
sqlite3 -header -column "$DB" "SELECT channel, COUNT(*) AS n, MIN(published_by_user_id) AS sample_publisher FROM channel_messages GROUP BY channel;" | sed 's/^/        /'

# ─── Verdict ───────────────────────────────────────────────────────
echo
echo "── Verdict ───────────────────────────────────────────────────────"
PASS=true

# A — heartbeats (lower bound only — boot adds variable head-time
# to the count; what matters is that the ticker fired multiple times).
if [[ "$HEARTBEAT_COUNT" -ge 2 ]]; then
  echo "  [A] heartbeats >= 2                = $HEARTBEAT_COUNT ✓"
else
  echo "  [A] heartbeats >= 2                = $HEARTBEAT_COUNT ✗"; PASS=false
fi

# A — payload shape: must include "version" and "ts" and "uptime_s"
if echo "$SAMPLE" | grep -q '"version"' && echo "$SAMPLE" | grep -q '"ts"' && echo "$SAMPLE" | grep -q '"uptime_s"'; then
  echo "  [A] heartbeat payload has all keys = ✓"
else
  echo "  [A] heartbeat payload has all keys = ✗ ($SAMPLE)"; PASS=false
fi

# B — admin endpoint
if [[ "$ADMIN_CODE" = "200" ]]; then
  echo "  [B] admin POST status              = 200 ✓"
else
  echo "  [B] admin POST status              = $ADMIN_CODE ✗"; PASS=false
fi
if [[ "$ADMIN_ROW" = "_admin" ]]; then
  echo "  [B] admin row published_by_user_id = _admin ✓"
else
  echo "  [B] admin row published_by_user_id = $ADMIN_ROW ✗"; PASS=false
fi

# C — deferred publish: visible_at must be in the future at publish
# time. The deliver_at was 30s ahead of when the test script started
# the agent run, but the agent took several seconds to actually
# publish, so the realised delta is 30s minus agent latency.
# Tolerate 15..32s (covers a busy provider taking 15s to respond).
if [[ -n "$DELTA_S" ]] && [[ "$DELTA_S" -ge 15 ]] && [[ "$DELTA_S" -le 32 ]]; then
  echo "  [C] visible_at - published_at      = ${DELTA_S}s ✓"
else
  echo "  [C] visible_at - published_at      = ${DELTA_S}s ✗ (expect 15..32)"; PASS=false
fi
if [[ -n "$TOOL_VISIBLE_AT" ]]; then
  echo "  [C] tool_result has visible_at     = $TOOL_VISIBLE_AT ✓"
else
  echo "  [C] tool_result has visible_at     = (missing) ✗"; PASS=false
fi

if $PASS; then
  echo "  PASS ✓"
  exit 0
else
  echo "  FAIL ✗ — see $TEST_DIR for full logs"
  exit 1
fi
