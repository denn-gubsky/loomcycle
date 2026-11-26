#!/usr/bin/env bash
# test/runtime/workspace-bundles/run.sh — the embedded bundle agents RFC GW
# P14 moved onto workspaces do their documented task on a workspace, with the
# shell tool they keep until P15 (Bash, Bashbox, the sandbox's tools) withheld.
# Deterministic: each bundle agent is turned into a code-js agent by an
# operator overlay that changes its provider and restates its tools minus the
# shell tool, so the run uses the bundle's OWN workspace grant (the overlay
# leaves `workspace:` alone; `tier: ""` clears the bundle's tier, which a
# provider pin may not sit beside). The real loop, the real local worker
# (workspace.mode local, host engine).
#
#   - team-examples sdlc/coder: fixes a bug in the `work` volume's checkout,
#     copies the checkout into a workspace, edits, copies the changed file
#     in again and runs its tests there;
#   - team-examples sdlc/reviewer: reads the diff with Git, and runs the tests
#     on its own workspace copy;
#   - agent-teams team/orchestrator, on the team/sqldata path: writes rows
#     into a workspace, totals them with a script and reads the report.
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
    tools: [Read, Write, Edit, Grep, Glob, Workspace, File, Context]
    volumes: [work]
  sdlc/reviewer:
    tier: ""
    provider: code-js
    tools: [Read, Grep, Glob, Git, Workspace, File, Context]
    volumes: [work]
  team/orchestrator:
    tier: ""
    provider: code-js
    tools: [Agent, Document, TeamDef, Channel, Interruption, Context, Memory, VolumeDef, Workspace, File]
YAML

echo "[1/4] build + boot (agent-teams + team-examples, workspace.mode local)"
go build -o bin/loomcycle ./cmd/loomcycle
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
LOOMCYCLE_PRESETS=agent-teams,team-examples \
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

echo "[2/4] sdlc/coder: fix in the volume, test in a workspace"
post_run "$TEST_DIR/coder.sse" sdlc/coder
R=$(sse_text "$TEST_DIR/coder.sse")
echo "      $R"
for want in "state=ready" "copied=yes" "tests=pass"; do
  echo "$R" | grep -q -- "$want" || fail "sdlc/coder: want '$want' in: $R"
done

echo "[3/4] sdlc/reviewer: Git diff, tests in its own workspace"
post_run "$TEST_DIR/reviewer.sse" sdlc/reviewer
R=$(sse_text "$TEST_DIR/reviewer.sse")
echo "      $R"
for want in "diff=seen" "tests=pass" "verdict=approve"; do
  echo "$R" | grep -q -- "$want" || fail "sdlc/reviewer: want '$want' in: $R"
done
echo "[4/4] team/orchestrator (team/sqldata): rows totalled in a workspace"
post_run "$TEST_DIR/orch.sse" team/orchestrator
R=$(sse_text "$TEST_DIR/orch.sse")
echo "      $R"
echo "$R" | grep -q -- "report=total=1545" || fail "team/orchestrator: want 'report=total=1545' in: $R"
grep -q '^event: error$' "$TEST_DIR/coder.sse" "$TEST_DIR/reviewer.sse" "$TEST_DIR/orch.sse" && fail "an EventError appeared on a run stream"
wait_until 10 no_workspaces || fail "workspace directories left after the runs ended: $(workspaces)"
echo "=== workspace-bundles runtime suite PASSED ==="
