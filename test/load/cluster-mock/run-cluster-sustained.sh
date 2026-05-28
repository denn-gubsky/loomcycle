#!/usr/bin/env bash
# run-cluster-sustained.sh — sustained-load multi-replica stress.
#
# Brings up an N-replica cluster (same shape as run-cluster-mock.sh)
# and drives the LB with sustained load across a sequence of PHASES.
# Default phase plan: x1000 5 min → x2000 5 min → x1000 5 min (15 min
# total ramp-peak-ramp).
#
# Each phase is a loop firing the circuit-stress driver in waves with
# the phase scale until phase wall time elapses. Per-wave summaries
# are aggregated into waves.csv; the per-second resource series from
# process_samples (with replica_id, Phase-1 substrate change) drives
# the time-series charts produced after teardown.
#
# Why a new orchestrator vs --sustained on run-cluster-mock.sh: the
# burst rig's "build → drive once → teardown" shape is exactly right
# for the matrix sweep; layering sustained semantics on top would
# muddy both. Two scripts, two contracts.
#
# Prereqs:
#   export LOOMCYCLE_AUTH_TOKEN=loomcycle
#
# Usage (from repo root):
#   ./test/load/cluster-mock/run-cluster-sustained.sh
#     --replicas 4 --phases "1000:5,2000:5,1000:5"
#
#   ./test/load/cluster-mock/run-cluster-sustained.sh \
#     --replicas 4 --phases "500:2,1000:2" --circuit-timeout 2m
#
# Output: $RESULTS_DIR/
#   waves/wave-NNNN/                   per-wave driver result dir
#   waves.csv                          per-wave aggregate (phase, scale, p50, p99, completed, failed, wall)
#   process_samples.csv                full per-second per-replica metrics dump
#   metrics-summary-{pre,post}.json    /v1/_metrics/summary snapshots
#   cluster-analysis.txt               post-run rollups
#   docker-compose.gen.yaml + nginx.gen.conf + loomcycle.gen.yaml
#   compose.log + replica-N.log

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CIRCUIT_DIR="$REPO_ROOT/test/load/circuit-stress"
cd "$REPO_ROOT"

# ─── Tunables ───────────────────────────────────────────────────────
REPLICAS="${REPLICAS:-4}"
PHASES="${PHASES:-1000:5,2000:5,1000:5}"    # "scale:minutes,scale:minutes,..."
CIRCUIT_TIMEOUT="${CIRCUIT_TIMEOUT:-2m}"
CIRCUITS_PER_USER="${CIRCUITS_PER_USER:-20}"
PG_PORT="${PG_PORT:-15433}"
LB_PORT="${LB_PORT:-18080}"
REPLICA_BASE_PORT="${REPLICA_BASE_PORT:-18800}"
PG_PASSWORD="${PG_PASSWORD:-loomcycle}"
PG_MAX_CONNECTIONS="${PG_MAX_CONNECTIONS:-200}"
PG_POOL_BUDGET="${PG_POOL_BUDGET:-160}"
IMAGE="${LOOMCYCLE_IMAGE:-loomcycle:mock-local}"
PROJECT="${COMPOSE_PROJECT:-lc-cluster-mock}"
NO_TEARDOWN="${NO_TEARDOWN:-0}"

# ─── Parse args ─────────────────────────────────────────────────────
while [ $# -gt 0 ]; do
    case "$1" in
        --replicas) REPLICAS="$2"; shift 2 ;;
        --replicas=*) REPLICAS="${1#*=}"; shift ;;
        --phases) PHASES="$2"; shift 2 ;;
        --phases=*) PHASES="${1#*=}"; shift ;;
        --circuit-timeout) CIRCUIT_TIMEOUT="$2"; shift 2 ;;
        --circuit-timeout=*) CIRCUIT_TIMEOUT="${1#*=}"; shift ;;
        --circuits-per-user) CIRCUITS_PER_USER="$2"; shift 2 ;;
        --no-teardown) NO_TEARDOWN=1; shift ;;
        *) echo "unknown arg: $1" >&2; exit 1 ;;
    esac
done

