#!/usr/bin/env bash
# test/runtime/workspace/egress.sh — a workspace's egress routes end to end
# (RFC GW P4): a booted runtime, real workers, deterministic code-js agents,
# no provider key, and the public registries through the `workspace` preset's
# routes.
#
# EGRESS_ENGINE=container (the CI job) runs two remote container-engine
# workers, one per tenant, each session on a network of its own whose only
# other member is its gateway (LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_DIR).
# EGRESS_ENGINE=host (the default) runs workspace.mode local with the host
# engine's loopback port (LOOMCYCLE_WORKSPACE_HOST_EGRESS=1). Checks that:
#   - pip installs a package from PyPI through `pypi` and `pypi-files`, its
#     files fetched through the route (the core's link rewrite), and imports
#     it; npm installs a package through `npm` with its integrity recorded;
#   - on the container engine, an agent granted `pypi` but not `pypi-files`
#     fails at once, the failure naming `pypi-files`; on both, a route the
#     workspace does not hold is refused;
#   - a tenant's credentialed route (EgressRouteDef + a restricted tenant
#     credential) to a local HTTPS upstream sends the tenant's token, and on
#     the container engine two tenants with a route of the same name each
#     reach their own upstream with their own token;
#   - the token canary: neither tenant's token is in the workers' disk,
#     environment or log, the sessions' environment, the run streams, the
#     store or the audit log, which holds one workspace_egress line per API
#     request with the path and no query.
# The API part needs port 443 on 127.0.0.1 (and 127.0.0.2) and SSL_CERT_FILE
# honoured (Linux); without them it is skipped, unless EGRESS_REQUIRE_API=1.
# Needs python3, openssl and network access to pypi.org and
# registry.npmjs.org; the container variant needs a Linux container daemon
# (Docker Desktop cannot connect a container to a host socket). It skips
# without them, unless LOOMCYCLE_REQUIRE_DOCKER=1. The image is
# WORKSPACE_IMAGE when set; otherwise it is built from Dockerfile.workspace.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

ENGINE="${EGRESS_ENGINE:-host}"
skip() {
  if [[ "${LOOMCYCLE_REQUIRE_DOCKER:-}" == 1 ]]; then echo "FAIL ✗ — $1"; exit 1; fi
  echo "SKIP — $1"; exit 0
}
for tool in python3 openssl curl; do
  command -v "$tool" >/dev/null 2>&1 || skip "$tool is not on PATH"
done
curl -fsS -o /dev/null --max-time 15 https://pypi.org/simple/six/ || skip "pypi.org is not reachable"
IMAGE=""
case "$ENGINE" in
  host) ;;
  container)
    command -v docker >/dev/null 2>&1 || skip "no docker on PATH"
    docker info --format '{{.ServerVersion}}' >/dev/null 2>&1 || skip "the container daemon does not answer"
    [[ "$(uname)" == Linux ]] || skip "container egress needs a Linux daemon"
    IMAGE="${WORKSPACE_IMAGE:-}"
    if [[ -z "$IMAGE" ]]; then
      IMAGE=loomcycle-workspace:e2e
      echo "[0] build the session image ($IMAGE) from Dockerfile.workspace"
      docker build -q -f Dockerfile.workspace -t "$IMAGE" . >/dev/null
    fi
    ;;
  *) echo "EGRESS_ENGINE must be host or container"; exit 2 ;;
esac

TEST_DIR="$(cd "$(mktemp -d -t loomcycle-wsegress.XXXXXX)" && pwd -P)"
PORT=18961
WPORT=18962
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
ROOT="$DATA/workspaces"
VOL="$TEST_DIR/vol"
mkdir -p "$VOL"
# shellcheck source=test/runtime/workspace/lib.sh
source "$SCRIPT_DIR/lib.sh"

# The tokens the API routes send: random, so a match anywhere is a leak.
CANARY1="canary1-$(python3 -I -c 'import secrets; print(secrets.token_hex(16))')"
CANARY2="canary2-$(python3 -I -c 'import secrets; print(secrets.token_hex(16))')"

# --- the API upstreams --------------------------------------------------------
API=0
bind443() { python3 -I -c 'import socket,sys; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind((sys.argv[1], 443))' "$1" 2>/dev/null; }
if [[ "$(uname)" == Linux ]] && bind443 127.0.0.1 && { [[ "$ENGINE" == host ]] || bind443 127.0.0.2; }; then
  API=1
elif [[ "${EGRESS_REQUIRE_API:-}" == 1 ]]; then
  fail "the API part needs port 443 on 127.0.0.1 and 127.0.0.2 and Linux (sysctl net.ipv4.ip_unprivileged_port_start=0)"
