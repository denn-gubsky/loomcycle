#!/usr/bin/env bash
# test/runtime/workspace/personal.sh — a personal worker (RFC GW P13) end to
# end: a booted runtime in workspace.mode remote with
# workspace.personal_workers on and its worker endpoint on loopback, a user
# and that user's personal worker token minted through the per-user mint,
# and `loomcycle worker --role workspace --personal --root code=<folder>`.
# Deterministic: a code-js agent, no provider key. Checks that:
#   - the personal worker attaches under user:<tenant>/<hash>/<id>;
#   - an agent with workspace.roots opens the user's folder, reads the
#     user's file, runs a command that writes into it, is refused a
#     checkpoint (not_on_root), and lists the workspace with its root;
#   - the release leaves the folder and both files, and the worker's state
#     folder holds no copy of them;
#   - no answer the run stored names the folder's path;
#   - an agent without workspace.roots is refused (not_granted);
#   - retiring the token detaches the worker within a recheck.
# Needs python3 and sqlite3 on PATH; skips cleanly without them.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

for tool in python3 sqlite3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "SKIP — $tool is not on PATH"; exit 0; }
done

TEST_DIR="$(mktemp -d -t loomcycle-wspersonal.XXXXXX)"
PORT=18955
WPORT=18956
cleanup() {
  [[ -n "${WPID:-}" ]] && { kill "$WPID" 2>/dev/null || true; wait "$WPID" 2>/dev/null || true; }
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
BASE="http://127.0.0.1:$PORT"
DATA="$(cd "$TEST_DIR" && pwd -P)/data"
ROOT="$(cd "$TEST_DIR" && pwd -P)/state"
USERDIR="$(cd "$TEST_DIR" && pwd -P)/my-code"
VOL="$(cd "$TEST_DIR" && pwd -P)/vol"
mkdir -p "$ROOT" "$USERDIR" "$VOL"
echo "the user's own file" > "$USERDIR/keep.txt"
# shellcheck source=test/runtime/workspace/lib.sh
source "$SCRIPT_DIR/lib.sh"

cat > "$TEST_DIR/loomcycle.yaml" <<YAML
provider_priority: [mock]
tiers:
  middle:
    - { provider: mock, model: mock-generic }
conversion:
  mode: inprocess
worker_endpoint:
  listen: 127.0.0.1:$WPORT
workspace:
  mode: remote
  personal_workers: true
agents:
  wsroot:
    provider: code-js
    description: "A workspace on the user's own folder, through their personal worker."
    tools: [Workspace]
    workspace: { ops: [read, write, exec], open: true, roots: true }
  wsnoroot:
    provider: code-js
    description: "The same agent without the roots grant."
    tools: [Workspace]
    workspace: { ops: [read, write, exec], open: true }
YAML

echo "[1/6] build + boot (workspace.mode remote, personal_workers on, endpoint on loopback)"
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
for _ in $(seq 1 50); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && { READY=1; break; }
  kill -0 "$PID" 2>/dev/null || { echo "boot failed:"; cat "$TEST_DIR/boot.log"; exit 1; }
  sleep 0.2
done
[[ "$READY" == "1" ]] || { echo "not ready:"; cat "$TEST_DIR/boot.log"; exit 1; }

echo "[2/6] a user, and that user's personal worker token (per-user mint)"
admin /v1/_users '{"subject":"ws-user","access_mode":"isolated"}' >/dev/null
MINT=$(admin /v1/_users/ws-user/tokens '{"purpose":"personal_worker"}')
WTOKEN=$(echo "$MINT" | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
WDEF=$(echo "$MINT" | python3 -c 'import sys,json; print(json.load(sys.stdin)["def_id"])')
[[ -n "$WTOKEN" ]] || fail "no personal worker token minted"
echo "$MINT" | grep -q '"workers:attach"' || fail "the token's scopes are not [workers:attach]: $MINT"

echo "[3/6] the personal worker attaches, offering root code"
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
LOOMCYCLE_WORKER_TOKEN="$WTOKEN" \
LOOMCYCLE_WORKER_INSECURE=1 \
LOOMCYCLE_WORKSPACE_ROOT="$ROOT" \
  ./bin/loomcycle worker --role workspace --personal --root "code=$USERDIR" --id laptop --dispatcher "127.0.0.1:$WPORT" > "$TEST_DIR/worker.log" 2>&1 &
WPID=$!
attached() { grep -q "workspace worker attached.*worker_id=user:default/[0-9a-f]\{16\}/laptop" "$TEST_DIR/boot.log"; }
wait_until 60 attached || { cat "$TEST_DIR/worker.log"; fail "the personal worker did not attach under its user id"; }

echo "[4/6] wsroot: open the user's folder, read, write, checkpoint refused, list, release"
post_run "$TEST_DIR/root.sse" wsroot
R=$(sse_text "$TEST_DIR/root.sse")
echo "      $R"
for want in "state=ready" "root=code" "read=the user's own file" "exit=0" "checkpoint=not_on_root" "listed_root=code" "released=ok"; do
  echo "$R" | grep -q -- "$want" || fail "wsroot: want '$want' in: $R"
done
grep -q '^event: error$' "$TEST_DIR/root.sse" && fail "an EventError appeared on the run stream"
[[ -f "$USERDIR/keep.txt" ]] || fail "the user's file is gone after release"
grep -q made-by-agent "$USERDIR/made.txt" || fail "the command's file is not in the user's folder"
! grep -rqa "the user's own file" "$ROOT" || fail "the worker's state folder holds a copy of the user's file"
RUN_ID=$(sqlite3 "$DATA/loomcycle.db" "SELECT id FROM runs ORDER BY started_at DESC LIMIT 1;")
sqlite3 "$DATA/loomcycle.db" "SELECT CAST(payload AS TEXT) FROM events WHERE run_id='$RUN_ID';" > "$TEST_DIR/root-events.txt"
! grep -aqF "$USERDIR" "$TEST_DIR/root-events.txt" || fail "the folder's path reached the run's stored events"

echo "[5/6] an agent without workspace.roots is refused"
post_run "$TEST_DIR/noroot.sse" wsnoroot
sse_text "$TEST_DIR/noroot.sse" | grep -q 'open=not_granted' || fail "wsnoroot was not refused: $(sse_text "$TEST_DIR/noroot.sse")"

echo "[6/6] retiring the token detaches the worker within a recheck"
admin /v1/_operatortokendef "{\"op\":\"retire\",\"def_id\":\"$WDEF\"}" >/dev/null
detached() { grep -Eq "(token no longer attaches it|workspace worker detached).*worker_id=user:default/" "$TEST_DIR/boot.log"; }
wait_until 120 detached || fail "the retired token's worker is still attached"
[[ -f "$USERDIR/keep.txt" && -f "$USERDIR/made.txt" ]] || fail "the user's files are gone after the token was retired"
echo "=== workspace personal-worker suite PASSED ==="
