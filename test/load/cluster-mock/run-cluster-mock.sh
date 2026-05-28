#!/usr/bin/env bash
# run-cluster-mock.sh — multi-replica mock-mode stress harness.
#
# Brings up an N-replica loomcycle cluster (built from HEAD, mock
# provider, shared Postgres, nginx LB) via docker compose, drives the
# circuit-stress harness through the LB, and captures per-replica
# metrics. The cluster analogue of circuit-stress/run-mock-fallback.sh.
#
# Topology (all in one generated compose, isolated network):
#   postgres            internal :5432   host :${PG_PORT}   (analysis)
#   replica-1..N        internal :8787   host :${REPLICA_BASE_PORT}+i
#   lb (nginx)          internal :80     host :${LB_PORT}   (driver target)
#
# Each replica differs ONLY by LOOMCYCLE_REPLICA_ID (the cluster-mode
# gate). They share one Postgres, so the per-replica pgxpool is sized
# from N to keep N × pool under the container's max_connections.
#
# Prereqs:
#   export LOOMCYCLE_AUTH_TOKEN=loomcycle   # shared bearer + Web UI token
#
# Usage (from repo root):
#   ./test/load/cluster-mock/run-cluster-mock.sh --replicas 2 --scale 100
#   ./test/load/cluster-mock/run-cluster-mock.sh --replicas 3 --scale 5000 --circuits-per-user 20
#   LOOMCYCLE_MOCK_429_RATE=0.15 \
#     ./test/load/cluster-mock/run-cluster-mock.sh --replicas 4 --scale 10000
#
# Scenario hooks (run concurrently with the driver):
#   --scenario cancel   launch inject-cancel.sh mid-load (cross-replica cancel)
#   --scenario crash    launch inject-crash.sh  mid-load (kill a replica)
#   --scenario none     (default) plain throughput run
#
# Failure-injection + sampler env (all optional, same as the
# single-replica harness):
#   LOOMCYCLE_MOCK_429_RATE / LOOMCYCLE_MOCK_500_RATE
#   LOOMCYCLE_MOCK_LATENCY_MS / LOOMCYCLE_MOCK_LATENCY_JITTER_MS
#   LOOMCYCLE_CHANNEL_DEBUG=1

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CIRCUIT_DIR="$REPO_ROOT/test/load/circuit-stress"
cd "$REPO_ROOT"

# ─── Tunables ───────────────────────────────────────────────────────
REPLICAS="${REPLICAS:-3}"
SCALE=1
SCENARIO="none"
PG_PORT="${PG_PORT:-15433}"          # distinct from circuit-stress's 15432
LB_PORT="${LB_PORT:-18080}"
REPLICA_BASE_PORT="${REPLICA_BASE_PORT:-18800}"  # replica-i → 18800+i
PG_PASSWORD="${PG_PASSWORD:-loomcycle}"
PG_MAX_CONNECTIONS="${PG_MAX_CONNECTIONS:-200}"
# Cluster-wide connection budget reserved for replica pools; the rest
# of max_connections is headroom for psql + sweeper + LISTEN sessions.
PG_POOL_BUDGET="${PG_POOL_BUDGET:-160}"
IMAGE="${LOOMCYCLE_IMAGE:-loomcycle:mock-local}"
PROJECT="${COMPOSE_PROJECT:-lc-cluster-mock}"
NO_TEARDOWN="${NO_TEARDOWN:-0}"

# ─── Parse args (consume rig flags, pass the rest to the driver) ─────
DRIVER_ARGS=()
while [ $# -gt 0 ]; do
    case "$1" in
        --replicas) REPLICAS="$2"; shift 2 ;;
        --replicas=*) REPLICAS="${1#*=}"; shift ;;
        --scenario) SCENARIO="$2"; shift 2 ;;
        --scenario=*) SCENARIO="${1#*=}"; shift ;;
        --no-teardown) NO_TEARDOWN=1; shift ;;
        --scale) SCALE="$2"; DRIVER_ARGS+=("$1" "$2"); shift 2 ;;
        --scale=*) SCALE="${1#*=}"; DRIVER_ARGS+=("$1"); shift ;;
        *) DRIVER_ARGS+=("$1"); shift ;;
    esac
done

require() { command -v "$1" >/dev/null 2>&1 || { echo "✗ missing dependency: $1" >&2; exit 1; }; }
require docker
require go
require jq
require psql
: "${LOOMCYCLE_AUTH_TOKEN:?LOOMCYCLE_AUTH_TOKEN must be set (e.g. export LOOMCYCLE_AUTH_TOKEN=loomcycle)}"

