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
  # `export`. `set +a` after the source switches it back off so we
  # don't unintentionally export later script-locals.
  #
  # `set +u` while sourcing: dotenv files commonly reference upstream
  # vars (e.g. `FOO="${UPSTREAM_VAR}/x"`) that aren't required for the
  # script itself but are kept for parity across machines. Under `set -u`
  # any such expansion against an unset upstream var aborts the script
  # before loomcycle starts — even when loomcycle itself doesn't need
  # that var. The binary's own required-config validation still
  # surfaces actually-missing vars with a clear log line.
  #
  # Save-then-restore via `$-` instead of unconditional `set -u`: if a
  # future invocation runs loomcycle.sh with `-u` explicitly off (or
  # a wrapper unsets it), we don't want to reintroduce strict-undefined
  # checking that the caller deliberately disabled. `$-` carries the
  # currently-enabled shell options as letter flags; we restore only
  # what was set going in.
  _loomcycle_saved_opts="$-"
  set -a
  set +u
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  [[ "$_loomcycle_saved_opts" == *u* ]] && set -u
  set +a
  unset _loomcycle_saved_opts
else
  echo "loomcycle.sh: no $ENV_FILE found (ok for first run; copy from .env.example)" >&2
fi

# ─── 3. Stop any prior instance bound to the loomcycle port ───────────
# Match by listen port (LOOMCYCLE_LISTEN_ADDR's :port suffix). Catches
# any prior loomcycle regardless of which checkout / binary path it
# was started from — useful when you have multiple working trees open
# and the script in one of them needs to clear whatever's on the port.
#
# Trade-off accepted: this WILL kill an unrelated process listening on
# the same port (e.g. `python -m http.server 8787`). LOOMCYCLE_LISTEN_ADDR
# defaults to 127.0.0.1:8787; bind to a less-conventional port if the
# blast radius is a concern.
#
# `lsof -sTCP:LISTEN` filters to listening sockets only, so a TCP
# client connected to that port from elsewhere on the box is NOT a
# match. That keeps innocent clients alive.
stop_prior_instance() {
  local addr="${LOOMCYCLE_LISTEN_ADDR:-127.0.0.1:8787}"
  # Strip everything up to and including the last ':' to get the port.
  # Works for "127.0.0.1:8787", "[::1]:8787", and ":8787".
  local port="${addr##*:}"
  if [[ -z "$port" || "$port" == "$addr" ]]; then
    echo "loomcycle.sh: cannot extract port from LOOMCYCLE_LISTEN_ADDR='$addr'; skipping prior-instance stop" >&2
    return 0
  fi

  # Need lsof. Skip cleanly on systems without it rather than failing
  # the whole start (the bind itself will surface the conflict if any).
  if ! command -v lsof >/dev/null 2>&1; then
    echo "loomcycle.sh: lsof not found; skipping prior-instance stop" >&2
    return 0
  fi

  local pids
  pids="$(lsof -ti tcp:"$port" -sTCP:LISTEN 2>/dev/null || true)"
  if [[ -z "$pids" ]]; then
    return 0
  fi

  echo "loomcycle.sh: stopping process(es) listening on port $port: $pids"
  # SIGTERM first — loomcycle catches it and shuts down cleanly
  # (releases the port, flushes the store).
  # shellcheck disable=SC2086
  kill -TERM $pids 2>/dev/null || true

  # Wait up to 5s for graceful exit.
  local i
  for i in $(seq 1 10); do
    if [[ -z "$(lsof -ti tcp:"$port" -sTCP:LISTEN 2>/dev/null)" ]]; then
      return 0
    fi
    sleep 0.5
  done

  echo "loomcycle.sh: process didn't exit on SIGTERM; sending SIGKILL" >&2
  pids="$(lsof -ti tcp:"$port" -sTCP:LISTEN 2>/dev/null || true)"
  if [[ -n "$pids" ]]; then
    # shellcheck disable=SC2086
    kill -KILL $pids 2>/dev/null || true
  fi
  sleep 0.5
}

stop_prior_instance

# ─── 4. Exec the binary so signals (Ctrl+C) reach loomcycle directly ──
echo "loomcycle.sh: starting $BIN --config $CONFIG"
exec "$BIN" --config "$CONFIG"
