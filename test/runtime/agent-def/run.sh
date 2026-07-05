#!/usr/bin/env bash
# test/runtime/agent-def/run.sh — v0.8.5 AgentDef-tool runtime smoke test.
#
# One agent (./agents/evolver.md) called once. The prompt asks it to
# chain six AgentDef ops — create, get, list, fork, promote, retire —
# against a fresh name ("derived-bot") that has no static cfg.Agents
# entry. The driver inspects the agent_defs + agent_def_active
# sqlite tables to verify the lifecycle ended in the expected shape.
#
# Verdict checks:
#   - agent_defs has 2 rows for `derived-bot` (v1 from create, v2 from fork)
#   - the v2 row has parent_def_id = v1's def_id
#   - the v2 row has retired = 1
#   - agent_def_active points at v1 (per the promote step)
#   - evolver's final text mentions DONE
#
# Same `test/runtime/<feature>/` convention as channels/, memory/,
# user-tier/.
#
# Usage:
#   GEMINI_API_KEY=... ./test/runtime/agent-def/run.sh
# OR:
#   set -a; source .env.local; set +a; ./test/runtime/agent-def/run.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-agent-def-test.XXXXXX)"
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
export LOOMCYCLE_LISTEN_ADDR="127.0.0.1:18789"
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
RUN_SSE="$TEST_DIR/run.sse"
USER_ID="test-user-agent-def"

# ─── 1. Build fresh binary ──────────────────────────────────────────
echo "[1/5] Building bin/loomcycle..."
go build -o bin/loomcycle ./cmd/loomcycle

# ─── 2. Boot loomcycle in background ────────────────────────────────
echo "[2/5] Booting loomcycle at $LOOMCYCLE_LISTEN_ADDR (config: $SCRIPT_DIR/loomcycle.yaml)..."
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

echo "[3/5] Boot-log highlights:"
grep -E "agents:|provider:|build:" "$BOOT_LOG" | sed 's/^/      /'
echo

# ─── 4. Drive the run ───────────────────────────────────────────────
# The prompt names six AgentDef ops in order. The evolver will chain
# them as tool calls. We pass instructions via a heredoc to keep the
# JSON request body simple (escape only the inner doublequotes).
echo "[4/5] Running evolver (single chained run)..."

PROMPT='Execute these six AgentDef operations in order. Each maps to one tool call.

(1) create a new agent named "derived-bot" with overlay {"system_prompt":"a derived prompt","tools":["AgentDef"]} and description "v1".

(2) get the def_id you just created. Read back the row.

(3) list all versions for name="derived-bot". There should be one row.

(4) fork name="derived-bot" with overlay {"system_prompt":"a forked prompt"} and description "v2". This creates v2 with parent_def_id=v1.

(5) promote the v1 def_id (the one from step 1) back to active. v1 is the active version again.

(6) retire the v2 def_id (the one from step 4) with retired=true.

After all six, write your one-line summary per the rules in your system prompt.'

curl -fsS -N \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d @- "http://$LOOMCYCLE_LISTEN_ADDR/v1/runs" <<EOF > "$RUN_SSE"
{
  "agent": "evolver",
  "user_id": "$USER_ID",
  "segments": [
    {
      "role": "user",
      "content": [
        {"type": "trusted-text", "text": $(python3 -c "import json,sys; print(json.dumps(sys.argv[1]))" "$PROMPT")}
      ]
    }
  ]
}
EOF

echo "      SSE event types:"
grep -E "^event:" "$RUN_SSE" | sort | uniq -c | sort -rn | sed 's/^/        /'