case "$SCENARIO" in none|cancel|crash) ;; *) echo "✗ unknown --scenario $SCENARIO (none|cancel|crash)" >&2; exit 1 ;; esac
if ! [[ "$REPLICAS" =~ ^[1-9][0-9]*$ ]]; then echo "✗ --replicas must be a positive integer" >&2; exit 1; fi

# Per-replica pool = floor(budget / N), min 10. Warn if the implied
# total would exceed max_connections (it can't, by construction, but
# surface the math for the operator).
POOL=$(( PG_POOL_BUDGET / REPLICAS ))
[ "$POOL" -lt 10 ] && POOL=10
TOTAL_POOL=$(( POOL * REPLICAS ))

RESULTS_DIR="${RESULTS_DIR:-$SCRIPT_DIR/results/$(date -u +%Y-%m-%dT%H-%M-%SZ)-cluster-r${REPLICAS}-${SCENARIO}}"
mkdir -p "$RESULTS_DIR"
echo "→ results dir: $RESULTS_DIR"
echo "→ topology: replicas=$REPLICAS scenario=$SCENARIO per-replica pool=$POOL (total=$TOTAL_POOL ≤ max_connections=$PG_MAX_CONNECTIONS)"

PG_DSN_HOST="postgres://postgres:${PG_PASSWORD}@127.0.0.1:${PG_PORT}/loomcycle?sslmode=disable"
COMPOSE_FILE="$RESULTS_DIR/docker-compose.gen.yaml"
NGINX_CONF="$RESULTS_DIR/nginx.gen.conf"
GEN_YAML="$RESULTS_DIR/loomcycle.gen.yaml"

# ─── Build the loomcycle image from HEAD ────────────────────────────
# The Dockerfile builds the web UI (stage 1) + the Go binary (stage 2),
# so /ui works for live observation. Mock is a runtime env gate, not a
# build flag — the stock image is fine.
echo "→ building $IMAGE from HEAD (Dockerfile builds UI + binary)…"
docker build -q -t "$IMAGE" "$REPO_ROOT" >/dev/null

# ─── Generate the scale-sized cluster yaml (channel injection) ──────
echo "→ generating cluster yaml for scale=$SCALE…"
awk -v scale="$SCALE" '
    /BEGIN generated channels/ {
        print
        for (i = 1; i <= scale; i++) {
            printf "  research-done/c%d:\n    description: \"r%d\"\n    scope: global\n    semantic: queue\n    default_ttl: 600\n    max_messages: 1000\n", i, i
            printf "  editing-done/c%d:\n    description: \"e%d\"\n    scope: global\n    semantic: queue\n    default_ttl: 600\n    max_messages: 1000\n", i, i
        }
        in_block = 1; next
    }
    /END generated channels/ { in_block = 0 }
    !in_block { print }
' "$SCRIPT_DIR/loomcycle.cluster-mock.yaml" > "$GEN_YAML"

# ─── Generate nginx upstream ────────────────────────────────────────
UPSTREAM=""
for i in $(seq 1 "$REPLICAS"); do UPSTREAM+="        server replica-${i}:8787;"$'\n'; done
awk -v servers="$UPSTREAM" '{ gsub(/__UPSTREAM_SERVERS__/, servers); print }' \
    "$SCRIPT_DIR/nginx.conf.tmpl" > "$NGINX_CONF"

# ─── Generate docker-compose ────────────────────────────────────────
# Mock + sampler + cluster env, identical across replicas except
# LOOMCYCLE_REPLICA_ID and the host port. Postgres co-resident in the
# compose network so replicas reach it as postgres:5432; published on
# $PG_PORT for host-side psql analysis.
M_LAT="${LOOMCYCLE_MOCK_LATENCY_MS:-50}"
M_JIT="${LOOMCYCLE_MOCK_LATENCY_JITTER_MS:-25}"
M_429="${LOOMCYCLE_MOCK_429_RATE:-0}"
M_500="${LOOMCYCLE_MOCK_500_RATE:-0}"
CH_DBG="${LOOMCYCLE_CHANNEL_DEBUG:-0}"

# Replicas-sweeper tuning: defaults (60s interval / 90s stale) leave the
# dead-replica reap firing outside a typical sub-minute load test window.
# For the crash scenario, drive both down so the reap is observable
# in-test (LOOMCYCLE_REPLICAS_SWEEP_INTERVAL_MS + LOOMCYCLE_REPLICAS_STALE_AFTER_MS
# are operator knobs added alongside this rig).
if [ "$SCENARIO" = "crash" ]; then
    REPL_SWEEP_MS="${LOOMCYCLE_REPLICAS_SWEEP_INTERVAL_MS:-5000}"
    REPL_STALE_MS="${LOOMCYCLE_REPLICAS_STALE_AFTER_MS:-15000}"
