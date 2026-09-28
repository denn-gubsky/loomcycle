#!/usr/bin/env bash
# test/runtime/channel-hooks/run.sh — channel hooks end to end, deterministic
# (a code-js HookDef, a code-js publishing agent; no LLM, no key, no Postgres).
#
#   - a publish to a hooked channel returns awaiting_hooks and is not visible
#     until the hook decides, from HTTP and from an agent's Channel tool
#   - the hook redacts, drops and releases; readers see exactly that
#   - decisions are recorded on the tenant's decisions channel and counted in
#     /metrics
#   - after a kill -9 with messages still waiting, a restart decides each one
#     exactly once
#   - with channel hooks off, a publish to the hooked channel is refused
#
# Open mode (no LOOMCYCLE_AUTH_TOKEN), deliberately: the yaml channel's hooks
# resolve in the operator's own tenant (""), which is where an open-mode
# operator writes its HookDefs. The legacy shared token writes them in tenant
# "default", where operator yaml does not look.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

TEST_DIR="$(mktemp -d -t loomcycle-chooks.XXXXXX)"
PORT=18943
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo; echo "Test dir kept for inspection: $TEST_DIR"
}
trap cleanup EXIT INT TERM

TOKEN="test-token-$(date +%s)"
BASE="http://127.0.0.1:$PORT"
fail() { echo "FAIL ✗ — $1"; [[ -f "$TEST_DIR/boot.log" ]] && tail -20 "$TEST_DIR/boot.log"; exit 1; }
api() { curl -sS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" "$@"; }

boot() { # $1 = LOOMCYCLE_CHANNEL_HOOKS value
  LOOMCYCLE_MOCK_ENABLED=1 \
  LOOMCYCLE_CODE_AGENTS_ENABLED=1 \
  LOOMCYCLE_CODE_AGENTS_ROOT="$SCRIPT_DIR/agent_code" \
  LOOMCYCLE_CODE_HOOKS_ENABLED=1 \
  LOOMCYCLE_CHANNEL_HOOKS="$1" \
  LOOMCYCLE_DATA_DIR="$TEST_DIR/data" \
  LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$PORT" \
    ./bin/loomcycle --config "$SCRIPT_DIR/loomcycle.yaml" >> "$TEST_DIR/boot.log" 2>&1 &
  PID=$!
  for i in $(seq 1 50); do
    curl -fsS "$BASE/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$PID" 2>/dev/null || fail "boot failed"
    sleep 0.2
  done
  fail "not ready"
}
publish() { api -X POST "$BASE/v1/_channels/inbox/publish" -d "{\"payload\":$1}"; }
peek_inbox() { api "$BASE/v1/_channels/inbox/peek?scope=global&max_messages=100"; }
count() { python3 -c "import sys,json; print(len(json.load(sys.stdin).get('messages') or []))"; }
wait_for_count() { # $1 = expected visible count, $2 = tenths of a second to wait (default 100)
  for i in $(seq 1 "${2:-100}"); do
    [[ "$(peek_inbox | count)" == "$1" ]] && return 0
    sleep 0.1
  done
  fail "inbox has $(peek_inbox | count) visible, want $1"
}

echo "[1/7] build + boot (channel hooks on)"
go build -o bin/loomcycle ./cmd/loomcycle
boot 1
grep -q "channel hooks: worker started" "$TEST_DIR/boot.log" || fail "the channel-hook worker did not start"

echo "[2/7] create the screen HookDef (code-js: drop spam, redact secrets)"
python3 - > "$TEST_DIR/hookdef.req" <<'PY'
import json
code = """function hook(ev) {
  var t = ev.body.text || "";
  if (t.indexOf("spam") >= 0) return {decision: "drop", reason: "spam"};
  if (t.indexOf("secret") >= 0) return {updated_body: {text: t.replace("secret", "[redacted]")}};
  return {};
}"""
print(json.dumps({"op": "create", "name": "screen", "overlay": {
    "event": "channel_publish", "fail_mode": "closed", "body": {"kind": "code-js", "code": code}}}))
PY
api -X POST "$BASE/v1/_hookdef" -d @"$TEST_DIR/hookdef.req" > "$TEST_DIR/hookdef.json"
grep -q '"def_id"' "$TEST_DIR/hookdef.json" || fail "HookDef create: $(cat "$TEST_DIR/hookdef.json")"

echo "[3/7] publish over HTTP: plain, secret, spam"
for body in '{"text":"hello"}' '{"text":"my secret"}' '{"text":"buy spam"}'; do
  out=$(publish "$body")
  echo "$out" | grep -q '"awaiting_hooks":true' || fail "publish did not report awaiting_hooks: $out"
done
wait_for_count 2
PEEK=$(peek_inbox)
echo "$PEEK" | grep -q 'hello' || fail "the plain message was not delivered: $PEEK"
echo "$PEEK" | grep -q '\[redacted\]' || fail "the secret was not redacted: $PEEK"
echo "$PEEK" | grep -q 'my secret' && fail "the unredacted body was delivered: $PEEK"
echo "$PEEK" | grep -q 'spam' && fail "the spam was delivered: $PEEK"
echo "      delivered: hello + [redacted]; spam dropped"

echo "[4/7] publish from an agent's Channel tool"
curl -fsS -N -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -H "Accept: text/event-stream" \
  -d '{"agent":"screened_writer","user_id":"u1","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}' \
  "$BASE/v1/runs" > "$TEST_DIR/run.sse"
grep -q 'awaiting_hooks' "$TEST_DIR/run.sse" || fail "the agent's publish did not report awaiting_hooks"
wait_for_count 3
peek_inbox | grep -q 'from the agent, with a \[redacted\]' || fail "the agent's message was not decided by the hook"

echo "[5/7] decisions recorded (read over MCP at tenant scope) and counted"
MCPH=(-H "Content-Type: application/json" -H "Accept: application/json, text/event-stream")
MCPSID=$(curl -sS -D - -o /dev/null "${MCPH[@]}" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"suite","version":"1"}}}' \
  "$BASE/v1/_mcp" | awk 'tolower($1)=="mcp-session-id:"{print $2}' | tr -d '\r')
