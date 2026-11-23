#!/usr/bin/env bash
# test/runtime/workspace/container.sh — the workspace suite on the container
# engine (RFC GW P3), end to end: a booted runtime in workspace.mode remote, a
# workers:attach token minted through the token API, and a remote worker
# (`loomcycle worker --role workspace`) on LOOMCYCLE_WORKSPACE_ENGINE=container
# with the workspace session image (Dockerfile.workspace). Deterministic:
# code-js agents, no provider key. Checks that:
#   - an agent whose grant requires `engine: container` is placed on the
#     worker, writes a Python script, runs it in its session and reads what
#     it wrote; the run's end removes the directory and the session;
#   - the data pass of run.sh (render, a script, import, render to a volume,
#     copy, extract, rm) runs with the commands in the session: the script
#     sees the fixed /workspace, no file's bytes reach a tool result.
# Needs a container daemon (docker) and python3 and sqlite3 on PATH. It
# skips without a daemon, unless LOOMCYCLE_REQUIRE_DOCKER=1, which makes that
# a failure. The image is WORKSPACE_IMAGE when set; otherwise it is built
# from Dockerfile.workspace (a few minutes, about 2 GB).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

skip() {
  if [[ "${LOOMCYCLE_REQUIRE_DOCKER:-}" == 1 ]]; then echo "FAIL ✗ — $1"; exit 1; fi
  echo "SKIP — $1"; exit 0
}
command -v docker >/dev/null 2>&1 || skip "no docker on PATH"
docker info --format '{{.ServerVersion}}' >/dev/null 2>&1 || skip "the container daemon does not answer"
for tool in python3 sqlite3; do
  command -v "$tool" >/dev/null 2>&1 || skip "$tool is not on PATH"
done

IMAGE="${WORKSPACE_IMAGE:-}"
if [[ -z "$IMAGE" ]]; then
  IMAGE=loomcycle-workspace:e2e
  echo "[0/5] build the session image ($IMAGE) from Dockerfile.workspace"
  docker build -q -f Dockerfile.workspace -t "$IMAGE" . >/dev/null
fi

TEST_DIR="$(mktemp -d -t loomcycle-wscontainer.XXXXXX)"
PORT=18953
WPORT=18954
cleanup() {
  [[ -n "${WPID:-}" ]] && { kill "$WPID" 2>/dev/null || true; wait "$WPID" 2>/dev/null || true; }
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
BASE="http://127.0.0.1:$PORT"
DATA="$(cd "$TEST_DIR" && pwd -P)/data"
ROOT="$(cd "$TEST_DIR" && pwd -P)/workspaces"
VOL="$(cd "$TEST_DIR" && pwd -P)/vol"
mkdir -p "$VOL" "$ROOT"
WS_ENGINE=container
# shellcheck source=test/runtime/workspace/lib.sh
source "$SCRIPT_DIR/lib.sh"
# KEY is the label the worker puts on its sessions: engine.WorkerKey of its
# id and root, so other workers' sessions on this daemon are not counted.
KEY=$(python3 -I -c 'import hashlib,sys; print(hashlib.sha256(sys.argv[1].encode()+b"\0"+sys.argv[2].encode()).hexdigest()[:12])' e2e "$ROOT")
# sessions counts the session containers of the worker under test.
sessions() { docker ps -aq --filter "label=loomcycle.workspace.worker=$KEY" | wc -l | tr -d ' '; }
no_sessions() { [[ "$(sessions)" == 0 ]]; }

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
volumes:
  work: { path: "$VOL", mode: rw }
agents:
  wsflow:
    provider: code-js
    description: "Workspace end to end on the container engine."
    tools: [Workspace]
    workspace: { ops: [read, write, exec], open: true, engine: container }
  wsdata:
    provider: code-js
    description: "The data pass on the container engine."
    tools: [Workspace, File, Document, Read, Write]
    workspace: { ops: [read, write, exec], open: true, engine: container }
    volumes: [work]
YAML

echo "[1/5] build + boot (workspace.mode remote)"
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

echo "[2/5] mint a workers:attach token and start a container-engine worker"
# The legacy token's runs are of tenant "default"; the worker serves it.
WTOKEN=$(curl -fsS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"op":"create","name":"ws-e2e","tenant_id":"default","subject":"ws-e2e","scopes":["workers:attach"]}' \
  "$BASE/v1/_operatortokendef" | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
[[ -n "$WTOKEN" ]] || fail "no token minted"
# The worker's own environment: HOME is the real one so the docker CLI finds
# its context (Docker Desktop keeps it under ~/.docker).
env -i PATH="$PATH" HOME="$HOME" \
LOOMCYCLE_WORKER_TOKEN="$WTOKEN" \
LOOMCYCLE_WORKER_INSECURE=1 \
LOOMCYCLE_WORKSPACE_ROOT="$ROOT" \
LOOMCYCLE_WORKSPACE_ENGINE=container \
LOOMCYCLE_WORKSPACE_CONTAINER_IMAGE="$IMAGE" \
LOOMCYCLE_WORKSPACE_CONTAINER_MEMORY_MB=1024 \
  ./bin/loomcycle worker --role workspace --id e2e --dispatcher "127.0.0.1:$WPORT" > "$TEST_DIR/worker.log" 2>&1 &
WPID=$!
attached() { grep -q "workspace worker attached.*worker_id=tenant:default/e2e.*engine=container" "$TEST_DIR/boot.log"; }
wait_until 300 attached || { cat "$TEST_DIR/worker.log"; fail "the container-engine worker did not attach"; }
grep -q 'toolchains="\[python node\]"' "$TEST_DIR/worker.log" || fail "the worker did not report the image's toolchains: $(cat "$TEST_DIR/worker.log")"

echo "[3/5] wsflow: open on the container engine, write square.py, run it, read out.txt"
post_run "$TEST_DIR/flow.sse" wsflow
R=$(sse_text "$TEST_DIR/flow.sse")
echo "      $R"
for want in "state=ready" "engine=container" "exit=0" "printed=squared_12" "result=144" "listed=1"; do
  echo "$R" | grep -q -- "$want" || fail "wsflow: want '$want' in: $R"
done
grep -q '^event: error$' "$TEST_DIR/flow.sse" && fail "an EventError appeared on the run stream"

echo "[4/5] the run's end removed the workspace and its session"
wait_until 30 no_workspaces || fail "workspace directories left after the run ended: $(workspaces)"
wait_until 30 no_sessions || fail "session containers left after the run ended: $(sessions)"

echo "[5/5] the data pass, its commands in the session"
data_pass
wait_until 30 no_sessions || fail "session containers left after the data pass ended: $(sessions)"
echo "=== workspace container-engine suite PASSED ==="
