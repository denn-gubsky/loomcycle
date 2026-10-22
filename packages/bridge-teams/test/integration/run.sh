#!/usr/bin/env bash
# Boots a real loomcycle (mock provider, sqlite, no keys) and runs the bridge's
# integration test against it: real per-user tokens, real interactive runs,
# a fake Teams side. Usage: packages/bridge-teams/test/integration/run.sh
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PKG="$(cd "$HERE/../.." && pwd)"
REPO="$(cd "$PKG/../.." && pwd)"
TEST_DIR="$(mktemp -d -t teams-bridge-it.XXXXXX)"
PORT="${TEAMS_BRIDGE_IT_PORT:-18961}"
BASE="http://127.0.0.1:$PORT"
TOKEN="it-$(openssl rand -hex 16)"
# Auth must be on: in open mode loomcycle ignores config principals and every
# caller is the default admin in tenant "", which cannot mint user tokens.
ADMIN_TOKEN="it-admin-$(openssl rand -hex 16)"
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo "logs: $TEST_DIR/boot.log"
  # CI keeps the server's log as an artifact: the temp directory goes with
  # the runner, and a stall inside the server shows only there, in the
  # goroutine dump taken on a failure.
  if [[ -n "${TEAMS_BRIDGE_IT_LOG_OUT:-}" && -f "$TEST_DIR/boot.log" ]]; then
    mkdir -p "$(dirname "$TEAMS_BRIDGE_IT_LOG_OUT")"
    cp "$TEST_DIR/boot.log" "$TEAMS_BRIDGE_IT_LOG_OUT"
  fi
}
# show_log prints the end of the server's log, for a failure in CI's output.
show_log() {
  echo "---- loomcycle server log, last 200 lines ($TEST_DIR/boot.log) ----"
  tail -n 200 "$TEST_DIR/boot.log" || true
  echo "---- end of server log ----"
}
trap cleanup EXIT INT TERM

cd "$REPO"
go build -o "$TEST_DIR/loomcycle" ./cmd/loomcycle
mkdir -p "$TEST_DIR/home"
# env -i + a scratch HOME: the boot must not pick up the developer's own
# ~/.config/loomcycle or .env files.
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
  LOOMCYCLE_MOCK_ENABLED=1 \
  GOTRACEBACK=all \
  LOOMCYCLE_DATA_DIR="$TEST_DIR/data" \
  LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$PORT" \
  LOOMCYCLE_AUTH_TOKEN="$ADMIN_TOKEN" \
  LOOMCYCLE_TEAMS_BRIDGE_IT_TOKEN="$TOKEN" \
  "$TEST_DIR/loomcycle" --config "$HERE/loomcycle.yaml" > "$TEST_DIR/boot.log" 2>&1 &
PID=$!
healthy=""
for _ in $(seq 1 100); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && { healthy=1; break; }
  kill -0 "$PID" 2>/dev/null || { show_log; exit 1; }
  sleep 0.2
done
# A server that never became healthy is a boot failure, not a test failure.
[[ -n "$healthy" ]] || { echo "loomcycle did not become healthy within 20s"; show_log; exit 1; }

cd "$PKG"
status=0
TEAMS_BRIDGE_IT_BASE_URL="$BASE" TEAMS_BRIDGE_IT_TOKEN="$TOKEN" npx vitest run test/integration || status=$?
if [[ $status -ne 0 ]]; then
  show_log
  # Where the server's runs are waiting: SIGQUIT makes the Go runtime write
  # every goroutine's stack to the log (GOTRACEBACK=all) and exit. It lands
  # in the kept log, not in this output, as it runs to thousands of lines.
  if kill -QUIT "$PID" 2>/dev/null; then
    wait "$PID" 2>/dev/null || true
    PID=""
    echo "goroutine dump appended to the server log"
  fi
fi
exit $status