[[ -n "$MCPSID" ]] || fail "no MCP session"
curl -sS -o /dev/null "${MCPH[@]}" -H "Mcp-Session-Id: $MCPSID" -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' "$BASE/v1/_mcp"
DEC=$(curl -sS "${MCPH[@]}" -H "Mcp-Session-Id: $MCPSID" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"peek_channel","arguments":{"channel":"_system/channel_hooks/decisions","scope":"tenant","max_messages":100}}}' \
  "$BASE/v1/_mcp")
echo "$DEC" | grep -q 'drop' || fail "no drop decision recorded: $DEC"
echo "$DEC" | grep -q 'rewrite_body' || fail "no rewrite decision recorded: $DEC"
api "$BASE/metrics" | grep -q 'loomcycle_channel_hooks_decisions_total{.*decision="drop"' || fail "/metrics has no drop counter"

echo "[5b/7] a runtime channel carries hooks too"
out=$(api -X POST "$BASE/v1/_channels" -d '{"name":"rt-inbox","scope":"global","hooks":{"channel_publish":["screen"]}}')
echo "$out" | grep -q '"channel_publish"' || fail "runtime channel create did not keep its hooks: $out"
out=$(api -X POST "$BASE/v1/_channels" -d '{"name":"rt-bad","scope":"global","hooks":{"channel_publish":["no-such-hook"]}}')
echo "$out" | grep -q 'channel_hooks_invalid' || fail "an unknown HookDef was accepted: $out"
out=$(api -X POST "$BASE/v1/_channels/rt-inbox/publish" -d '{"payload":{"text":"runtime secret"}}')
echo "$out" | grep -q '"awaiting_hooks":true' || fail "runtime publish did not report awaiting_hooks: $out"
for i in $(seq 1 100); do
  api "$BASE/v1/_channels/rt-inbox/peek?max_messages=10" | grep -q 'runtime \[redacted\]' && break
  sleep 0.1
done
api "$BASE/v1/_channels/rt-inbox/peek?max_messages=10" | grep -q 'runtime \[redacted\]' || fail "the runtime channel's message was not decided by its hook"
api "$BASE/v1/_channels" | grep -q '"hooks"' || fail "the channel list does not show hooks"

echo "[6/7] kill -9 with messages waiting; a restart decides each exactly once"
for i in $(seq 1 20); do publish "{\"text\":\"batch $i\"}" > /dev/null; done
kill -9 "$PID"; wait "$PID" 2>/dev/null || true; PID=""
boot 1
# A message the killed worker was deciding keeps its lease (60s) before the
# restarted one may claim it — hence the long wait.
wait_for_count 23 900
N=$(peek_inbox | python3 -c "import sys,json; m=json.load(sys.stdin)['messages']; print(len([x for x in m if 'batch' in json.dumps(x)]))")
[[ "$N" == "20" ]] || fail "delivered $N of the 20 batch messages after the restart"

echo "[7/7] with channel hooks off, a publish to the hooked channel is refused"
kill "$PID"; wait "$PID" 2>/dev/null || true; PID=""
boot 0
grep -q 'carries hooks, but channel hooks are off' "$TEST_DIR/boot.log" || fail "no boot warning for the hooked channel"
code=$(curl -sS -o "$TEST_DIR/refused.json" -w '%{http_code}' -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -X POST "$BASE/v1/_channels/inbox/publish" -d '{"payload":{"text":"x"}}')
[[ "$code" == "409" ]] && grep -q channel_hooks_disabled "$TEST_DIR/refused.json" || fail "want 409 channel_hooks_disabled, got $code: $(cat "$TEST_DIR/refused.json")"

echo "PASS ✓ — channel hooks decided every surface's messages, recorded their decisions, survived a kill -9, and refuse when off"
