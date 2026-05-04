#!/usr/bin/env bash
# loomcycle.sh — rebuild + restart the loomcycle sidecar with stamped build metadata.
#
# Usage:
#   ./loomcycle.sh               # rebuild + start, sourcing .env.local
#   ./loomcycle.sh --config X    # override config path (default ~/.config/loomcycle/loomcycle.yaml)
#   ./loomcycle.sh --no-build    # skip the rebuild (use existing bin/loomcycle)
#   ./loomcycle.sh --version     # build then print build identifier and exit
#
# Why this exists: `git pull && bin/loomcycle ...` runs the OLD binary.
# This script makes "fresh source → fresh binary" the default. Build
# metadata (commit + UTC time) is injected via -ldflags so a running
# loomcycle can identify itself at startup.

set -euo pipefail

# ─── Locate ourselves regardless of the caller's pwd ──────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# ─── Defaults (override via flags or environment) ─────────────────────
CONFIG="${LOOMCYCLE_CONFIG:-$HOME/.config/loomcycle/loomcycle.yaml}"
ENV_FILE="${LOOMCYCLE_ENV_FILE:-.env.local}"
BIN="bin/loomcycle"
DO_BUILD=1
SHOW_VERSION=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --config)     CONFIG="$2"; shift 2 ;;
    --no-build)   DO_BUILD=0; shift ;;
    --version)    SHOW_VERSION=1; shift ;;
    -h|--help)    sed -n '1,12p' "$0"; exit 0 ;;
    *)            echo "loomcycle.sh: unknown flag '$1'" >&2; exit 2 ;;
  esac
done

# ─── 1. Rebuild with stamped commit + time ────────────────────────────
if [[ $DO_BUILD -eq 1 ]]; then
  COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo 'unknown')"
  # Append "-dirty" if the working tree has uncommitted changes — tells
  # the operator the binary doesn't match a clean commit they could
  # check out.
  if ! git diff-index --quiet HEAD -- 2>/dev/null; then
    COMMIT="${COMMIT}-dirty"
  fi
  BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  echo "loomcycle.sh: building commit=$COMMIT time=$BUILD_TIME"
  go build \
    -ldflags "-X main.buildCommit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
    -o "$BIN" \
    ./cmd/loomcycle
fi

if [[ $SHOW_VERSION -eq 1 ]]; then
  exec "$BIN" --version
fi

# ─── 2. Source .env.local before exec so loomcycle inherits the vars ──
if [[ -f "$ENV_FILE" ]]; then
  echo "loomcycle.sh: sourcing $ENV_FILE"
  # `set -a` exports every assignment without needing each line to use
  # `export`. `set +a` switches it back off so we don't unintentionally
  # export later script-locals.
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
else
  echo "loomcycle.sh: no $ENV_FILE found (ok for first run; copy from .env.example)" >&2
fi

# ─── 3. Stop any prior loomcycle instance ─────────────────────────────
# This is the design choice you might want to tune — see
# stop_prior_instance() below. Default behaviour: send SIGTERM to any
# loomcycle process started from THIS bin path, wait briefly for it to
# release the port, fall back to SIGKILL if it stuck around.
stop_prior_instance() {
  # Match by exact binary path so we never kill an unrelated process
  # that happens to be on the same port. `pgrep -f` matches the full
  # command line.
  local target_bin
  target_bin="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"

  local pids
  pids="$(pgrep -f "$target_bin" || true)"
  if [[ -z "$pids" ]]; then
    return 0
  fi

  echo "loomcycle.sh: stopping prior instance(s): $pids"
  # SIGTERM first — loomcycle catches it and shuts down cleanly
  # (releases the port, flushes the store).
  # shellcheck disable=SC2086
  kill -TERM $pids 2>/dev/null || true

  # Wait up to 5s for graceful exit.
  local i
  for i in $(seq 1 10); do
    if ! pgrep -f "$target_bin" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done

  echo "loomcycle.sh: prior instance didn't exit; sending SIGKILL" >&2
  # shellcheck disable=SC2086
  kill -KILL $pids 2>/dev/null || true
  sleep 0.5
}

stop_prior_instance

# ─── 4. Exec the binary so signals (Ctrl+C) reach loomcycle directly ──
echo "loomcycle.sh: starting $BIN --config $CONFIG"
exec "$BIN" --config "$CONFIG"
