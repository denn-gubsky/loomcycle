#!/usr/bin/env bash
# run.sh — convenience wrapper for the circuit-stress load test.
#
# Brings up a single-binary loomcycle against a Postgres container,
# runs the driver with whatever flags you pass through, then shuts
# the binary down cleanly. The Postgres container is left running
# (and reused across invocations) so consecutive runs see a clean
# DB after the driver's sanity sweep but don't pay the container
# startup tax each time.
#
# Prereqs (one-time):
#   loomcycle anthropic login              # populate ~/.config/loomcycle/anthropic-oauth.json
#   export LOOMCYCLE_AUTH_TOKEN=$(openssl rand -hex 32)
#
# Usage (from repo root):
#   ./test/load/circuit-stress/run.sh                          # x1 smoke
#   ./test/load/circuit-stress/run.sh --scale 10               # x10
#   ./test/load/circuit-stress/run.sh --scale 100 --circuits-per-user 15
#   ./test/load/circuit-stress/run.sh --scale 1000 --circuits-per-user 20

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
cd "$REPO_ROOT"

PG_CONTAINER="${PG_CONTAINER:-lc-loadtest-pg}"
PG_PORT="${PG_PORT:-15432}"   # avoid colliding with local Postgres on :5432
PG_PASSWORD="${PG_PASSWORD:-loomcycle}"
LC_PORT="${LC_PORT:-8787}"
RESULTS_DIR="${RESULTS_DIR:-$SCRIPT_DIR/results/$(date -u +%Y-%m-%dT%H-%M-%SZ)}"

require() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "✗ missing dependency: $1" >&2
        exit 1
    }
}
require docker
require go
require psql || true   # only needed for post-test validation

: "${LOOMCYCLE_AUTH_TOKEN:?LOOMCYCLE_AUTH_TOKEN must be set}"

mkdir -p "$RESULTS_DIR"
echo "→ results dir: $RESULTS_DIR"

# ─── Postgres ───────────────────────────────────────────────────────
if ! docker inspect "$PG_CONTAINER" >/dev/null 2>&1; then
    echo "→ starting Postgres container ($PG_CONTAINER on :$PG_PORT)…"
    docker run -d --name "$PG_CONTAINER" \
        -p "127.0.0.1:$PG_PORT:5432" \
        -e POSTGRES_PASSWORD="$PG_PASSWORD" \
        postgres:16 >/dev/null
    # Wait until pg_isready
    until docker exec "$PG_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; do
        sleep 1
    done
elif [ "$(docker inspect -f '{{.State.Running}}' "$PG_CONTAINER")" != "true" ]; then
    echo "→ restarting Postgres container ($PG_CONTAINER)…"
    docker start "$PG_CONTAINER" >/dev/null
    until docker exec "$PG_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; do
        sleep 1
    done
else
    echo "→ Postgres container $PG_CONTAINER already running"
fi

export LOOMCYCLE_PG_DSN="postgres://postgres:$PG_PASSWORD@127.0.0.1:$PG_PORT/postgres?sslmode=disable"

# ─── loomcycle binary ───────────────────────────────────────────────
# Always rebuild — Go's incremental compile is sub-second when source
# is unchanged, but skipping the rebuild on a stale `bin/loomcycle`
# silently runs an old binary. The dual-provider load tests on
# 2026-05-26 chased this trap four times: `make build-all` populates
# Go's build cache but doesn't write `bin/loomcycle`, so an old
# artifact from days prior keeps getting executed.
LC_BIN="${LC_BIN:-$REPO_ROOT/bin/loomcycle}"
echo "→ rebuilding loomcycle binary from current HEAD…"
go build -o "$LC_BIN" ./cmd/loomcycle/

# Env that the test yaml + OAuth-dev driver depend on.
export LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1
export LOOMCYCLE_PG_AUTOMIGRATE=1
export LOOMCYCLE_METRICS_ENABLED=1
export LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS=120000
export LOOMCYCLE_LISTEN_ADDR="127.0.0.1:$LC_PORT"

