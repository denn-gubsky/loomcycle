#!/usr/bin/env bash
# A real boot, local only: builds loomcycle from this repo, boots it on the
# mock provider (sqlite, no keys), then runs the built bridge CLI against it
# with a fake Bot API on loopback and posts webhook updates to the bridge.
# Usage: packages/bridge-telegram/test/integration/run.sh
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PKG="$(cd "$HERE/../.." && pwd)"
REPO="$(cd "$PKG/../.." && pwd)"
TEST_DIR="$(mktemp -d -t telegram-bridge-it.XXXXXX)"
PORT="${TELEGRAM_BRIDGE_IT_PORT:-18971}"
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
  LOOMCYCLE_TELEGRAM_BRIDGE_IT_TOKEN="$TOKEN" \
  "$TEST_DIR/loomcycle" --config "$HERE/loomcycle.yaml" > "$TEST_DIR/boot.log" 2>&1 &
PID=$!
for _ in $(seq 1 100); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && break
  kill -0 "$PID" 2>/dev/null || { tail -30 "$TEST_DIR/boot.log"; exit 1; }
  sleep 0.2
done

cd "$PKG"
npm run build >/dev/null
TELEGRAM_BRIDGE_IT_BASE_URL="$BASE" TELEGRAM_BRIDGE_IT_TOKEN="$TOKEN" TELEGRAM_BRIDGE_IT_HOME="$TEST_DIR/home" \
  node "$HERE/smoke.mjs"