else
    REPL_SWEEP_MS="${LOOMCYCLE_REPLICAS_SWEEP_INTERVAL_MS:-}"
    REPL_STALE_MS="${LOOMCYCLE_REPLICAS_STALE_AFTER_MS:-}"
fi

{
cat <<YAML
# GENERATED by run-cluster-mock.sh — do not edit. replicas=$REPLICAS
services:
  postgres:
    image: postgres:16
    command: ["-c", "max_connections=$PG_MAX_CONNECTIONS"]
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: $PG_PASSWORD
      POSTGRES_DB: loomcycle
    ports: ["127.0.0.1:$PG_PORT:5432"]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres -d loomcycle"]
      interval: 2s
      retries: 30
YAML

for i in $(seq 1 "$REPLICAS"); do
  HOST_PORT=$(( REPLICA_BASE_PORT + i ))
cat <<YAML
  replica-${i}:
    image: $IMAGE
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      LOOMCYCLE_REPLICA_ID: replica-${i}
      LOOMCYCLE_AUTH_TOKEN: "$LOOMCYCLE_AUTH_TOKEN"
      LOOMCYCLE_STORAGE_BACKEND: postgres
      LOOMCYCLE_PG_DSN: postgres://postgres:${PG_PASSWORD}@postgres:5432/loomcycle?sslmode=disable
      LOOMCYCLE_PG_AUTOMIGRATE: "1"
      LOOMCYCLE_PG_MAX_OPEN_CONNS: "$POOL"
      LOOMCYCLE_LISTEN_ADDR: "0.0.0.0:8787"
      LOOMCYCLE_MOCK_ENABLED: "1"
      LOOMCYCLE_MOCK_LATENCY_MS: "$M_LAT"
      LOOMCYCLE_MOCK_LATENCY_JITTER_MS: "$M_JIT"
      LOOMCYCLE_MOCK_429_RATE: "$M_429"
      LOOMCYCLE_MOCK_500_RATE: "$M_500"
      LOOMCYCLE_METRICS_ENABLED: "1"
      LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS: "1000"
      LOOMCYCLE_METRICS_COLLECT_SYSTEM: "1"
      LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS: "120000"
      LOOMCYCLE_CHANNEL_DEBUG: "$CH_DBG"
      LOOMCYCLE_REPLICAS_SWEEP_INTERVAL_MS: "$REPL_SWEEP_MS"
      LOOMCYCLE_REPLICAS_STALE_AFTER_MS: "$REPL_STALE_MS"
    ports: ["127.0.0.1:${HOST_PORT}:8787"]
    volumes:
      - $GEN_YAML:/etc/loomcycle/loomcycle.yaml:ro
    command: ["--config", "/etc/loomcycle/loomcycle.yaml"]
YAML
done

cat <<YAML
  lb:
    image: nginx:1.27-alpine
    depends_on:
$(for i in $(seq 1 "$REPLICAS"); do echo "      - replica-${i}"; done)
    ports: ["127.0.0.1:$LB_PORT:80"]
    ulimits:
      # Default container nofile (1024) is too low for the
      # circuit-stress launch storm (15K concurrent connections at
      # scale=5000). Without this the LB silently returns 500s with
      # `accept4() failed: No file descriptors available` in its log
      # and the matrix attributes the loss to loomcycle instead.
      nofile:
        soft: 65536
        hard: 65536
    volumes:
      - $NGINX_CONF:/etc/nginx/nginx.conf:ro
YAML
} > "$COMPOSE_FILE"

