#!/usr/bin/env bash
# A real-boot smoke test (local, not CI): boots loomcycle (mock provider,
# sqlite, no keys), runs the built Slack bridge CLI against it, and drives it
# with signed Slack requests while a fake Slack Web API records what the
# bridge sends. Usage: packages/bridge-slack/test/integration/run.sh
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PKG="$(cd "$HERE/../.." && pwd)"
REPO="$(cd "$PKG/../.." && pwd)"
TEST_DIR="$(mktemp -d -t slack-bridge-it.XXXXXX)"
PORT="${SLACK_BRIDGE_IT_PORT:-18971}"
BASE="http://127.0.0.1:$PORT"
TOKEN="it-$(openssl rand -hex 16)"
# Auth must be on: in open mode loomcycle ignores config principals and every
# caller is the default admin in tenant "", which cannot mint user tokens.
ADMIN_TOKEN="it-admin-$(openssl rand -hex 16)"
cleanup() {
  [[ -n "${PID:-}" ]] && { kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; }
  echo "logs: $TEST_DIR/boot.log"
}
trap cleanup EXIT INT TERM

cd "$REPO"
go build -o "$TEST_DIR/loomcycle" ./cmd/loomcycle
mkdir -p "$TEST_DIR/home"
# env -i + a scratch HOME: the boot must not pick up the developer's own
# ~/.config/loomcycle or .env files.
env -i PATH="$PATH" HOME="$TEST_DIR/home" \
  LOOMCYCLE_MOCK_ENABLED=1 \
  LOOMCYCLE_DATA_DIR="$TEST_DIR/data" \
  LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$PORT" \
  LOOMCYCLE_AUTH_TOKEN="$ADMIN_TOKEN" \
  LOOMCYCLE_SLACK_BRIDGE_IT_TOKEN="$TOKEN" \
  "$TEST_DIR/loomcycle" --config "$HERE/loomcycle.yaml" > "$TEST_DIR/boot.log" 2>&1 &
PID=$!
for _ in $(seq 1 100); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && break
  kill -0 "$PID" 2>/dev/null || { tail -30 "$TEST_DIR/boot.log"; exit 1; }
  sleep 0.2
done

cd "$PKG"
npm run build >/dev/null
SLACK_BRIDGE_IT_BASE_URL="$BASE" SLACK_BRIDGE_IT_TOKEN="$TOKEN" node "$HERE/smoke.mjs"
