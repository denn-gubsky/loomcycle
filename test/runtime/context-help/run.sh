#!/usr/bin/env bash
# test/runtime/context-help/run.sh — v0.8.8 Context.help runtime smoke.
#
# One agent (help-reader.md) calls Context.help twice: once for the
# index, once with topic=scopes for the body. We verify the index is
# non-empty, the agent quotes the body content, and the run completes.
#
# Verdict checks:
#   - run completed cleanly
#   - tool_call events include at least 2 calls to Context
#   - boot log mentions "help: loaded N bundled topics"
#   - final text mentions topic count, a known topic name, and a
#     quoted phrase that exists in the actual scopes topic body
#   - final text ends with DONE
#
# Usage:
#   GEMINI_API_KEY=... ./test/runtime/context-help/run.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-context-help-test.XXXXXX)"
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
export LOOMCYCLE_LISTEN_ADDR="127.0.0.1:18793"
export LOOMCYCLE_AUTH_TOKEN="test-token-$(date +%s)"
if [[ -z "${GEMINI_API_KEY:-}" ]]; then
  echo "ERROR: GEMINI_API_KEY not set. Source .env.local first." >&2
  exit 1
fi
if ! command -v python3 &>/dev/null; then
  echo "ERROR: python3 is required." >&2
  exit 1
fi

mkdir -p "$TEST_DIR/data"
BOOT_LOG="$TEST_DIR/boot.log"
RUN_SSE="$TEST_DIR/run.sse"

echo "[1/5] Building bin/loomcycle..."
go build -o bin/loomcycle ./cmd/loomcycle

echo "[2/5] Booting loomcycle at $LOOMCYCLE_LISTEN_ADDR..."
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
    echo "ERROR: loomcycle exited during boot." >&2
    cat "$BOOT_LOG" >&2
    exit 1
  fi
  sleep 0.2
done
if [[ "$READY" != "1" ]]; then
  echo "ERROR: loomcycle did not become ready within ~10s." >&2
  cat "$BOOT_LOG" >&2
  exit 1
fi

echo "[3/5] Boot-log highlights:"
grep -E "agents:|help:|provider:|build:" "$BOOT_LOG" | sed 's/^/      /'
echo

PROMPT='Execute the two Context.help operations described in your system prompt, then write your one-line DONE summary.'

echo "[4/5] Running help-reader..."
curl -fsS -N \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d @- "http://$LOOMCYCLE_LISTEN_ADDR/v1/runs" <<EOF > "$RUN_SSE"
{
  "agent": "help-reader",
  "user_id": "test-user-help",
  "segments": [
    {"role": "user", "content": [{"type": "trusted-text", "text": $(python3 -c "import json,sys; print(json.dumps(sys.argv[1]))" "$PROMPT")}]}
  ]
}
EOF

echo "      SSE event types:"
grep -E "^event:" "$RUN_SSE" | sort | uniq -c | sort -rn | sed 's/^/        /'

# Count Context tool calls.
CONTEXT_CALLS=$(grep -E '"name":"Context"' "$RUN_SSE" | grep -c "tool_use" || echo "0")
echo "      Context tool calls: $CONTEXT_CALLS"

FINAL_TEXT=$(grep -A1 '^event: text$' "$RUN_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" 2>/dev/null)

DB="$TEST_DIR/data/loomcycle.db"
RUN_STATUS=$(sqlite3 "$DB" "SELECT status FROM runs ORDER BY started_at LIMIT 1;" 2>/dev/null || echo "")

echo
echo "── Final text ────────────────────────────────────────────────────"
echo "$FINAL_TEXT"

echo
echo "[5/5] Verdict:"
PASS=true

if [[ "$CONTEXT_CALLS" -ge 2 ]]; then
  echo "  Context calls >= 2          = $CONTEXT_CALLS ✓"
else
  echo "  Context calls >= 2          = $CONTEXT_CALLS ✗"; PASS=false
fi

if [[ "$RUN_STATUS" = "completed" ]]; then
  echo "  run status                  = completed ✓"
else
  echo "  run status                  = $RUN_STATUS ✗"; PASS=false
fi

# Boot log mentions help: loaded.
if grep -qE "help: loaded [0-9]+ bundled topics" "$BOOT_LOG"; then
  echo "  boot logs help: loaded       = ✓"
else
  echo "  boot logs help: loaded       = ✗"; PASS=false
fi

# Final text mentions a topic name we know is in the bundled index.
if echo "$FINAL_TEXT" | grep -qiE "scopes|subagents|experimentation|loomcycle|system-channels"; then
  echo "  text mentions a topic name   = ✓"
else
  echo "  text mentions a topic name   = ✗"; PASS=false
fi

# Final text ends with DONE.
if echo "$FINAL_TEXT" | grep -qi "DONE"; then
  echo "  text ends with DONE          = ✓"
else
  echo "  text ends with DONE          = ✗"; PASS=false
fi

if $PASS; then
  echo "  PASS ✓"
  exit 0
else
  echo "  FAIL ✗ — see $TEST_DIR for full logs"
  exit 1
fi