else
  echo "      the API part is skipped: it needs port 443 on loopback and Linux's SSL_CERT_FILE"
fi
CORE_ENV=()
if [[ "$API" == 1 ]]; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=loomcycle-egress-e2e \
    -addext "subjectAltName=IP:127.0.0.1,IP:127.0.0.2" \
    -keyout "$TEST_DIR/up.key" -out "$TEST_DIR/up.crt" 2>/dev/null
  # The core trusts the system roots (pypi.org, npm) and this certificate.
  SYSTEM_ROOTS=""
  for f in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt; do
    [[ -f "$f" ]] && { SYSTEM_ROOTS="$f"; break; }
  done
  [[ -n "$SYSTEM_ROOTS" ]] || fail "no system CA bundle found"
  cat "$SYSTEM_ROOTS" "$TEST_DIR/up.crt" > "$TEST_DIR/roots.pem"
  CORE_ENV+=("SSL_CERT_FILE=$TEST_DIR/roots.pem")
  start_upstream() { # name addr
    python3 -I "$SCRIPT_DIR/egress/upstream.py" "$1" "$2" 443 "$TEST_DIR/up.crt" "$TEST_DIR/up.key" "$TEST_DIR/$1.requests" > "$TEST_DIR/$1.out" 2>&1 &
    PIDS+=($!)
    wait_until 10 grep -q ready "$TEST_DIR/$1.out" || fail "upstream $1 did not start: $(cat "$TEST_DIR/$1.out")"
  }
  start_upstream upstream1 127.0.0.1
  [[ "$ENGINE" == container ]] && start_upstream upstream2 127.0.0.2
fi

# --- the runtime ------------------------------------------------------------
GRANT_ENGINE=""
MODE=local
ENDPOINT=""
if [[ "$ENGINE" == container ]]; then
  GRANT_ENGINE=", engine: container"
  MODE=remote
  ENDPOINT="worker_endpoint: { listen: 127.0.0.1:$WPORT }"
fi
CODE="$(sed 's/^/      /' "$SCRIPT_DIR/egress/agent.js")"
cat > "$TEST_DIR/loomcycle.yaml" <<YAML
provider_priority: [mock]
tiers:
  middle:
    - { provider: mock, model: mock-generic }
conversion:
  mode: inprocess
$ENDPOINT
workspace:
  mode: $MODE
  egress:
    routes:
      pypi:       { url: "https://pypi.org/" }
      pypi-files: { url: "https://files.pythonhosted.org/" }
      npm:        { url: "https://registry.npmjs.org/" }
    tenant_routes: true
agents:
  wsegress:
    provider: code-js
    description: "Egress end to end: every route."
    tools: [Workspace]
    workspace: { ops: [read, write, exec], open: true, egress: [pypi, pypi-files, npm, crm]$GRANT_ENGINE }
    code: |
$CODE
  wsegressnf:
    provider: code-js
    description: "Egress end to end: pypi without pypi-files."
    tools: [Workspace]
    workspace: { ops: [read, write, exec], open: true, egress: [pypi, npm]$GRANT_ENGINE }
    code: |
$CODE
YAML

echo "[1/7] build + boot (engine $ENGINE, workspace.mode $MODE)"
go build -o bin/loomcycle ./cmd/loomcycle
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
LOOMCYCLE_MOCK_ENABLED=1 \
LOOMCYCLE_DATA_DIR="$DATA" \
LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$PORT" \
LOOMCYCLE_AUTH_TOKEN="$TOKEN" \
LOOMCYCLE_SECRET_KEY="$(python3 -I -c 'import base64,os; print(base64.b64encode(os.urandom(32)).decode())')" \
LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST="127.0.0.1/32,127.0.0.2/32" \
LOOMCYCLE_AUDIT_LOG_PATH="$TEST_DIR/audit.jsonl" \
LOOMCYCLE_WORKSPACE_HOST_EGRESS=1 \
${CORE_ENV[@]+"${CORE_ENV[@]}"} \
  ./bin/loomcycle --config "$TEST_DIR/loomcycle.yaml" > "$TEST_DIR/boot.log" 2>&1 &
PID=$!
READY=0
for _ in $(seq 1 100); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && { READY=1; break; }
  kill -0 "$PID" 2>/dev/null || { echo "boot failed:"; cat "$TEST_DIR/boot.log"; exit 1; }
  sleep 0.2
done
[[ "$READY" == "1" ]] || { echo "not ready:"; cat "$TEST_DIR/boot.log"; exit 1; }

