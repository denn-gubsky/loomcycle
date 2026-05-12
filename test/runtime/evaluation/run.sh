#!/usr/bin/env bash
# test/runtime/evaluation/run.sh — v0.8.5 Evaluation-tool runtime smoke test.
#
# Two-run scenario:
#   Run 1: `worker` does a trivial deterministic op. The driver
#          captures the run_id from the SSE "agent" event.
#   Run 2: `evaluator` receives the worker's run_id in its prompt,
#          submits one Evaluation, then reads it back via get +
#          list_for_run, then attempts aggregate (which will refuse
#          for missing def_id — that branch exists to confirm the
#          read-side scope gate fires correctly).
#
# emitter_role for the evaluator's submit will be "unrelated"
# (different agents, no parent relationship). That's the `submit_any`
# code path; the test exercises exactly that.
#
# Verdict checks:
#   - evaluations table has exactly 1 row
#   - row.run_id == worker's run_id
#   - row.score == 0.8
#   - row.emitter_role == "unrelated"
#   - evaluator's final text mentions the eval_id and score 0.8
#
# Usage:
#   GEMINI_API_KEY=... ./test/runtime/evaluation/run.sh
# OR:
#   set -a; source .env.local; set +a; ./test/runtime/evaluation/run.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-evaluation-test.XXXXXX)"
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
export LOOMCYCLE_LISTEN_ADDR="127.0.0.1:18790"
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
WORKER_SSE="$TEST_DIR/worker.sse"
EVALUATOR_SSE="$TEST_DIR/evaluator.sse"
USER_ID="test-user-eval"

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
  echo "ERROR: loomcycle did not become ready within ~10 s. Boot log so far:" >&2
  cat "$BOOT_LOG" >&2
  exit 1
fi

echo "[3/6] Boot-log highlights:"
grep -E "agents:|provider:|build:" "$BOOT_LOG" | sed 's/^/      /'
echo

# Helper: POST one /v1/runs with a user prompt; capture SSE to a file.
post_run() {
  local agent="$1" out_path="$2" prompt="$3"
  curl -fsS -N \
    -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
    -H "Content-Type: application/json" \
    -H "Accept: text/event-stream" \
    -d @- "http://$LOOMCYCLE_LISTEN_ADDR/v1/runs" <<EOF > "$out_path"
{
  "agent": "$agent",
  "user_id": "$USER_ID",
  "segments": [
    {
      "role": "user",
      "content": [
        {"type": "trusted-text", "text": $(python3 -c "import json,sys; print(json.dumps(sys.argv[1]))" "$prompt")}
      ]
    }
  ]
}
EOF
}

# Extract the run_id from the "agent" SSE event (first event emitted
# at run start; payload includes run_id / agent_id / session_id).
extract_run_id() {
  local sse_path="$1"
  grep -A1 '^event: agent$' "$sse_path" | grep '^data:' | head -1 | \
    sed 's/^data: //' | python3 -c "
import sys, json
for line in sys.stdin:
    try:
        d = json.loads(line)
        if 'run_id' in d:
            print(d['run_id'])
            break
    except Exception:
        pass
"
}

# ─── 4. Run 1 — worker ─────────────────────────────────────────────
echo "[4/6] Running worker (target for the evaluation)..."
post_run worker "$WORKER_SSE" 'Do the single Memory operation described in your system prompt. One tool call only. Then write the one-line DONE summary.'

WORKER_RUN_ID=$(extract_run_id "$WORKER_SSE")
echo "      worker run_id: $WORKER_RUN_ID"
if [[ -z "$WORKER_RUN_ID" || "$WORKER_RUN_ID" == "null" ]]; then
  echo "ERROR: could not extract worker run_id from SSE; see $WORKER_SSE" >&2
  exit 1
fi

echo "      worker SSE event types:"
grep -E "^event:" "$WORKER_SSE" | sort | uniq -c | sort -rn | sed 's/^/        /'

# ─── 5. Run 2 — evaluator ──────────────────────────────────────────
echo
echo "[5/6] Running evaluator against worker's run_id..."
EVAL_PROMPT="Here is the worker's run_id: $WORKER_RUN_ID

