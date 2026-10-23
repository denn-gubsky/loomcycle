#!/usr/bin/env bash
# test/runtime/git-tool/run.sh — the Git tool end to end through a booted
# server. Deterministic: code-js agents (no LLM, no provider key), a real
# volume, the real loop. Checks that an agent can init, commit and read a
# repository, that the commit is a real git commit with the operator's
# identity and the run trailer (read back with the git binary), and that
# the network is refused without git_remote, for an unlisted host, and for
# a non-https scheme, without any request leaving the process.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-gittool.XXXXXX)"
PORT=18947
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
BASE="http://127.0.0.1:$PORT"
VOL="$(cd "$TEST_DIR" && pwd -P)/vol"
mkdir -p "$VOL"
fail() { echo "FAIL ✗ — $1"; exit 1; }
command -v git >/dev/null || fail "git is needed to read the commit back"

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
{"agent":"$2","user_id":"git-user","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}
JSON
}

cat > "$TEST_DIR/loomcycle.yaml" <<YAML
provider_priority: [mock]
tiers:
  middle:
    - { provider: mock, model: mock-generic }
volumes:
  work: { path: "$VOL", mode: rw, default: true }
agents:
  gitflow:
    provider: code-js
    description: "Git tool end to end: init, write, commit, log; network refused."
    tools: [Git, Write]
    volumes: [work]
  gitnet:
    provider: code-js
    description: "Git tool network gate: an unlisted host and a non-https scheme."
    tools: [Git]
    volumes: [work]
    git_remote: [fetch]
YAML

echo "[1/4] build + boot (code-js, a git host list that does not include the asked-for host)"
go build -o bin/loomcycle ./cmd/loomcycle
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
LOOMCYCLE_MOCK_ENABLED=1 \
LOOMCYCLE_CODE_AGENTS_ENABLED=1 \
LOOMCYCLE_CODE_AGENTS_ROOT="$SCRIPT_DIR/agent_code" \
LOOMCYCLE_DATA_DIR="$TEST_DIR/data" \
LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$PORT" \
LOOMCYCLE_AUTH_TOKEN="$TOKEN" \
LOOMCYCLE_GIT_SOURCE_HOSTS="git.example.invalid" \
LOOMCYCLE_GIT_PUSH_HOSTS="git.example.invalid" \
LOOMCYCLE_GIT_AUTHOR_NAME="e2e-bot" \
LOOMCYCLE_GIT_AUTHOR_EMAIL="e2e-bot@example.invalid" \
  ./bin/loomcycle --config "$TEST_DIR/loomcycle.yaml" > "$TEST_DIR/boot.log" 2>&1 &
PID=$!
READY=0
for i in $(seq 1 50); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && { READY=1; break; }
  kill -0 "$PID" 2>/dev/null || { echo "boot failed:"; cat "$TEST_DIR/boot.log"; exit 1; }
  sleep 0.2
done
[[ "$READY" == "1" ]] || { echo "not ready:"; cat "$TEST_DIR/boot.log"; exit 1; }
grep -q "git source: enabled for git.example.invalid" "$TEST_DIR/boot.log" || fail "boot log does not report the git hosts"
grep -q "git: push enabled for git.example.invalid" "$TEST_DIR/boot.log" || fail "boot log does not report the push hosts"

echo "[2/4] gitflow: init, write, add, commit, log; ls_remote without git_remote"
post_run "$TEST_DIR/flow.sse" gitflow
R=$(sse_text "$TEST_DIR/flow.sse")
echo "      $R"
for want in "branch=trunk" "dirty=true" "commit=yes" "commits=1" "author=e2e-bot" "trailer=true" "net=git_remote_not_allowed"; do
  echo "$R" | grep -q -- "$want" || fail "gitflow: want '$want' in: $R"
done

echo "[3/4] the commit is real: read back with git"
GIT_LOG=$(git -C "$VOL/svc" log --format='%an <%ae>|%s|%(trailers:key=Loomcycle-Run,valueonly)' -1)
echo "      $GIT_LOG"
echo "$GIT_LOG" | grep -q '^e2e-bot <e2e-bot@example.invalid>|add hello|' || fail "git log: $GIT_LOG"
echo "$GIT_LOG" | grep -qE '\|[^|]+$' || fail "the commit has no Loomcycle-Run trailer: $GIT_LOG"
[[ "$(git -C "$VOL/svc" rev-parse --abbrev-ref HEAD)" == "trunk" ]] || fail "branch is not trunk"
git -C "$VOL/svc" fsck --no-progress >/dev/null 2>&1 || fail "git fsck failed on the repository the tool wrote"

echo "[4/4] gitnet: unlisted host and non-https scheme refused before any request"
post_run "$TEST_DIR/net.sse" gitnet
R=$(sse_text "$TEST_DIR/net.sse")
echo "      $R"
echo "$R" | grep -q "host=host_not_allowed" || fail "gitnet: an unlisted host was not refused: $R"
echo "$R" | grep -q "scheme=invalid_argument" || fail "gitnet: a file:// URL was not refused: $R"
if grep -q '^event: error$' "$TEST_DIR/flow.sse" "$TEST_DIR/net.sse"; then
  fail "an EventError appeared on a run stream"
fi
echo "=== git-tool runtime suite PASSED ==="
