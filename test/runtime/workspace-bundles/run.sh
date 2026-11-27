#!/usr/bin/env bash
# test/runtime/workspace-bundles/run.sh — the embedded bundle agents RFC GW
# P14 moved onto workspaces do their documented task on a workspace, the only
# place they run a command since P15 removed Bash and Bashbox.
# Deterministic: each bundle agent is turned into a code-js agent by an
# operator overlay that changes only its provider, so the run uses the
# bundle's OWN tools and workspace grant (the overlay leaves `tools:` and
# `workspace:` alone; `tier: ""` clears the bundle's tier, which a provider
# pin may not sit beside). The real loop, the real local worker
# (workspace.mode local, host engine).
#
#   - team-examples sdlc/coder: fixes a bug in the `work` volume's checkout,
#     copies the checkout into a workspace, edits, copies the changed file
#     in again and runs its tests there;
#   - team-examples sdlc/reviewer: reads the diff with Git, and runs the tests
#     on its own workspace copy;
#   - agent-teams team/orchestrator, on the team/sqldata path: writes rows
#     into a workspace, totals them with a script and reads the report;
#   - dev-workspace dev/exec (already code-js, run as shipped): runs a command
#     envelope in a workspace, a failing command stopping the rest;
#   - chat chat/medium: writes a script into a workspace and runs it.
#
# Needs python3 and git on PATH; the suite skips cleanly without them.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

for bin in python3 git; do
  command -v "$bin" >/dev/null 2>&1 || { echo "SKIP — $bin is not on PATH"; exit 0; }
done

TEST_DIR="$(mktemp -d -t loomcycle-wsbundles.XXXXXX)"
PORT=18961
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
BASE="http://127.0.0.1:$PORT"
DATA="$(cd "$TEST_DIR" && pwd -P)/data"
ROOT="$DATA/workspaces"
VOL="$(cd "$TEST_DIR" && pwd -P)/vol"
mkdir -p "$VOL"
# shellcheck source=test/runtime/workspace/lib.sh
source "$REPO_ROOT/test/runtime/workspace/lib.sh"

# A checkout with a bug and a test that finds it, committed so the reviewer's
# diff shows the coder's fix.
printf 'def add(a, b):\n    return a - b\n' > "$VOL/calc.py"
printf 'import unittest\nfrom calc import add\n\nclass T(unittest.TestCase):\n    def test_add(self):\n        self.assertEqual(add(2, 3), 5)\n' > "$VOL/test_calc.py"
git -C "$VOL" init -q -b main
git -C "$VOL" -c user.email=t@example.com -c user.name=t add -A
git -C "$VOL" -c user.email=t@example.com -c user.name=t commit -q -m "a bug"

cat > "$TEST_DIR/loomcycle.yaml" <<YAML
provider_priority: [mock]
tiers:
  middle:
    - { provider: mock, model: mock-generic }
workspace:
  mode: local
volumes:
  work: { path: "$VOL", mode: rw }
agents:
  sdlc/coder:
    tier: ""
    provider: code-js
    volumes: [work]
  sdlc/reviewer:
    tier: ""
    provider: code-js
    volumes: [work]
  chat/medium:
    tier: ""
    provider: code-js
    # The bundle's `context: {mode: auto}` resolves a non-interactive run on a
    # non-local provider to the stateful loop, which binds no tools in code-js;
    # recap is what the chat agents get in the interactive terminal.
    context: {mode: recap}
  team/orchestrator:
    tier: ""
    provider: code-js
YAML

echo "[1/6] build + boot (agent-teams + team-examples + dev-workspace + chat, workspace.mode local)"
go build -o bin/loomcycle ./cmd/loomcycle
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
LOOMCYCLE_PRESETS=agent-teams,team-examples,dev-workspace,chat \
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
wait_until 15 grep -q "workspace worker attached" "$TEST_DIR/boot.log" || fail "the local worker did not attach"

echo "[2/6] sdlc/coder: fix in the volume, test in a workspace"
post_run "$TEST_DIR/coder.sse" sdlc/coder
R=$(sse_text "$TEST_DIR/coder.sse")
echo "      $R"
for want in "state=ready" "copied=yes" "tests=pass"; do
  echo "$R" | grep -q -- "$want" || fail "sdlc/coder: want '$want' in: $R"
done

echo "[3/6] sdlc/reviewer: Git diff, tests in its own workspace"
post_run "$TEST_DIR/reviewer.sse" sdlc/reviewer
R=$(sse_text "$TEST_DIR/reviewer.sse")
echo "      $R"
for want in "diff=seen" "tests=pass" "verdict=approve"; do
  echo "$R" | grep -q -- "$want" || fail "sdlc/reviewer: want '$want' in: $R"
done
echo "[4/6] team/orchestrator (team/sqldata): rows totalled in a workspace"
post_run "$TEST_DIR/orch.sse" team/orchestrator
R=$(sse_text "$TEST_DIR/orch.sse")
echo "      $R"
echo "$R" | grep -q -- "report=total=1545" || fail "team/orchestrator: want 'report=total=1545' in: $R"
echo "[5/6] dev/exec (dev-workspace): a command envelope in a workspace"
# post_run takes plain text; the envelope is JSON, so the request body is built
# with python to escape it.
envelope_run() {
  python3 -I -c 'import json,sys; print(json.dumps({"agent":"dev/exec","user_id":"ws-user","segments":[{"role":"user","content":[{"type":"trusted-text","text":sys.argv[1]}]}]}))' "$2" |
    curl -fsS -N -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
      -H "Accept: text/event-stream" -d @- "$BASE/v1/runs" > "$1"
}
envelope_run "$TEST_DIR/exec.sse" '{"files":[{"path":"sq.py","content":"import sys\nprint(int(sys.argv[1]) ** 2)\n"}],"commands":["python3 sq.py 12","false","echo never"],"read":["sq.py"],"keep_open":true}'
R=$(sse_text "$TEST_DIR/exec.sse")
echo "$R" | sed 's/^/      /'
echo "$R" | grep -q "hint" && fail "dev/exec: a failed command's report carries the tool's help text: $R"
for want in "1 ok, 1 failed, 1 skipped" "144" "[exit: 1]" "SKIPPED" "# read sq.py" "ignored (a workspace has no such setting): keep_open"; do
  echo "$R" | grep -qF -- "$want" || fail "dev/exec: want '$want' in: $R"
done
echo "[6/6] chat/medium: a script run in a workspace"
post_run "$TEST_DIR/chat.sse" chat/medium
R=$(sse_text "$TEST_DIR/chat.sse")
echo "      $R"
echo "$R" | grep -q -- "state=ready fib20=6765" || fail "chat/medium: want 'state=ready fib20=6765' in: $R"
grep -q '^event: error$' "$TEST_DIR/coder.sse" "$TEST_DIR/reviewer.sse" "$TEST_DIR/orch.sse" "$TEST_DIR/exec.sse" "$TEST_DIR/chat.sse" && fail "an EventError appeared on a run stream"
wait_until 10 no_workspaces || fail "workspace directories left after the runs ended: $(workspaces)"
echo "=== workspace-bundles runtime suite PASSED ==="