# ─── Bring up the cluster ───────────────────────────────────────────
dc() { docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"; }
teardown() {
    if [ "$NO_TEARDOWN" = "1" ]; then
        echo "→ --no-teardown: leaving cluster up. Tear down with:"
        echo "    docker compose -p $PROJECT -f $COMPOSE_FILE down -v"
        return
    fi
    echo "→ tearing down cluster…"
    dc logs --no-color > "$RESULTS_DIR/compose.log" 2>&1 || true
    for i in $(seq 1 "$REPLICAS"); do
        dc logs --no-color "replica-${i}" > "$RESULTS_DIR/replica-${i}.log" 2>&1 || true
    done
    dc down -v >/dev/null 2>&1 || true
}
trap teardown EXIT

echo "→ docker compose up (project=$PROJECT)…"
dc up -d >/dev/null

# ─── Wait for cluster readiness: every replica healthy AND lists all N ─
echo "→ waiting for $REPLICAS replicas to register heartbeats…"
DEADLINE=$(( SECONDS + 120 ))
for i in $(seq 1 "$REPLICAS"); do
    PORT=$(( REPLICA_BASE_PORT + i ))
    while :; do
        N=$(curl -fsS -m 2 -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
              "http://127.0.0.1:${PORT}/healthz" 2>/dev/null | jq -r '.replicas | length' 2>/dev/null || echo 0)
        [ "$N" = "$REPLICAS" ] && { echo "  replica-${i} healthy, sees $N replicas"; break; }
        if [ "$SECONDS" -ge "$DEADLINE" ]; then
            echo "✗ replica-${i} never saw all $REPLICAS replicas (last saw: $N). Logs:" >&2
            dc logs --tail 40 "replica-${i}" >&2 || true
            exit 1
        fi
        sleep 2
    done
done
echo "  ✓ all $REPLICAS replicas up and mutually visible"

# ─── Wait for the nginx LB to accept connections ────────────────────
# Replicas being healthy doesn't mean nginx has bound :80 yet — and a
# bad generated upstream would crash-loop nginx, which we want to catch
# here (with logs) rather than as a confusing driver "connection
# refused".
echo "→ waiting for LB on :$LB_PORT…"
LB_DEADLINE=$(( SECONDS + 30 ))
until curl -fsS -m 2 "http://127.0.0.1:$LB_PORT/healthz" >/dev/null 2>&1; do
    if [ "$SECONDS" -ge "$LB_DEADLINE" ]; then
        echo "✗ LB never became reachable on :$LB_PORT. nginx logs:" >&2
        dc logs --tail 30 lb >&2 || true
        exit 1
    fi
    sleep 1
done
echo "  ✓ LB reachable"

# ─── Metrics snapshot (pre) ─────────────────────────────────────────
curl -fsS -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
    "http://127.0.0.1:$LB_PORT/v1/_metrics/summary" \
    >"$RESULTS_DIR/metrics-summary-pre.json" 2>/dev/null || true

# ─── Build the driver ───────────────────────────────────────────────
echo "→ building circuit-stress driver…"
(cd "$CIRCUIT_DIR" && go build -o circuit-stress .)
DRIVER_BIN="$CIRCUIT_DIR/circuit-stress"

# ─── Launch scenario injector (concurrent with the driver) ──────────
INJECTOR_PID=""
if [ "$SCENARIO" = "cancel" ]; then
    PG_DSN="$PG_DSN_HOST" REPLICAS="$REPLICAS" REPLICA_BASE_PORT="$REPLICA_BASE_PORT" \
        LB_PORT="$LB_PORT" RESULTS_DIR="$RESULTS_DIR" \
        bash "$SCRIPT_DIR/inject-cancel.sh" &
    INJECTOR_PID=$!
elif [ "$SCENARIO" = "crash" ]; then
    PROJECT="$PROJECT" COMPOSE_FILE="$COMPOSE_FILE" PG_DSN="$PG_DSN_HOST" \
        REPLICAS="$REPLICAS" RESULTS_DIR="$RESULTS_DIR" \
        bash "$SCRIPT_DIR/inject-crash.sh" &
    INJECTOR_PID=$!
fi

# ─── Run the driver against the LB ──────────────────────────────────
echo "→ running driver against LB :$LB_PORT  (Web UI: http://127.0.0.1:$LB_PORT/ui?token=$LOOMCYCLE_AUTH_TOKEN)"
echo
set +e
"$DRIVER_BIN" "${DRIVER_ARGS[@]}" \
    --results-dir "$RESULTS_DIR" \
    --base-url "http://127.0.0.1:$LB_PORT" \
    --token "$LOOMCYCLE_AUTH_TOKEN"
RC=$?
set -e
if [ -n "$INJECTOR_PID" ]; then
    if [ "$SCENARIO" = "cancel" ]; then
        # Cancel injector loops; tell it to stop.
        kill -TERM "$INJECTOR_PID" 2>/dev/null || true
        wait "$INJECTOR_PID" 2>/dev/null || true
    else
        # Crash injector is bounded (WAIT_TOTAL_S after kill) AND needs
        # its post-kill polling window to finish so the forensic dump
        # captures the full clear-path trajectory. Don't SIGTERM —
        # just wait it out.
        echo "→ waiting for crash injector forensic window…"
        wait "$INJECTOR_PID" 2>/dev/null || true
    fi
fi

# ─── Metrics snapshot (post) + per-replica rollups ──────────────────
curl -fsS -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
    "http://127.0.0.1:$LB_PORT/v1/_metrics/summary" \
    >"$RESULTS_DIR/metrics-summary-post.json" 2>/dev/null || true

echo
echo "→ per-replica analysis (PG_DSN=$PG_DSN_HOST)…"
PG_DSN="$PG_DSN_HOST" RESULTS_DIR="$RESULTS_DIR" REPLICAS="$REPLICAS" \
    bash "$SCRIPT_DIR/analyze-cluster.sh" || echo "  (analysis script reported an issue; see above)"

echo
echo "→ done. Results in $RESULTS_DIR/"
exit $RC