require() { command -v "$1" >/dev/null 2>&1 || { echo "✗ missing dependency: $1" >&2; exit 1; }; }
require docker; require go; require jq; require psql
: "${LOOMCYCLE_AUTH_TOKEN:?LOOMCYCLE_AUTH_TOKEN must be set}"
[[ "$REPLICAS" =~ ^[1-9][0-9]*$ ]] || { echo "✗ --replicas must be positive integer" >&2; exit 1; }

# Compute max scale across phases (for channel pre-generation).
MAX_SCALE=0
TOTAL_MIN=0
for phase in ${PHASES//,/ }; do
    s="${phase%:*}"; m="${phase##*:}"
    [ "$s" -gt "$MAX_SCALE" ] && MAX_SCALE="$s"
    TOTAL_MIN=$(( TOTAL_MIN + m ))
done

POOL=$(( PG_POOL_BUDGET / REPLICAS )); [ "$POOL" -lt 10 ] && POOL=10
TOTAL_POOL=$(( POOL * REPLICAS ))

RESULTS_DIR="${RESULTS_DIR:-$SCRIPT_DIR/results/$(date -u +%Y-%m-%dT%H-%M-%SZ)-sustained-r${REPLICAS}}"
mkdir -p "$RESULTS_DIR/waves"
echo "→ results dir: $RESULTS_DIR"
echo "→ topology: replicas=$REPLICAS phases=[$PHASES] (~${TOTAL_MIN} min total) max_scale=$MAX_SCALE"
echo "→ pool per replica=$POOL (total $TOTAL_POOL ≤ max_connections=$PG_MAX_CONNECTIONS)"

PG_DSN_HOST="postgres://postgres:${PG_PASSWORD}@127.0.0.1:${PG_PORT}/loomcycle?sslmode=disable"
COMPOSE_FILE="$RESULTS_DIR/docker-compose.gen.yaml"
NGINX_CONF="$RESULTS_DIR/nginx.gen.conf"
GEN_YAML="$RESULTS_DIR/loomcycle.gen.yaml"

# ─── Build image (cached if no source change) ───────────────────────
echo "→ building $IMAGE from HEAD…"
docker build -q -t "$IMAGE" "$REPO_ROOT" >/dev/null

# ─── Generate cluster yaml with channels c1..MAX_SCALE ──────────────
echo "→ generating cluster yaml for max scale=$MAX_SCALE…"
awk -v scale="$MAX_SCALE" '
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

# ─── Generate nginx upstream + compose ──────────────────────────────
UPSTREAM=""
for i in $(seq 1 "$REPLICAS"); do UPSTREAM+="        server replica-${i}:8787;"$'\n'; done
awk -v servers="$UPSTREAM" '{ gsub(/__UPSTREAM_SERVERS__/, servers); print }' \
    "$SCRIPT_DIR/nginx.conf.tmpl" > "$NGINX_CONF"

M_LAT="${LOOMCYCLE_MOCK_LATENCY_MS:-50}"
M_JIT="${LOOMCYCLE_MOCK_LATENCY_JITTER_MS:-25}"
M_429="${LOOMCYCLE_MOCK_429_RATE:-0.15}"
M_500="${LOOMCYCLE_MOCK_500_RATE:-0}"
CH_DBG="${LOOMCYCLE_CHANNEL_DEBUG:-0}"

{
cat <<YAML
# GENERATED by run-cluster-sustained.sh — do not edit. replicas=$REPLICAS
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
      nofile:
        soft: 65536
        hard: 65536
    volumes:
      - $NGINX_CONF:/etc/nginx/nginx.conf:ro
YAML
} > "$COMPOSE_FILE"

# ─── Bring up the cluster ───────────────────────────────────────────
dc() { docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"; }
TEST_START_ISO=""
teardown() {
    echo "→ capturing logs + final per-replica analysis…"
    dc logs --no-color > "$RESULTS_DIR/compose.log" 2>&1 || true
    for i in $(seq 1 "$REPLICAS"); do
        dc logs --no-color "replica-${i}" > "$RESULTS_DIR/replica-${i}.log" 2>&1 || true
    done
    PG_DSN="$PG_DSN_HOST" RESULTS_DIR="$RESULTS_DIR" REPLICAS="$REPLICAS" \
        bash "$SCRIPT_DIR/analyze-cluster.sh" 2>&1 || true
    # Dump full process_samples for time-series charts BEFORE teardown wipes the volume.
    if [ -n "$TEST_START_ISO" ]; then
        echo "→ dumping process_samples for time-series charts…"
        psql "$PG_DSN_HOST" -c "\COPY (
            SELECT replica_id, sampled_at, active_runs, queued_runs,
                   loomcycle_cpu_pct_x100, loomcycle_rss_bytes,
                   loomcycle_num_goroutines, system_cpu_pct_x100
            FROM process_samples
            WHERE sampled_at >= '$TEST_START_ISO'::timestamptz
            ORDER BY sampled_at, replica_id
        ) TO '$RESULTS_DIR/process_samples.csv' WITH (FORMAT CSV, HEADER true)" 2>/dev/null || true
        ls -l "$RESULTS_DIR/process_samples.csv" 2>/dev/null || true
    fi
    if [ "$NO_TEARDOWN" = "1" ]; then
        echo "→ --no-teardown: leaving cluster up. Tear down with:"
        echo "    docker compose -p $PROJECT -f $COMPOSE_FILE down -v"
    else
        echo "→ tearing down cluster…"
        dc down -v >/dev/null 2>&1 || true
    fi
}
trap teardown EXIT

echo "→ docker compose up…"
dc up -d >/dev/null

# ─── Cluster readiness ──────────────────────────────────────────────
echo "→ waiting for $REPLICAS replicas to register heartbeats…"
DEADLINE=$(( SECONDS + 120 ))
for i in $(seq 1 "$REPLICAS"); do
    PORT=$(( REPLICA_BASE_PORT + i ))
    while :; do
        N=$(curl -fsS -m 2 -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
              "http://127.0.0.1:${PORT}/healthz" 2>/dev/null | jq -r '.replicas | length' 2>/dev/null || echo 0)
        [ "$N" = "$REPLICAS" ] && { echo "  replica-${i} healthy, sees $N replicas"; break; }
        if [ "$SECONDS" -ge "$DEADLINE" ]; then
            echo "✗ replica-${i} never saw all $REPLICAS replicas (last saw: $N)" >&2; exit 1
        fi; sleep 2
    done
done

echo "→ waiting for LB on :$LB_PORT…"
LB_DEADLINE=$(( SECONDS + 30 ))
until curl -fsS -m 2 "http://127.0.0.1:$LB_PORT/healthz" >/dev/null 2>&1; do
    [ "$SECONDS" -ge "$LB_DEADLINE" ] && { echo "✗ LB not reachable" >&2; exit 1; }
    sleep 1
done
echo "  ✓ all replicas + LB up"

# Pre snapshot.
curl -fsS -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
    "http://127.0.0.1:$LB_PORT/v1/_metrics/summary" \
    >"$RESULTS_DIR/metrics-summary-pre.json" 2>/dev/null || true

# Build driver.
echo "→ building circuit-stress driver…"
(cd "$CIRCUIT_DIR" && go build -o circuit-stress .)
DRIVER_BIN="$CIRCUIT_DIR/circuit-stress"

# ─── Sustained-load phase loop ──────────────────────────────────────
TEST_START_ISO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
TEST_START_EPOCH=$SECONDS
WAVES_CSV="$RESULTS_DIR/waves.csv"
echo "phase_idx,phase_scale,wave_idx,wave_start_epoch_offset_s,wave_wall_s,circuits,completed,failed,silent_regression,p50_ms,p95_ms,p99_ms,max_ms" > "$WAVES_CSV"

phase_idx=0
wave_idx=0
echo
echo "════════ STARTING SUSTAINED LOAD ════════"
echo "  start: $TEST_START_ISO   Web UI: http://127.0.0.1:$LB_PORT/ui?token=$LOOMCYCLE_AUTH_TOKEN"
echo

for phase in ${PHASES//,/ }; do
    phase_idx=$(( phase_idx + 1 ))
    P_SCALE="${phase%:*}"
    P_MIN="${phase##*:}"
    P_END=$(( SECONDS + P_MIN * 60 ))
    P_START_OFFSET=$(( SECONDS - TEST_START_EPOCH ))
    echo "── PHASE $phase_idx: scale=$P_SCALE  duration=${P_MIN}m  (offset T+${P_START_OFFSET}s) ──"

    while [ "$SECONDS" -lt "$P_END" ]; do
        wave_idx=$(( wave_idx + 1 ))
        W_DIR="$RESULTS_DIR/waves/$(printf 'wave-%04d' "$wave_idx")"
        mkdir -p "$W_DIR"
        W_START_OFFSET=$(( SECONDS - TEST_START_EPOCH ))
        W_START_EPOCH=$SECONDS

        "$DRIVER_BIN" --scale "$P_SCALE" --circuits-per-user "$CIRCUITS_PER_USER" \
            --circuit-timeout "$CIRCUIT_TIMEOUT" --results-dir "$W_DIR" \
            --base-url "http://127.0.0.1:$LB_PORT" --token "$LOOMCYCLE_AUTH_TOKEN" \
            > "$W_DIR/driver.log" 2>&1 || true

        W_WALL=$(( SECONDS - W_START_EPOCH ))

        # Parse driver summary from its log. Each grep is wrapped in
        # `|| true` because pipefail+errexit otherwise kills the script
        # when a wave has zero silent regressions (no match → grep exits 1,
        # pipefail propagates, errexit terminates). Empty SILENT_LINE = 0.
        SUMMARY_LINE=$(grep -E "^  Circuits:" "$W_DIR/driver.log" 2>/dev/null | head -1 || true)
        DURATION_LINE=$(grep -E "^  Duration:" "$W_DIR/driver.log" 2>/dev/null | head -1 || true)
        SILENT_LINE=$(grep "silent regression" "$W_DIR/driver.log" 2>/dev/null | head -1 || true)

        circuits=$(echo "$SUMMARY_LINE" | grep -oP '\d+(?= total)' || echo 0)
        completed=$(echo "$SUMMARY_LINE" | grep -oP '\d+(?= completed)' || echo 0)
        failed=$(echo "$SUMMARY_LINE" | grep -oP '\d+(?= failed)' || echo 0)
        silent=$(echo "$SILENT_LINE" | grep -oP '⚠ \K\d+' || echo 0)
        p50=$(echo "$DURATION_LINE" | grep -oP 'p50=\K\d+' || echo "")
        p95=$(echo "$DURATION_LINE" | grep -oP 'p95=\K\d+' || echo "")
        p99=$(echo "$DURATION_LINE" | grep -oP 'p99=\K\d+' || echo "")
        max_ms=$(echo "$DURATION_LINE" | grep -oP 'max=\K\d+' || echo "")

        echo "$phase_idx,$P_SCALE,$wave_idx,$W_START_OFFSET,$W_WALL,$circuits,$completed,$failed,$silent,$p50,$p95,$p99,$max_ms" >> "$WAVES_CSV"
        ELAPSED=$(( SECONDS - TEST_START_EPOCH ))
        REMAIN_PHASE=$(( P_END - SECONDS ))
        printf "  wave %4d  T+%4ds  scale=%-5d  wall=%3ds  %d/%d completed  silent=%d  p50=%sms p99=%sms  (phase remaining: %ds)\n" \
            "$wave_idx" "$ELAPSED" "$P_SCALE" "$W_WALL" "$completed" "$circuits" "$silent" "$p50" "$p99" "$REMAIN_PHASE"

        # Tiny inter-wave gap; the next iteration immediately fires the
        # driver again. Effective load is "back-to-back waves until phase end".
        :
    done
    echo "── phase $phase_idx complete  (waves: $wave_idx total so far) ──"
    echo
done

TOTAL_WALL=$(( SECONDS - TEST_START_EPOCH ))
echo "════════ SUSTAINED LOAD COMPLETE: ${TOTAL_WALL}s, $wave_idx waves total ════════"

# Post snapshot.
curl -fsS -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
    "http://127.0.0.1:$LB_PORT/v1/_metrics/summary" \
    >"$RESULTS_DIR/metrics-summary-post.json" 2>/dev/null || true

echo
echo "→ results dir: $RESULTS_DIR"
echo "→ waves CSV:   $WAVES_CSV"
echo "→ process_samples + analysis dump during teardown…"
