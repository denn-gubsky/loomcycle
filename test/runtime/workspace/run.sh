#!/usr/bin/env bash
# test/runtime/workspace/run.sh — workspaces (RFC GW P1) end to end through a
# booted server in workspace.mode local. Deterministic: code-js agents (no
# LLM, no provider key), the real loop, the real local worker child on its
# private socket. Checks that:
#   - an agent opens a workspace, writes a Python script, runs it and reads
#     the file it wrote, and the workspace's directory is gone from the
#     worker's disk once the run has ended;
#   - a run paused mid-run (RFC X) keeps its workspace: the directory is
#     there while the runtime is paused, the run uses it again after the
#     resume, and the run's end releases it;
#   - the local worker exits when the runtime is killed (--parent-pid);
#   - in open mode (no authentication) a run that names no tenant_id, so
#     carries the empty tenant, opens, uses and releases a workspace on the
#     local worker, which serves that tenant when workspace.local.tenant is
#     unset.
# Needs python3 on PATH (the workspace runs it); skips cleanly without it.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

if ! command -v python3 >/dev/null 2>&1; then
  echo "SKIP — python3 is not on PATH; the workspace scenario runs a Python script on the host engine"
  exit 0
fi

TEST_DIR="$(mktemp -d -t loomcycle-workspace.XXXXXX)"
PORT=18951
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  [[ -n "${RUNPID:-}" ]] && { kill "$RUNPID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
BASE="http://127.0.0.1:$PORT"
DATA="$(cd "$TEST_DIR" && pwd -P)/data"
ROOT="$DATA/workspaces"
fail() { echo "FAIL ✗ — $1"; exit 1; }

sse_text() {
  grep -A1 '^event: text$' "$1" | grep '^data:' | sed 's/^data: //' | \
    python3 -c "import sys,json
for line in sys.stdin:
  try: print(json.loads(line).get('text',''), end='')
  except: pass" 2>/dev/null || true
}
post_run() {
  curl -fsS -N -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -H "Accept: text/event-stream" -d @- "$BASE/v1/runs" <<JSON > "$1"
{"agent":"$2","user_id":"ws-user","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}
JSON
}
admin() { curl -fsS -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -d "$2" "$BASE$1"; }
# workspaces lists the workspace directories on the local worker's disk.
workspaces() { find "$ROOT" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | sort; }
# wait_until polls a condition for up to $1 seconds.
wait_until() {
  local secs="$1"; shift
  for _ in $(seq 1 $((secs * 10))); do "$@" && return 0; sleep 0.1; done
  return 1
}
no_workspaces() { [[ -z "$(workspaces)" ]]; }

# The legacy LOOMCYCLE_AUTH_TOKEN runs under tenant "default", the tenant the
# local worker serves by default.
cat > "$TEST_DIR/loomcycle.yaml" <<YAML
provider_priority: [mock]
tiers:
  middle:
    - { provider: mock, model: mock-generic }
conversion:
  mode: inprocess
workspace:
  mode: local
agents:
  wsflow:
    provider: code-js
    description: "Workspace end to end: open, write a Python script, run it, read its output."
    tools: [Workspace]
    workspace: { ops: [read, write, exec], open: true }
  wspause:
    provider: code-js
    description: "A workspace held across a runtime pause."
    tools: [Workspace]
    workspace: { ops: [read, write, exec], open: true }
YAML

echo "[1/6] build + boot (workspace.mode local)"
go build -o bin/loomcycle ./cmd/loomcycle
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
LOOMCYCLE_MOCK_ENABLED=1 \
LOOMCYCLE_CODE_AGENTS_ROOT="$SCRIPT_DIR/agent_code" \
LOOMCYCLE_DATA_DIR="$DATA" \
LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$PORT" \
LOOMCYCLE_AUTH_TOKEN="$TOKEN" \
  ./bin/loomcycle --config "$TEST_DIR/loomcycle.yaml" > "$TEST_DIR/boot.log" 2>&1 &
PID=$!
READY=0
for i in $(seq 1 50); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && { READY=1; break; }
  kill -0 "$PID" 2>/dev/null || { echo "boot failed:"; cat "$TEST_DIR/boot.log"; exit 1; }
  sleep 0.2
done
[[ "$READY" == "1" ]] || { echo "not ready:"; cat "$TEST_DIR/boot.log"; exit 1; }
grep -q "workspace: mode local runs every agent's workspace commands" "$TEST_DIR/boot.log" || fail "boot log does not warn that local mode has no isolation"
wait_until 15 grep -q "workspace worker attached.*worker_id=local:default/local" "$TEST_DIR/boot.log" || fail "the local worker did not attach"

echo "[2/6] wsflow: open, write square.py, run it, read out.txt"
post_run "$TEST_DIR/flow.sse" wsflow
R=$(sse_text "$TEST_DIR/flow.sse")
echo "      $R"
for want in "state=ready" "engine=host" "exit=0" "printed=squared_12" "result=144" "listed=1"; do
  echo "$R" | grep -q -- "$want" || fail "wsflow: want '$want' in: $R"
done

echo "[3/6] the run's end released the workspace: its directory is gone"
wait_until 10 no_workspaces || fail "workspace directories left after the run ended: $(workspaces)"

echo "[4/6] wspause: a run paused mid-run keeps its workspace"
post_run "$TEST_DIR/pause.sse" wspause &
RUNPID=$!
started() { compgen -G "$ROOT/*/started" >/dev/null; }
wait_until 15 started || fail "the paused run's command never started"
P=$(admin /v1/_pause '{"timeout_ms":20000}')
echo "      pause: $P"
echo "$P" | grep -q '"paused_runs_count":1' || fail "pause did not park the run: $P"
HELD=$(workspaces)
[[ -n "$HELD" && -f "$HELD/marker.txt" ]] || fail "the paused run's workspace is gone while it is paused"
sleep 2
[[ -f "$HELD/marker.txt" ]] || fail "the paused run's workspace went away during the pause"
R=$(admin /v1/_resume '{}')
echo "      resume: $R"
wait "$RUNPID" || fail "the paused run's stream failed"
RUNPID=""
R=$(sse_text "$TEST_DIR/pause.sse")
echo "      $R"
for want in "slept=slept" "marker=kept_across_the_pause" "after=42"; do
  echo "$R" | grep -q -- "$want" || fail "wspause: want '$want' in: $R"
done
wait_until 10 no_workspaces || fail "workspace directories left after the resumed run ended: $(workspaces)"
if command -v sqlite3 >/dev/null 2>&1 && [[ -f "$DATA/loomcycle.db" ]]; then
  N=$(sqlite3 "$DATA/loomcycle.db" "SELECT count(*) FROM workspaces;")
  [[ "$N" == "0" ]] || fail "$N workspace rows left after both runs ended"
fi
if grep -q '^event: error$' "$TEST_DIR/flow.sse" "$TEST_DIR/pause.sse"; then
  fail "an EventError appeared on a run stream"
fi

echo "[5/6] the local worker exits when the runtime is killed"
CHILD=$(pgrep -P "$PID" -f "role workspace" || true)
[[ -n "$CHILD" ]] || fail "no local workspace worker is running under the runtime"
kill -9 "$PID"; wait "$PID" 2>/dev/null || true; PID=""
gone() { ! kill -0 "$CHILD" 2>/dev/null; }
wait_until 10 gone || fail "the local worker (pid $CHILD) outlived the runtime"
echo "[6/6] open mode: a run that names no tenant opens, uses and releases a workspace"
OPEN_PORT=18952
OPEN_BASE="http://127.0.0.1:$OPEN_PORT"
OPEN_DATA="$(cd "$TEST_DIR" && pwd -P)/open-data"
ROOT="$OPEN_DATA/workspaces"
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
LOOMCYCLE_MOCK_ENABLED=1 \
LOOMCYCLE_CODE_AGENTS_ROOT="$SCRIPT_DIR/agent_code" \
LOOMCYCLE_DATA_DIR="$OPEN_DATA" \
LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$OPEN_PORT" \
  ./bin/loomcycle --config "$TEST_DIR/loomcycle.yaml" > "$TEST_DIR/boot-open.log" 2>&1 &
PID=$!
READY=0
for i in $(seq 1 50); do
  curl -fsS "$OPEN_BASE/healthz" >/dev/null 2>&1 && { READY=1; break; }
  kill -0 "$PID" 2>/dev/null || { echo "open-mode boot failed:"; cat "$TEST_DIR/boot-open.log"; exit 1; }
  sleep 0.2
done
[[ "$READY" == "1" ]] || { echo "not ready:"; cat "$TEST_DIR/boot-open.log"; exit 1; }
wait_until 15 grep -q "workspace worker attached.*worker_id=local:/local" "$TEST_DIR/boot-open.log" || fail "the open-mode local worker did not attach for the empty tenant"
# No Authorization header and no tenant_id: the run carries the empty tenant.
curl -fsS -N -H "Content-Type: application/json" -H "Accept: text/event-stream" \
  -d '{"agent":"wsflow","user_id":"ws-user","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}' \
  "$OPEN_BASE/v1/runs" > "$TEST_DIR/open.sse"
R=$(sse_text "$TEST_DIR/open.sse")
echo "      $R"
for want in "state=ready" "exit=0" "result=144" "listed=1"; do
  echo "$R" | grep -q -- "$want" || fail "open mode: want '$want' in: $R"
done
grep -q '^event: error$' "$TEST_DIR/open.sse" && fail "an EventError appeared on the open-mode run stream"
wait_until 10 no_workspaces || fail "open mode: workspace directories left after the run ended: $(workspaces)"
echo "=== workspace runtime suite PASSED ==="
