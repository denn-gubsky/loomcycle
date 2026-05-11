#!/usr/bin/env bash
# test/runtime/user-tier/run.sh — v0.8.2 user_tier resolve-time
# tier-walk smoke test.
#
# Drives one /v1/runs with user_tier=low. The tier's candidate list
# is [deepseek, anthropic]. DEEPSEEK_BASE_URL is overridden to
# http://127.0.0.1:1, so:
#
#   1. At boot, the resolver probes /v1/models on each provider.
#      The deepseek probe fails with `connect: connection refused`
#      → matrix marks deepseek unreachable.
#   2. At run-start, the resolver walks the user_tier's candidate
#      list. deepseek is skipped (unreachable in matrix); anthropic
#      is healthy → resolver returns claude-haiku-4-5-20251001.
#   3. The run completes against the fallback provider.
#
# This validates the RESOLVE-TIME tier-walk path of v0.8.2: the
# user_tier overlay correctly walks past stalled candidates and
# picks the next healthy one. The runs.user_tier marker persists
# for cost retros and compliance.
#
# Note: the MID-RUN fallback path (v0.8.2 PR #53 — error
# classifier + ReResolve from inside the loop) is exercised by the
# Go unit tests in internal/loop/fallback_test.go. Exercising it
# from a runtime test would require a fake HTTP server that probes
# 200 on /v1/models but 503 on /v1/chat/completions — out of scope
# for this smoke (the boot-probe path is the simpler shape that
# operators actually hit).
#
# Same `test/runtime/<feature>/` convention as channels/ and memory/.
#
# Usage:
#   DEEPSEEK_API_KEY=... ANTHROPIC_API_KEY=... ./test/runtime/user-tier/run.sh
# OR:
#   set -a; source .env.local; set +a; ./test/runtime/user-tier/run.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-user-tier-test.XXXXXX)"
trap 'cleanup' EXIT INT TERM

cleanup() {
  if [[ -n "${LOOMCYCLE_PID:-}" ]]; then
    kill "$LOOMCYCLE_PID" 2>/dev/null || true
    wait "$LOOMCYCLE_PID" 2>/dev/null || true
  fi
  echo
  echo "Test dir kept for inspection: $TEST_DIR"
}

# ─── Env requirements ───────────────────────────────────────────────
# Need BOTH keys: DeepSeek for the primary registration (the
# unreachable URL means auth is never tried, but the registration
# check is DEEPSEEK_API_KEY != ""), Anthropic for the fallback to
# actually succeed.
if [[ -z "${DEEPSEEK_API_KEY:-}" ]]; then
  echo "ERROR: DEEPSEEK_API_KEY not set. Source .env.local first." >&2
  exit 1
fi
if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "ERROR: ANTHROPIC_API_KEY not set — required for the fallback step." >&2
  echo "  This test needs BOTH DEEPSEEK_API_KEY (primary, will fail by design)" >&2
  echo "  AND ANTHROPIC_API_KEY (fallback, must succeed)." >&2
  exit 1
fi

export LOOMCYCLE_DATA_DIR="$TEST_DIR/data"
export LOOMCYCLE_AGENTS_ROOT="$SCRIPT_DIR/agents"
export LOOMCYCLE_LISTEN_ADDR="127.0.0.1:18789"   # +1 vs memory test
export LOOMCYCLE_AUTH_TOKEN="test-token-$(date +%s)"

# THE KEY KNOB. Point the DeepSeek driver at an unreachable address.
# Port 1 is reserved + nothing listens on it on macOS or Linux by
# default, so the dial fails immediately with ECONNREFUSED.
# v0.8.2's errclass classifies wrapped net.OpError as Retryable →
# fallback fires.
export DEEPSEEK_BASE_URL="http://127.0.0.1:1"

# Shorten the per-stream header timeout so the deepseek call fails
# quickly (the connect attempt itself is sub-millisecond on the
# loopback, but make sure we don't sit idle).
export LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS=5000

mkdir -p "$TEST_DIR/data"
BOOT_LOG="$TEST_DIR/boot.log"
RUN_SSE="$TEST_DIR/run.sse"
USER_ID="test-user-fallback"

# ─── 1. Build fresh binary ──────────────────────────────────────────
echo "[1/5] Building bin/loomcycle..."
go build -o bin/loomcycle ./cmd/loomcycle

# ─── 2. Boot loomcycle ──────────────────────────────────────────────
echo "[2/5] Booting loomcycle at $LOOMCYCLE_LISTEN_ADDR (config: $SCRIPT_DIR/loomcycle.yaml)..."
echo "      DEEPSEEK_BASE_URL=$DEEPSEEK_BASE_URL  (deliberately unreachable)"
./bin/loomcycle --config "$SCRIPT_DIR/loomcycle.yaml" > "$BOOT_LOG" 2>&1 &
LOOMCYCLE_PID=$!

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