# api POSTs a JSON body to a /v1 path with a bearer and prints the answer.
api() { curl -fsS -X POST -H "Authorization: Bearer $1" -H "Content-Type: application/json" -d "$3" "$BASE$2"; }
mint() { # tenant subject scopes-json
  api "$TOKEN" /v1/_operatortokendef "{\"op\":\"create\",\"name\":\"$2\",\"tenant_id\":\"$1\",\"subject\":\"$2\",\"scopes\":$3}" |
    python3 -I -c 'import sys,json; print(json.load(sys.stdin)["token"])'
}
WPIDS_FILE="$TEST_DIR/worker.pids"
: > "$WPIDS_FILE"
if [[ "$ENGINE" == container ]]; then
  echo "[2/7] two tenants, a container-engine worker for each, egress on"
  T1_TOKEN=$(mint t1 op1 '["substrate:tenant","runs:create","runs:read"]')
  T2_TOKEN=$(mint t2 op2 '["substrate:tenant","runs:create","runs:read"]')
  start_worker() { # tenant id root egressdir
    local wt; wt=$(mint "$1" "w-$2" '["workers:attach"]')
    mkdir -p "$3"
    env -i PATH="$PATH" HOME="$HOME" \
    LOOMCYCLE_WORKER_TOKEN="$wt" \
    LOOMCYCLE_WORKER_INSECURE=1 \
    LOOMCYCLE_WORKSPACE_ROOT="$3" \
    LOOMCYCLE_WORKSPACE_ENGINE=container \
    LOOMCYCLE_WORKSPACE_CONTAINER_IMAGE="$IMAGE" \
    LOOMCYCLE_WORKSPACE_CONTAINER_MEMORY_MB=1024 \
    LOOMCYCLE_WORKSPACE_CONTAINER_EGRESS_DIR="$4" \
      ./bin/loomcycle worker --role workspace --id "$2" --dispatcher "127.0.0.1:$WPORT" > "$TEST_DIR/worker-$2.log" 2>&1 &
    PIDS+=($!)
    echo "$!" >> "$WPIDS_FILE"
    attached() { grep -q "workspace worker attached.*worker_id=tenant:$1/$2.*engine=container" "$TEST_DIR/boot.log"; }
    wait_until 300 attached "$1" "$2" || { cat "$TEST_DIR/worker-$2.log"; fail "worker $2 of $1 did not attach"; }
  }
  # Socket paths are short (at most 100 bytes in all): /tmp, not TEST_DIR.
  EGDIR1="$(mktemp -d /tmp/lcg1.XXXX)"
  EGDIR2="$(mktemp -d /tmp/lcg2.XXXX)"
  start_worker t1 e1 "$TEST_DIR/ws1" "$EGDIR1"
  start_worker t2 e2 "$TEST_DIR/ws2" "$EGDIR2"
  RUN_TOKEN="$T1_TOKEN"
  RUN_USER=op1
  WORKER_DIRS=("$TEST_DIR/ws1" "$TEST_DIR/ws2" "$EGDIR1" "$EGDIR2")
else
  echo "[2/7] the local worker (host engine, loopback egress port)"
  local_attached() { grep -q "workspace worker attached.*engine=host" "$TEST_DIR/boot.log"; }
  wait_until 60 local_attached || fail "the local worker did not attach: $(cat "$TEST_DIR/boot.log")"
  # The local worker is the runtime's child; its pid is in its log line.
  pgrep -f "worker --role workspace" -P "$PID" >> "$WPIDS_FILE" || true
  T1_TOKEN="$TOKEN"
  RUN_TOKEN="$TOKEN"
  RUN_USER=ws-user
  WORKER_DIRS=("$ROOT")
fi