# ─── 5. Storage inspection ─────────────────────────────────────────
DB="$TEST_DIR/data/loomcycle.db"
echo
echo "[5/5] Storage inspection ($DB):"
if [[ -f "$DB" ]]; then
  echo "      agent_defs rows (for derived-bot):"
  sqlite3 -header -column "$DB" \
    "SELECT def_id, name, version, parent_def_id, retired, bootstrapped_from_static FROM agent_defs WHERE name='derived-bot' ORDER BY version;" \
    | sed 's/^/        /'
  echo "      agent_def_active row:"
  sqlite3 -header -column "$DB" \
    "SELECT name, def_id, promoted_by_agent_id FROM agent_def_active WHERE name='derived-bot';" \
    | sed 's/^/        /'
  echo "      runs row (the evolver run):"
  sqlite3 "$DB" "SELECT id, status, stop_reason, model FROM runs ORDER BY started_at;" | sed 's/^/        /'
else
  echo "      WARNING: $DB not found"
fi

# ─── Final report ──────────────────────────────────────────────────
echo
echo "── Final text ────────────────────────────────────────────────────"
grep -A1 '^event: text$' "$RUN_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" || true
echo

# ─── Verdict ───────────────────────────────────────────────────────
echo
echo "── Verdict ───────────────────────────────────────────────────────"
PASS=true

# 2 rows expected for name=derived-bot (v1 from create, v2 from fork).
ROW_COUNT=$(sqlite3 "$DB" "SELECT COUNT(*) FROM agent_defs WHERE name='derived-bot';" 2>/dev/null || echo "0")
echo "  agent_defs rows for derived-bot      = $ROW_COUNT (expect 2)"
[[ "$ROW_COUNT" == "2" ]] || PASS=false

# v1 def_id = the row with version=1; v2's parent_def_id must match.
V1_DEFID=$(sqlite3 "$DB" "SELECT def_id FROM agent_defs WHERE name='derived-bot' AND version=1;" 2>/dev/null || echo "")
V2_PARENT=$(sqlite3 "$DB" "SELECT parent_def_id FROM agent_defs WHERE name='derived-bot' AND version=2;" 2>/dev/null || echo "")
echo "  v2.parent_def_id == v1.def_id        = $([ "$V2_PARENT" = "$V1_DEFID" ] && [ -n "$V1_DEFID" ] && echo yes || { echo no; PASS=false; }) ($V2_PARENT vs $V1_DEFID)"

# v2 must be retired.
V2_RETIRED=$(sqlite3 "$DB" "SELECT retired FROM agent_defs WHERE name='derived-bot' AND version=2;" 2>/dev/null || echo "")
echo "  v2 retired                           = $V2_RETIRED (expect 1)"
[[ "$V2_RETIRED" == "1" ]] || PASS=false

# v1 must NOT be retired.
V1_RETIRED=$(sqlite3 "$DB" "SELECT retired FROM agent_defs WHERE name='derived-bot' AND version=1;" 2>/dev/null || echo "")
echo "  v1 retired                           = $V1_RETIRED (expect 0)"
[[ "$V1_RETIRED" == "0" ]] || PASS=false

# Active pointer must equal v1 (because of the promote(v1) at step 5).
ACTIVE_DEFID=$(sqlite3 "$DB" "SELECT def_id FROM agent_def_active WHERE name='derived-bot';" 2>/dev/null || echo "")
echo "  active pointer == v1.def_id          = $([ "$ACTIVE_DEFID" = "$V1_DEFID" ] && [ -n "$V1_DEFID" ] && echo yes || { echo no; PASS=false; }) ($ACTIVE_DEFID)"

# Evolver's final text should contain DONE.
RUN_TEXT=$(grep -A1 '^event: text$' "$RUN_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" 2>/dev/null)
DONE_PRESENT=$(echo "$RUN_TEXT" | grep -qi 'DONE' && echo yes || { echo no; PASS=false; })
echo "  final text mentions DONE             = $DONE_PRESENT"

# Run finished cleanly.
RUN_STATUS=$(sqlite3 "$DB" "SELECT status FROM runs ORDER BY started_at LIMIT 1;" 2>/dev/null || echo "")
echo "  run status                           = $RUN_STATUS (expect completed)"
[[ "$RUN_STATUS" == "completed" ]] || PASS=false

if $PASS; then
  echo "  PASS ✓"
  exit 0
else
  echo "  FAIL ✗ — see $TEST_DIR for full logs"
  exit 1
fi