echo "[3/5] Boot-log highlights:"
grep -E "user_tiers:|agents:|providers" "$BOOT_LOG" | sed 's/^/      /' || true
echo

# ─── 4. Drive a single run with user_tier=low ──────────────────────
echo "[4/5] Running greeter with user_tier=low (primary deepseek fails → fallback to anthropic)..."
# Drop `-f` (no auto-bail on 4xx/5xx) so the response body lands in
# RUN_SSE for inspection even on error. Capture status separately.
RUN_HTTP_STATUS=$(curl -sS -N -w '%{http_code}' -o "$RUN_SSE" \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d @- "http://$LOOMCYCLE_LISTEN_ADDR/v1/runs" <<EOF
{
  "agent": "greeter",
  "user_id": "$USER_ID",
  "user_tier": "low",
  "segments": [
    {
      "role": "user",
      "content": [
        {"type": "trusted-text", "text": "say hello"}
      ]
    }
  ]
}
EOF
)
echo "      HTTP status: $RUN_HTTP_STATUS"
if [[ "$RUN_HTTP_STATUS" != "200" ]]; then
  echo "      Response body:"
  head -50 "$RUN_SSE" | sed 's/^/        /'
fi

echo "      Run SSE event types:"
grep -E "^event:" "$RUN_SSE" | sort | uniq -c | sort -rn | sed 's/^/        /'

# ─── 5. Verdict ────────────────────────────────────────────────────
DB="$TEST_DIR/data/loomcycle.db"
echo "[5/5] Storage inspection ($DB):"
if [[ -f "$DB" ]]; then
  echo "      runs rows (note: user_tier marker, model = fallback provider):"
  sqlite3 "$DB" "SELECT id, status, stop_reason, model, user_tier FROM runs ORDER BY started_at;" | sed 's/^/        /'
else
  echo "      WARNING: $DB not found"
fi

echo
echo "── Run final text ────────────────────────────────────────────────"
grep -A1 '^event: text$' "$RUN_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" || true
echo

echo
echo "── Verdict ───────────────────────────────────────────────────────"
PASS=true

# Check 1: boot probe marked deepseek unreachable (this is the
# precondition for the tier walk to skip it at run-start).
PROBE_LINE=$(grep -E "resolve probe: deepseek (unreachable|excluded)" "$BOOT_LOG" | head -1)
echo "  Probe marked deepseek bad:      $(if [[ -n "$PROBE_LINE" ]]; then echo "yes"; else echo "no"; PASS=false; fi)"
if [[ -n "$PROBE_LINE" ]]; then
  echo "    └─ $PROBE_LINE"
fi

# Check 2: run completed (not failed/cancelled).
RUN_STATUS=$(sqlite3 "$DB" "SELECT status FROM runs;" 2>/dev/null | head -1)
RUN_MODEL=$(sqlite3 "$DB" "SELECT model FROM runs;" 2>/dev/null | head -1)
RUN_TIER=$(sqlite3 "$DB" "SELECT user_tier FROM runs;" 2>/dev/null | head -1)
echo "  Run status:                     $RUN_STATUS (expect completed)"
[[ "$RUN_STATUS" == "completed" ]] || PASS=false

# Check 3: model used = fallback target = claude-haiku-4-5-20251001
# (the tier's SECOND candidate, picked after deepseek was skipped).
echo "  Run model (= fallback target):  $RUN_MODEL (expect claude-haiku-4-5-20251001)"
[[ "$RUN_MODEL" == "claude-haiku-4-5-20251001" ]] || PASS=false

# Check 4: runs.user_tier marker stamped (v0.8.2 audit column).
echo "  Run user_tier marker:           $RUN_TIER (expect low)"
[[ "$RUN_TIER" == "low" ]] || PASS=false

# Check 5: final text contains "Hello" — proves the fallback provider
# actually executed, not just that it was resolved.
FINAL_TEXT=$(grep -A1 '^event: text$' "$RUN_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" 2>/dev/null)
if echo "$FINAL_TEXT" | grep -qi 'hello'; then
  echo "  Final text contains 'Hello':    yes"
else
  echo "  Final text contains 'Hello':    no"
  PASS=false
fi

if $PASS; then
  echo "  PASS ✓"
  exit 0
else
  echo "  FAIL ✗ — see $TEST_DIR for full logs"
  exit 1
fi