Execute the four Evaluation operations described in your system prompt, using this run_id as the target. Then write the one-line DONE summary."

post_run evaluator "$EVALUATOR_SSE" "$EVAL_PROMPT"

echo "      evaluator SSE event types:"
grep -E "^event:" "$EVALUATOR_SSE" | sort | uniq -c | sort -rn | sed 's/^/        /'

# ─── 6. Storage inspection ─────────────────────────────────────────
DB="$TEST_DIR/data/loomcycle.db"
echo
echo "[6/6] Storage inspection ($DB):"
if [[ -f "$DB" ]]; then
  echo "      evaluations rows:"
  sqlite3 -header -column "$DB" \
    "SELECT eval_id, run_id, score, emitter_role, emitter_agent_id, rationale FROM evaluations;" \
    | sed 's/^/        /'
  echo "      runs rows:"
  sqlite3 "$DB" "SELECT id, status, stop_reason FROM runs ORDER BY started_at;" | sed 's/^/        /'
else
  echo "      WARNING: $DB not found"
fi

# ─── Final report ──────────────────────────────────────────────────
echo
echo "── Worker final text ─────────────────────────────────────────────"
grep -A1 '^event: text$' "$WORKER_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" || true
echo

echo
echo "── Evaluator final text ──────────────────────────────────────────"
grep -A1 '^event: text$' "$EVALUATOR_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" || true
echo

# ─── Verdict ───────────────────────────────────────────────────────
echo
echo "── Verdict ───────────────────────────────────────────────────────"
PASS=true

EVAL_COUNT=$(sqlite3 "$DB" "SELECT COUNT(*) FROM evaluations;" 2>/dev/null || echo "0")
echo "  evaluations rows                     = $EVAL_COUNT (expect 1)"
[[ "$EVAL_COUNT" == "1" ]] || PASS=false

EVAL_ROW_RUNID=$(sqlite3 "$DB" "SELECT run_id FROM evaluations LIMIT 1;" 2>/dev/null || echo "")
echo "  row.run_id == worker run_id          = $([ "$EVAL_ROW_RUNID" = "$WORKER_RUN_ID" ] && [ -n "$WORKER_RUN_ID" ] && echo yes || { echo no; PASS=false; }) ($EVAL_ROW_RUNID vs $WORKER_RUN_ID)"

EVAL_SCORE=$(sqlite3 "$DB" "SELECT score FROM evaluations LIMIT 1;" 2>/dev/null || echo "")
echo "  row.score                            = $EVAL_SCORE (expect 0.8)"
[[ "$EVAL_SCORE" == "0.8" ]] || PASS=false

EVAL_ROLE=$(sqlite3 "$DB" "SELECT emitter_role FROM evaluations LIMIT 1;" 2>/dev/null || echo "")
echo "  row.emitter_role                     = $EVAL_ROLE (expect unrelated)"
[[ "$EVAL_ROLE" == "unrelated" ]] || PASS=false

EVAL_TEXT=$(grep -A1 '^event: text$' "$EVALUATOR_SSE" | grep '^data:' | sed 's/^data: //' | \
  python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" 2>/dev/null)

# Evaluator's text should mention score 0.8 and DONE.
echo "  evaluator text mentions 0.8          = $(echo "$EVAL_TEXT" | grep -qE '0\.8|0,8' && echo yes || { echo no; PASS=false; })"
echo "  evaluator text mentions DONE         = $(echo "$EVAL_TEXT" | grep -qi 'DONE' && echo yes || { echo no; PASS=false; })"

# Worker run should have completed.
WORKER_STATUS=$(sqlite3 "$DB" "SELECT status FROM runs WHERE id='$WORKER_RUN_ID';" 2>/dev/null || echo "")
echo "  worker run status                    = $WORKER_STATUS (expect completed)"
[[ "$WORKER_STATUS" == "completed" ]] || PASS=false

if $PASS; then
  echo "  PASS ✓"
  exit 0
else
  echo "  FAIL ✗ — see $TEST_DIR for full logs"
  exit 1
fi
