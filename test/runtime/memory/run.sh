#!/usr/bin/env bash
# test/runtime/memory/run.sh — v0.8.0 Memory-tool runtime smoke test.
#
# One agent (./agents/memorybot.md) called twice on the same
# loomcycle instance + sqlite DB. Run 1 writes a user-scope fact +
# an agent-scope counter (incr). Run 2 reads them back and bumps
# the counter again. State persistence across the run boundary —
# the core promise of Memory — is what this validates against the
# real wire.
#
# Same `test/runtime/<feature>/` convention as channels/.
#
# Usage:
#   DEEPSEEK_API_KEY=... ./test/runtime/memory/run.sh
# OR:
#   set -a; source .env.local; set +a; ./test/runtime/memory/run.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-memory-test.XXXXXX)"
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
export LOOMCYCLE_LISTEN_ADDR="127.0.0.1:18788"   # +1 vs channels test
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
RUN1_SSE="$TEST_DIR/run1.sse"
RUN2_SSE="$TEST_DIR/run2.sse"
USER_ID="test-user-memory"

# ─── 1. Build fresh binary ──────────────────────────────────────────
echo "[1/6] Building bin/loomcycle..."
go build -o bin/loomcycle ./cmd/loomcycle

# ─── 2. Boot loomcycle in background ────────────────────────────────
echo "[2/6] Booting loomcycle at $LOOMCYCLE_LISTEN_ADDR (config: $SCRIPT_DIR/loomcycle.yaml)..."
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
  # Boot taking longer than 10 s (cold-start probes on a slow
  # network). Fail loudly so the cause is visible.
  echo "ERROR: loomcycle did not become ready within ~10 s. Boot log so far:" >&2
  cat "$BOOT_LOG" >&2
  exit 1
fi

echo "[3/6] Boot-log highlights:"
grep -E "memory:|agents:|skills:" "$BOOT_LOG" | sed 's/^/      /'
echo

# Helper: POST one /v1/runs with a user prompt; capture SSE to a file.
post_run() {
  local out_path="$1" prompt="$2"
  curl -fsS -N \
    -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
    -H "Content-Type: application/json" \
    -H "Accept: text/event-stream" \
    -d @- "http://$LOOMCYCLE_LISTEN_ADDR/v1/runs" <<EOF > "$out_path"
{
  "agent": "memorybot",
  "user_id": "$USER_ID",
  "segments": [
    {
      "role": "user",
      "content": [
        {"type": "trusted-text", "text": "$prompt"}
      ]
    }
  ]
}
EOF
}

# ─── 4. Run 1 — write phase ────────────────────────────────────────
echo "[4/6] Running memorybot (run 1: write phase)..."
post_run "$RUN1_SSE" "Do these three Memory operations in order. (1) set scope=user key=favorite_color value=purple. (2) set scope=agent key=state value=started. (3) incr scope=agent key=run_count delta=1."

echo "      Run 1 SSE event types:"
grep -E "^event:" "$RUN1_SSE" | sort | uniq -c | sort -rn | sed 's/^/        /'

# ─── 5. Run 2 — read phase ─────────────────────────────────────────
echo "[5/6] Running memorybot (run 2: read phase)..."
post_run "$RUN2_SSE" "Do these three Memory operations in order. (1) get scope=user key=favorite_color. (2) get scope=agent key=state. (3) incr scope=agent key=run_count delta=1."

echo "      Run 2 SSE event types:"
grep -E "^event:" "$RUN2_SSE" | sort | uniq -c | sort -rn | sed 's/^/        /'

# ─── 6. Storage inspection ─────────────────────────────────────────
DB="$TEST_DIR/data/loomcycle.db"
echo "[6/6] Storage inspection ($DB):"
if [[ -f "$DB" ]]; then
  echo "      memory rows:"
  sqlite3 "$DB" "SELECT scope, scope_id, key, value, expires_at FROM memory ORDER BY scope, key;" | sed 's/^/        /'
  echo "      runs rows:"
  sqlite3 "$DB" "SELECT id, session_id, status, stop_reason, model FROM runs ORDER BY started_at;" | sed 's/^/        /'
else
  echo "      WARNING: $DB not found"
fi

# ─── Final report ──────────────────────────────────────────────────
echo
echo "── Run 1 final text ──────────────────────────────────────────────"
grep -A1 '^event: text$' "$RUN1_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" || true
echo
echo
echo "── Run 2 final text ──────────────────────────────────────────────"
grep -A1 '^event: text$' "$RUN2_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" || true
echo

# ─── Verdict ───────────────────────────────────────────────────────
# Expected final state in the memory table:
#   user / test-user-memory / favorite_color = "purple"
#   agent / memorybot / state                 = "started"
#   agent / memorybot / run_count             = 2
#
# Plus: run 2's text output must mention "purple" AND "started" (proof
# the agent SAW the persisted state — not just that the rows happen
# to be there).
echo
echo "── Verdict ───────────────────────────────────────────────────────"
PURPLE_VALUE=$(sqlite3 "$DB" "SELECT value FROM memory WHERE scope='user' AND key='favorite_color';" 2>/dev/null || echo "")
STATE_VALUE=$(sqlite3 "$DB" "SELECT value FROM memory WHERE scope='agent' AND key='state';" 2>/dev/null || echo "")
COUNTER_VALUE=$(sqlite3 "$DB" "SELECT value FROM memory WHERE scope='agent' AND key='run_count';" 2>/dev/null || echo "")
RUN2_TEXT=$(grep -A1 '^event: text$' "$RUN2_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" 2>/dev/null)

# Memory values are JSON-encoded; recursively json.loads until we
# hit a non-string. This handles BOTH the canonical single-encoded
# shape (Memory.set called with value: "purple" → stored bytes
# "purple") AND the double-encoded shape some models emit (value:
# "\"purple\"" → stored bytes "\"purple\""). Either is acceptable
# storage end state — the agent saw the value and acted on it.
unwrap_json_value() {
  echo "$1" | python3 -c "
import sys, json
raw = sys.stdin.read().strip()
if not raw:
    sys.exit(0)
v = raw
# Decode at most 3 times to bound runaway loops on malformed input.
for _ in range(3):
    try:
        decoded = json.loads(v)
    except Exception:
        break
    if isinstance(decoded, str):
        v = decoded
        continue
    v = decoded
    break
print(v if isinstance(v, str) else json.dumps(v))
"
}

PASS=true
PURPLE_DECODED=$(unwrap_json_value "$PURPLE_VALUE")
STATE_DECODED=$(unwrap_json_value "$STATE_VALUE")
echo "  memory.user.favorite_color    = $PURPLE_VALUE  → decoded: $PURPLE_DECODED  (expect: purple)"
[[ "$PURPLE_DECODED" == "purple" ]] || PASS=false
echo "  memory.agent.state            = $STATE_VALUE  → decoded: $STATE_DECODED  (expect: started)"
[[ "$STATE_DECODED" == "started" ]] || PASS=false
echo "  memory.agent.run_count        = $COUNTER_VALUE (expect 2)"
[[ "$COUNTER_VALUE" == "2" ]] || PASS=false

echo "  run 2 text mentions 'purple'  → $(echo "$RUN2_TEXT" | grep -qi 'purple'  && echo yes || { echo no; PASS=false; })"
echo "  run 2 text mentions 'started' → $(echo "$RUN2_TEXT" | grep -qi 'started' && echo yes || { echo no; PASS=false; })"

if $PASS; then
  echo "  PASS ✓"
  exit 0
else
  echo "  FAIL ✗ — see $TEST_DIR for full logs"
  exit 1
fi