# run_cmd runs the command in file $3 with agent $2 under token $4 (user $5)
# and leaves the agent's report in $1.json.
run_cmd() {
  python3 -I - "$2" "$3" "$5" > "$TEST_DIR/$1.req" <<'PY'
import json, sys
agent, path, user = sys.argv[1:4]
print(json.dumps({"agent": agent, "user_id": user,
  "segments": [{"role": "user", "content": [{"type": "trusted-text", "text": open(path).read()}]}]}))
PY
  curl -fsS -N -H "Authorization: Bearer $4" -H "Content-Type: application/json" -H "Accept: text/event-stream" \
    -d @"$TEST_DIR/$1.req" "$BASE/v1/runs" > "$TEST_DIR/$1.sse" || fail "$1: the run request failed"
  sse_text "$TEST_DIR/$1.sse" > "$TEST_DIR/$1.json"
  python3 -I -c 'import json,sys; json.load(open(sys.argv[1]))' "$TEST_DIR/$1.json" 2>/dev/null ||
    fail "$1: no report from the agent: $(cat "$TEST_DIR/$1.sse" | tail -20)"
}
# field prints one field of a report.
field() { python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$TEST_DIR/$1.json" "$2"; }
want_in() { # report text
  field "$1" output | grep -qF -- "$2" || fail "$1: want '$2' in the output: $(field "$1" output)"
}

# Commands print the session's egress base as EGRESS/, so a check reads the
# same on both engines (and the host port's secret stays out of the output).
cat > "$TEST_DIR/pip.cmd" <<'SH'
export HOME="$PWD"
norm() { sed -E -e "s#${LOOMCYCLE_EGRESS}#EGRESS/#g" -e 's#http://egress/#EGRESS/#g'; }
python3 -m venv v >/dev/null
v/bin/pip install -v --no-cache-dir six==1.16.0 > pip.log 2>&1; echo "pip_rc=$?"
grep -E 'Downloading|Successfully installed' pip.log | norm
v/bin/python -c 'import six; print("imported six", six.__version__)'
SH
cat > "$TEST_DIR/npm.cmd" <<'SH'
export HOME="$PWD"
mkdir -p n && cd n && npm init -y >/dev/null
npm install --no-fund left-pad@1.3.0 > npm.log 2>&1; echo "npm_rc=$?"
grep -c '"integrity"' package-lock.json | sed 's/^/integrity_entries=/'
node -e 'console.log("left-pad", require("left-pad")("7", 3, "0"))'
SH
cat > "$TEST_DIR/nofiles.cmd" <<'SH'
export HOME="$PWD"
norm() { sed -E -e "s#${LOOMCYCLE_EGRESS}#EGRESS/#g" -e 's#http://egress/#EGRESS/#g'; }
python3 -m venv v >/dev/null
start=$(date +%s)
# perl's alarm, not timeout(1): macOS has no timeout. A kill by it is rc 142.
perl -e 'alarm shift; exec @ARGV' 300 v/bin/pip download --no-cache-dir --no-deps --retries 0 -d dl six==1.16.0 > pip.log 2>&1; echo "pip_rc=$?"
echo "seconds=$(( $(date +%s) - start ))"
grep -iE '403|error' pip.log | norm | head -5
SH
cat > "$TEST_DIR/route.cmd" <<'SH'
python3 -c '
import os, sys, urllib.request, urllib.error
for path in sys.argv[1:]:
    try:
        r = urllib.request.urlopen(os.environ["LOOMCYCLE_EGRESS"] + path, timeout=60)
        print(path, r.status, r.read().decode())
    except urllib.error.HTTPError as e:
        print(path, e.code, e.read().decode().strip())
' crm/deals?since=2026 nope/x
env | sed -E 's#^(LOOMCYCLE_EGRESS=).*#\1EGRESS#'
SH

echo "[3/7] pip install through pypi and pypi-files"
run_cmd pip wsegress "$TEST_DIR/pip.cmd" "$RUN_TOKEN" "$RUN_USER"
[[ "$(field pip engine)" == "$ENGINE" ]] || fail "pip: engine $(field pip engine), want $ENGINE"
want_in pip "pip_rc=0"
want_in pip "EGRESS/pypi-files/packages/"
want_in pip "imported six 1.16.0"
field pip output | grep -q "files.pythonhosted.org/packages" && fail "pip fetched a file past the route: $(field pip output)"

echo "[4/7] npm install through npm"
run_cmd npm wsegress "$TEST_DIR/npm.cmd" "$RUN_TOKEN" "$RUN_USER"
want_in npm "npm_rc=0"
want_in npm "left-pad 007"
field npm output | grep -q "integrity_entries=[1-9]" || fail "npm: no integrity in the lockfile: $(field npm output)"

echo "[5/7] without pypi-files pip fails at once, naming the route"
if [[ "$ENGINE" == container ]]; then
  run_cmd nofiles wsegressnf "$TEST_DIR/nofiles.cmd" "$RUN_TOKEN" "$RUN_USER"
  field nofiles output | grep -q "pip_rc=0" && fail "nofiles: pip succeeded without pypi-files"
  field nofiles output | grep -q "pip_rc=142" && fail "nofiles: pip hung until the timeout"
  want_in nofiles "pypi-files"
  secs=$(field nofiles output | sed -n 's/^seconds=//p')
  [[ -n "$secs" && "$secs" -lt 120 ]] || fail "nofiles: pip took ${secs:-?}s to fail"
else
  # The host engine's commands have the host's network: without the files
  # route the index is not rewritten and pip fetches the files directly.
  echo "      skipped: the host engine is not a boundary"
fi

echo "[6/7] the tenant's credentialed route, and a route the workspace does not hold"
if [[ "$API" == 1 ]]; then
  route_for() { # token addr canary
    api "$1" /v1/_credentialdef "{\"op\":\"create\",\"name\":\"crm_token\",\"scope\":\"tenant\",\"value\":\"$3\",\"allowed_urls\":[\"https://$2/api\"]}" > /dev/null ||
      fail "creating the tenant credential failed"
    api "$1" /v1/_egressroutedef "{\"op\":\"create\",\"name\":\"crm\",\"url\":\"https://$2/api/\",\"credential\":\"\$cred:crm_token\"}" > /dev/null ||
      fail "creating the tenant route failed"
  }
  route_for "$T1_TOKEN" 127.0.0.1 "$CANARY1"
  run_cmd route1 wsegress "$TEST_DIR/route.cmd" "$RUN_TOKEN" "$RUN_USER"
  want_in route1 "crm/deals?since=2026 200 upstream1 /api/deals?since=2026"
  grep -qF "Bearer $CANARY1" "$TEST_DIR/upstream1.requests" || fail "upstream1 did not receive tenant 1's token"
  if [[ "$ENGINE" == container ]]; then
    route_for "$T2_TOKEN" 127.0.0.2 "$CANARY2"
    run_cmd route2 wsegress "$TEST_DIR/route.cmd" "$T2_TOKEN" op2
    want_in route2 "crm/deals?since=2026 200 upstream2 /api/deals?since=2026"
    grep -qF "Bearer $CANARY2" "$TEST_DIR/upstream2.requests" || fail "upstream2 did not receive tenant 2's token"
    grep -qF "$CANARY2" "$TEST_DIR/upstream1.requests" && fail "tenant 2's token reached tenant 1's upstream"
    grep -qF "$CANARY1" "$TEST_DIR/upstream2.requests" && fail "tenant 1's token reached tenant 2's upstream"
    [[ "$(wc -l < "$TEST_DIR/upstream1.requests")" == 1 ]] || fail "upstream1 got requests from another tenant: $(cat "$TEST_DIR/upstream1.requests")"
  fi
else
  run_cmd route1 wsegress "$TEST_DIR/route.cmd" "$RUN_TOKEN" "$RUN_USER"
fi
want_in route1 "nope/x 403"
want_in route1 'route "nope" is not one this workspace holds'

echo "[7/7] the token canary and the audit log"
if [[ "$API" == 1 ]]; then
  for c in "$CANARY1" "$CANARY2"; do
    for f in "$TEST_DIR"/*.sse "$TEST_DIR"/*.json "$TEST_DIR"/*.log "$TEST_DIR/audit.jsonl"; do
      [[ -f "$f" ]] && grep -qaF -- "$c" "$f" && fail "a token is in $f"
    done
    grep -rqaF -- "$c" "$DATA" "${WORKER_DIRS[@]}" 2>/dev/null && fail "a token is on disk under the store or a worker: $(grep -rlaF -- "$c" "$DATA" "${WORKER_DIRS[@]}")"
    while read -r wp; do
      [[ -r "/proc/$wp/environ" ]] && tr '\0' '\n' < "/proc/$wp/environ" | grep -qF -- "$c" && fail "a token is in worker $wp's environment"
    done < "$WPIDS_FILE"
  done
  python3 -I - "$TEST_DIR/audit.jsonl" "$ENGINE" <<'PY' || fail "the audit log: $(grep workspace_egress "$TEST_DIR/audit.jsonl" || true)"
import json, sys
path, engine = sys.argv[1:3]
lines = [json.loads(l) for l in open(path) if '"workspace_egress"' in l]
crm = [l for l in lines if l.get("egress_route") == "crm"]
want = {"t1"} if engine == "container" else {"default"}
if engine == "container":
    want.add("t2")
tenants = {l.get("target_tenant") for l in crm}
assert tenants == want, f"tenants {tenants}, want {want}"
for l in crm:
    assert l.get("egress_path") == "deals", l
    assert l.get("egress_status") == 200, l
    assert "since" not in json.dumps(l), l
print("      audit: %d workspace_egress lines for crm" % len(crm))
PY
else
  echo "      skipped with the API part"
fi
echo "=== workspace egress suite ($ENGINE) PASSED ==="
