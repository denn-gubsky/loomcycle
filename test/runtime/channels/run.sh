#!/usr/bin/env bash
# test/runtime/channels/run.sh — v0.8.4 Channel-tool runtime smoke test.
#
# Boots loomcycle with the two test agents in this directory's
# `agents/` subdir, declares a user-scoped `findings` queue via the
# colocated `loomcycle.yaml`, drives one researcher run (publishes
# 3 findings) followed by one analyst run (drains + reports), and
# inspects the resulting sqlite tables.
#
# This is the FIRST of a growing family of per-tool runtime tests
# under `test/runtime/<feature>/`. The pattern:
#   - `agents/<name>.md` per agent (frontmatter + body)
#   - `loomcycle.yaml`  per-test operator config (channels, MCP, etc.)
#   - `run.sh`          driver — builds, boots, drives, inspects
#
# Usage (run from anywhere; the script self-roots to repo root):
#   DEEPSEEK_API_KEY=... ./test/runtime/channels/run.sh
# OR (if .env.local has the key):
#   set -a; source .env.local; set +a; ./test/runtime/channels/run.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

# ─── Test isolation ─────────────────────────────────────────────────
# Fresh data dir + token + port so this test never collides with a
# running production loomcycle. /tmp prefix is auto-cleaned on reboot.
TEST_DIR="$(mktemp -d -t loomcycle-channels-test.XXXXXX)"
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
export LOOMCYCLE_LISTEN_ADDR="127.0.0.1:18787"
export LOOMCYCLE_AUTH_TOKEN="test-token-$(date +%s)"
# Forward whatever DEEPSEEK_API_KEY the operator has in their shell.
if [[ -z "${DEEPSEEK_API_KEY:-}" ]]; then
  echo "ERROR: DEEPSEEK_API_KEY not set. Source .env.local first:" >&2
  echo "  set -a; source .env.local; set +a" >&2
  exit 1
fi

mkdir -p "$TEST_DIR/data"
BOOT_LOG="$TEST_DIR/boot.log"
RESEARCHER_SSE="$TEST_DIR/researcher.sse"
ANALYST_SSE="$TEST_DIR/analyst.sse"

# ─── 1. Build fresh binary ──────────────────────────────────────────
echo "[1/6] Building bin/loomcycle..."
go build -o bin/loomcycle ./cmd/loomcycle

# ─── 2. Boot loomcycle in background ────────────────────────────────
echo "[2/6] Booting loomcycle at $LOOMCYCLE_LISTEN_ADDR (config: $SCRIPT_DIR/loomcycle.yaml)..."
./bin/loomcycle --config "$SCRIPT_DIR/loomcycle.yaml" > "$BOOT_LOG" 2>&1 &
LOOMCYCLE_PID=$!

# Wait up to 10 s for /healthz to respond.
for i in $(seq 1 50); do
  if curl -fsS "http://$LOOMCYCLE_LISTEN_ADDR/healthz" > /dev/null 2>&1; then
    echo "      ready after ~$((i * 200))ms"
    break
  fi
  if ! kill -0 "$LOOMCYCLE_PID" 2>/dev/null; then
    echo "ERROR: loomcycle exited during boot. Boot log:" >&2
    cat "$BOOT_LOG" >&2
    exit 1
  fi
  sleep 0.2
done

# ─── 3. Surface the boot log lines that matter ──────────────────────
echo "[3/6] Boot-log highlights (channels + agents):"
grep -E "channels:|agents:|skills:" "$BOOT_LOG" | sed 's/^/      /'
echo

# ─── 4. Run the researcher ──────────────────────────────────────────
USER_ID="test-user-channels"
echo "[4/6] Running researcher (publishes 3 findings)..."
curl -fsS -N \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d @- "http://$LOOMCYCLE_LISTEN_ADDR/v1/runs" <<EOF > "$RESEARCHER_SSE"
{
  "agent": "researcher",
  "user_id": "$USER_ID",
  "segments": [
    {
      "role": "user",
      "content": [
        {"type": "trusted-text", "text": "Publish your three Tokyo findings now."}
      ]
    }
  ]
}
EOF

# Quick summary of what happened on the researcher's side.
echo "      Researcher SSE event types:"
grep -E "^event:" "$RESEARCHER_SSE" | sort | uniq -c | sort -rn | sed 's/^/        /'

# ─── 5. Run the analyst ─────────────────────────────────────────────
echo "[5/6] Running analyst (drains + reports)..."
curl -fsS -N \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d @- "http://$LOOMCYCLE_LISTEN_ADDR/v1/runs" <<EOF > "$ANALYST_SSE"
{
  "agent": "analyst",
  "user_id": "$USER_ID",
  "segments": [
    {
      "role": "user",
      "content": [
        {"type": "trusted-text", "text": "Drain the findings channel and report."}
      ]
    }
  ]
}
EOF

echo "      Analyst SSE event types:"
grep -E "^event:" "$ANALYST_SSE" | sort | uniq -c | sort -rn | sed 's/^/        /'

# ─── 6. Inspect storage ─────────────────────────────────────────────
DB="$TEST_DIR/data/loomcycle.db"
echo "[6/6] Storage inspection ($DB):"
if [[ -f "$DB" ]]; then
  echo "      channel_messages rows:"
  sqlite3 "$DB" "SELECT id, channel, scope, scope_id, length(payload) AS payload_bytes, published_at FROM channel_messages ORDER BY id;" | sed 's/^/        /'
  echo "      channel_cursors rows:"
  sqlite3 "$DB" "SELECT channel, scope, scope_id, cursor, updated_at FROM channel_cursors;" | sed 's/^/        /'
  echo "      runs rows:"
  sqlite3 "$DB" "SELECT id, session_id, status, stop_reason, model FROM runs ORDER BY started_at;" | sed 's/^/        /'
else
  echo "      WARNING: $DB not found"
fi

# ─── Final report ───────────────────────────────────────────────────
echo
echo "── Researcher's final text output ────────────────────────────────"
# Extract `text` deltas from the SSE stream. The shape is:
#   event: text
#   data: {"text":"..."}
grep -A1 '^event: text$' "$RESEARCHER_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" || true
echo
echo
echo "── Analyst's final text output ───────────────────────────────────"
grep -A1 '^event: text$' "$ANALYST_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" || true
echo
echo
echo "── Verdict ───────────────────────────────────────────────────────"
PUBLISHED=$(sqlite3 "$DB" "SELECT COUNT(*) FROM channel_messages WHERE channel='findings';" 2>/dev/null || echo 0)
CURSORS=$(sqlite3 "$DB" "SELECT COUNT(*) FROM channel_cursors WHERE channel='findings';" 2>/dev/null || echo 0)
echo "  channel_messages count: $PUBLISHED (expect 3)"
echo "  channel_cursors count:  $CURSORS (expect 1 — analyst's committed cursor)"
if [[ "$PUBLISHED" == "3" && "$CURSORS" == "1" ]]; then
  echo "  PASS ✓"
  exit 0
else
  echo "  FAIL ✗ — see $TEST_DIR for full logs"
  exit 1
fi