# ─── Generate the scale-sized yaml ──────────────────────────────────
# Parse --scale out of the args we'll forward to the driver so we
# know how many channel entries to write into the generated yaml.
# Default matches the driver's default. (Bash parameter parsing
# stays simple — we tolerate any arg order.)
SCALE=1
args=("$@")
for ((i=0; i<${#args[@]}; i++)); do
    a="${args[i]}"
    if [ "$a" = "--scale" ] && [ $((i+1)) -lt ${#args[@]} ]; then
        SCALE="${args[i+1]}"
    elif [[ "$a" == --scale=* ]]; then
        SCALE="${a#--scale=}"
    fi
done
echo "→ generating yaml for scale=${SCALE}..."
GEN_YAML="$RESULTS_DIR/loomcycle.gen.yaml"
awk -v scale="$SCALE" '
    /BEGIN generated channels/ {
        print
        for (i = 1; i <= scale; i++) {
            printf "  research-done/c%d:\n    description: \"researcher → editor signal for circuit %d\"\n    scope: global\n    semantic: queue\n    default_ttl: 600\n    max_messages: 1000\n", i, i
            printf "  editing-done/c%d:\n    description: \"editor → evaluator signal for circuit %d\"\n    scope: global\n    semantic: queue\n    default_ttl: 600\n    max_messages: 1000\n", i, i
        }
        in_block = 1
        next
    }
    /END generated channels/ { in_block = 0 }
    !in_block { print }
' "$SCRIPT_DIR/loomcycle.template.yaml" > "$GEN_YAML"

echo "→ starting loomcycle on :$LC_PORT (logs: $RESULTS_DIR/loomcycle.log)…"
"$LC_BIN" --config "$GEN_YAML" \
    >"$RESULTS_DIR/loomcycle.log" 2>&1 &
LC_PID=$!

cleanup() {
    if kill -0 "$LC_PID" 2>/dev/null; then
        echo "→ stopping loomcycle (pid=$LC_PID)…"
        kill -INT "$LC_PID" 2>/dev/null || true
        wait "$LC_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# Wait for /healthz
echo "→ waiting for /healthz…"
for i in 1 2 3 4 5 6 7 8 9 10; do
    if curl -fsS -m 2 -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
        "http://127.0.0.1:$LC_PORT/healthz" >/dev/null 2>&1; then
        echo "  loomcycle healthy"
        break
    fi
    if [ "$i" = "10" ]; then
        echo "✗ loomcycle never became healthy; tail of log:"
        tail -40 "$RESULTS_DIR/loomcycle.log"
        exit 1
    fi
    sleep 1
done

# Pre-test metrics snapshot
echo "→ metrics snapshot (pre)…"
curl -fsS -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
    "http://127.0.0.1:$LC_PORT/v1/_metrics/summary" \
    >"$RESULTS_DIR/metrics-summary-pre.json" 2>/dev/null || true

# ─── Run the driver ─────────────────────────────────────────────────
# Always rebuild — same rationale as the loomcycle binary above.
# Sub-second when source is unchanged; prevents the stale-binary trap.
DRIVER_BIN="${DRIVER_BIN:-$SCRIPT_DIR/circuit-stress}"
echo "→ rebuilding driver from current source…"
(cd "$SCRIPT_DIR" && go build -o circuit-stress .)

echo "→ running driver: $DRIVER_BIN $* --results-dir $RESULTS_DIR --base-url http://127.0.0.1:$LC_PORT"
echo
"$DRIVER_BIN" "$@" --results-dir "$RESULTS_DIR" --base-url "http://127.0.0.1:$LC_PORT"
RC=$?

# Post-test metrics snapshot
echo "→ metrics snapshot (post)…"
curl -fsS -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
    "http://127.0.0.1:$LC_PORT/v1/_metrics/summary" \
    >"$RESULTS_DIR/metrics-summary-post.json" 2>/dev/null || true

echo
echo "→ done. Results in $RESULTS_DIR/"
echo "    circuits.jsonl, loomcycle.log, metrics-summary-{pre,post}.json"
echo "    Web UI:  open \"http://127.0.0.1:$LC_PORT/ui?token=\$LOOMCYCLE_AUTH_TOKEN\""

exit $RC
