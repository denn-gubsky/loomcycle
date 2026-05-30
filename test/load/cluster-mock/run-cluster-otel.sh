#!/usr/bin/env bash
# run-cluster-otel.sh — sustained-load multi-replica stress with live
# OTEL observability (Grafana + Tempo + Prometheus + Loki).
#
# Combines the sustained-load phase-loop rig (run-cluster-sustained.sh)
# with Profile A of examples/observability/grafana-tempo/. Mounts the
# observability team's pre-provisioned configs as-is (single source of
# truth — when their dashboards/datasources update, this rig picks them
# up), generates only the per-N-replica prometheus scrape config + the
# compose.
#
# Telemetry pipeline (per the observability RFC):
#
#   replica-1..N ──OTLP/HTTP──▶ otel-collector ──▶ tempo (traces)
#       │                            └─ spanmetrics ──▶ prometheus
#       └──/metrics──▶ prometheus (own substrate counters)
#   grafana (UI on :3001) ──▶ tempo + prometheus + loki
#
# All replicas share one service.name=loomcycle but a unique
# service.instance.id (LOOMCYCLE_REPLICA_ID) so dashboards can split
# per-replica when needed.
#
# Prereqs:
#   export LOOMCYCLE_AUTH_TOKEN=loomcycle
#
# Usage (from repo root):
#   ./test/load/cluster-mock/run-cluster-otel.sh                       # default: r=4, 1000:10,2000:10,1000:10 = 30 min
#   ./test/load/cluster-mock/run-cluster-otel.sh --replicas 4 --phases "1000:5"
#   ./test/load/cluster-mock/run-cluster-otel.sh --no-teardown         # keep stack up to keep browsing Grafana
#
# While the run is in flight, open Grafana at:
#   http://127.0.0.1:3001  (anonymous Viewer, or admin/admin)
#   → Dashboards → Loomcycle → "Loomcycle overview"
#
# Results are written to $RESULTS_DIR/ in the same layout as
# run-cluster-sustained.sh PLUS dashboard-snapshot PNGs fetched from
# Grafana's /render endpoint at teardown.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
CIRCUIT_DIR="$REPO_ROOT/test/load/circuit-stress"
OBS_DIR="$REPO_ROOT/examples/observability/grafana-tempo"
cd "$REPO_ROOT"

# ─── Tunables ───────────────────────────────────────────────────────
REPLICAS="${REPLICAS:-4}"
PHASES="${PHASES:-1000:10,2000:10,1000:10}"     # 30 min default
CIRCUIT_TIMEOUT="${CIRCUIT_TIMEOUT:-2m}"
CIRCUITS_PER_USER="${CIRCUITS_PER_USER:-20}"

# Ports (avoid clashing with the burst rig's lc-cluster-mock project).
PG_PORT="${PG_PORT:-15434}"
LB_PORT="${LB_PORT:-18180}"
REPLICA_BASE_PORT="${REPLICA_BASE_PORT:-18900}"
GRAFANA_PORT="${GRAFANA_PORT:-3001}"
PROMETHEUS_PORT="${PROMETHEUS_PORT:-9090}"
TEMPO_PORT="${TEMPO_PORT:-3200}"
LOKI_PORT="${LOKI_PORT:-3100}"
OTEL_HTTP_PORT="${OTEL_HTTP_PORT:-4318}"

PG_PASSWORD="${PG_PASSWORD:-loomcycle}"
PG_MAX_CONNECTIONS="${PG_MAX_CONNECTIONS:-200}"
PG_POOL_BUDGET="${PG_POOL_BUDGET:-160}"
IMAGE="${LOOMCYCLE_IMAGE:-loomcycle:mock-otel-local}"
PROJECT="${COMPOSE_PROJECT:-lc-cluster-otel}"
NO_TEARDOWN="${NO_TEARDOWN:-0}"

while [ $# -gt 0 ]; do
    case "$1" in
        --replicas) REPLICAS="$2"; shift 2 ;;
        --replicas=*) REPLICAS="${1#*=}"; shift ;;
        --phases) PHASES="$2"; shift 2 ;;
        --phases=*) PHASES="${1#*=}"; shift ;;
        --circuit-timeout) CIRCUIT_TIMEOUT="$2"; shift 2 ;;
        --no-teardown) NO_TEARDOWN=1; shift ;;
        *) echo "unknown arg: $1" >&2; exit 1 ;;
    esac
done

require() { command -v "$1" >/dev/null 2>&1 || { echo "✗ missing dependency: $1" >&2; exit 1; }; }
require docker; require go; require jq; require psql; require curl
: "${LOOMCYCLE_AUTH_TOKEN:?LOOMCYCLE_AUTH_TOKEN must be set}"
[[ "$REPLICAS" =~ ^[1-9][0-9]*$ ]] || { echo "✗ --replicas must be positive integer" >&2; exit 1; }

# Compute max scale + total minutes.
MAX_SCALE=0; TOTAL_MIN=0
for phase in ${PHASES//,/ }; do
    s="${phase%:*}"; m="${phase##*:}"
    [ "$s" -gt "$MAX_SCALE" ] && MAX_SCALE="$s"
    TOTAL_MIN=$(( TOTAL_MIN + m ))
done

POOL=$(( PG_POOL_BUDGET / REPLICAS )); [ "$POOL" -lt 10 ] && POOL=10
TOTAL_POOL=$(( POOL * REPLICAS ))

RESULTS_DIR="${RESULTS_DIR:-$SCRIPT_DIR/results/$(date -u +%Y-%m-%dT%H-%M-%SZ)-otel-r${REPLICAS}}"
mkdir -p "$RESULTS_DIR/waves" "$RESULTS_DIR/snapshots"
echo "→ results dir: $RESULTS_DIR"
echo "→ topology: replicas=$REPLICAS phases=[$PHASES] (~${TOTAL_MIN} min) max_scale=$MAX_SCALE"
echo "→ pool per replica=$POOL (total $TOTAL_POOL ≤ max_connections=$PG_MAX_CONNECTIONS)"
echo

PG_DSN_HOST="postgres://postgres:${PG_PASSWORD}@127.0.0.1:${PG_PORT}/loomcycle?sslmode=disable"
COMPOSE_FILE="$RESULTS_DIR/docker-compose.gen.yaml"
NGINX_CONF="$RESULTS_DIR/nginx.gen.conf"
GEN_YAML="$RESULTS_DIR/loomcycle.gen.yaml"
PROM_CONF="$RESULTS_DIR/prometheus.gen.yml"

# ─── Build image (same as sustained rig — just tag differently) ─────
echo "→ building $IMAGE from HEAD…"
docker build -q -t "$IMAGE" "$REPO_ROOT" >/dev/null

# ─── Cluster yaml + nginx upstream (reuse the sustained rig's logic) ─
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

UPSTREAM=""
for i in $(seq 1 "$REPLICAS"); do UPSTREAM+="        server replica-${i}:8787;"$'\n'; done
awk -v servers="$UPSTREAM" '{ gsub(/__UPSTREAM_SERVERS__/, servers); print }' \
    "$SCRIPT_DIR/nginx.conf.tmpl" > "$NGINX_CONF"

# ─── Generate prometheus.yml with N replica scrape targets ──────────
echo "→ generating prometheus.yml for $REPLICAS replicas…"
{
cat <<YAML
global:
  scrape_interval: 15s
  evaluation_interval: 15s
  external_labels:
    cluster: loomcycle-cluster-otel

scrape_configs:
  # Each replica's /metrics endpoint — substrate counters, per-replica
  # labelled. Prometheus auto-discovers replica_id via the relabel.
YAML
for i in $(seq 1 "$REPLICAS"); do
cat <<YAML
  - job_name: loomcycle-replica-${i}
    static_configs:
      - targets: ["replica-${i}:8787"]
        labels:
          replica_id: replica-${i}
    metrics_path: /metrics
    scheme: http
    authorization:
      type: Bearer
      credentials: ${LOOMCYCLE_AUTH_TOKEN}
YAML
done
cat <<YAML

  # spanmetrics — Prometheus histograms synthesised from OTEL spans
  # by the OTEL Collector's spanmetrics connector (see Profile A RFC).
  - job_name: loomcycle-spanmetrics
    static_configs:
      - targets: ["otel-collector:8889"]
    metrics_path: /metrics
    scheme: http
YAML
} > "$PROM_CONF"

# ─── Generate docker-compose ────────────────────────────────────────
M_LAT="${LOOMCYCLE_MOCK_LATENCY_MS:-50}"
M_JIT="${LOOMCYCLE_MOCK_LATENCY_JITTER_MS:-25}"
M_429="${LOOMCYCLE_MOCK_429_RATE:-0.15}"
M_500="${LOOMCYCLE_MOCK_500_RATE:-0}"

{
cat <<YAML
# GENERATED by run-cluster-otel.sh — do not edit. replicas=$REPLICAS
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

  # ---- telemetry pipeline (mounts from examples/observability/grafana-tempo/) ----

  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.95.0
    command: ["--config=/etc/otelcol/config.yaml"]
    volumes:
      - $OBS_DIR/otel-collector/config.yaml:/etc/otelcol/config.yaml:ro

  tempo:
    image: grafana/tempo:2.4.1
    command: ["-config.file=/etc/tempo/tempo.yaml"]
    volumes:
      - $OBS_DIR/tempo/tempo.yaml:/etc/tempo/tempo.yaml:ro
      - tempo-data:/var/tempo
    ports: ["127.0.0.1:$TEMPO_PORT:3200"]

  prometheus:
    image: prom/prometheus:v2.52.0
    command:
      - --config.file=/etc/prometheus/prometheus.yml
      - --storage.tsdb.retention.time=15d
    volumes:
      - $PROM_CONF:/etc/prometheus/prometheus.yml:ro
      - prometheus-data:/prometheus
    ports: ["127.0.0.1:$PROMETHEUS_PORT:9090"]

  loki:
    image: grafana/loki:2.9.5
    command: ["-config.file=/etc/loki/loki-config.yaml"]
    volumes:
      - $OBS_DIR/loki/loki-config.yaml:/etc/loki/loki-config.yaml:ro
      - loki-data:/loki
    ports: ["127.0.0.1:$LOKI_PORT:3100"]

  grafana:
    image: grafana/grafana:10.4.0
    environment:
      GF_SECURITY_ADMIN_PASSWORD: admin
      GF_AUTH_ANONYMOUS_ENABLED: "true"
      GF_AUTH_ANONYMOUS_ORG_ROLE: Viewer
      GF_FEATURE_TOGGLES_ENABLE: traceqlEditor
      # Built-in image-renderer so /render/d/... works for snapshots.
      GF_RENDERING_SERVER_URL: ""
    volumes:
      - $OBS_DIR/grafana/provisioning:/etc/grafana/provisioning:ro
      - grafana-data:/var/lib/grafana
    ports: ["127.0.0.1:$GRAFANA_PORT:3000"]
    depends_on: [prometheus, tempo, loki]

YAML

for i in $(seq 1 "$REPLICAS"); do
  HOST_PORT=$(( REPLICA_BASE_PORT + i ))
cat <<YAML
  replica-${i}:
    image: $IMAGE
    # Auto-restart past the docker DNS-propagation race against fresh
    # volumes: on rare clean-cluster boots, pg_isready returns ready
    # but the replica's first pg ping resolves "postgres" before the
    # container's network is fully routable. The substrate exits
    # fatally on first-ping failure; on_failure restart bridges the
    # ~2-5s window cleanly. Bounded retries so a real bug still surfaces.
    restart: on-failure:5
    depends_on:
      postgres:
        condition: service_healthy
      otel-collector:
        condition: service_started
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
      # ── OTEL — every replica sends to the same collector. ──
      LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector:4318
      LOOMCYCLE_OTEL_SERVICE_NAME: loomcycle
      LOOMCYCLE_OTEL_TRACES_SAMPLER_RATIO: "1.0"
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

volumes:
  tempo-data:
  prometheus-data:
  loki-data:
  grafana-data:
YAML
} > "$COMPOSE_FILE"

# ─── Bring up the stack ─────────────────────────────────────────────
dc() { docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"; }
TEST_START_ISO=""
teardown() {
    echo "→ capturing logs + final analysis…"
    dc logs --no-color > "$RESULTS_DIR/compose.log" 2>&1 || true
    for i in $(seq 1 "$REPLICAS"); do
        dc logs --no-color "replica-${i}" > "$RESULTS_DIR/replica-${i}.log" 2>&1 || true
    done
    dc logs --no-color otel-collector > "$RESULTS_DIR/otel-collector.log" 2>&1 || true

    PG_DSN="$PG_DSN_HOST" RESULTS_DIR="$RESULTS_DIR" REPLICAS="$REPLICAS" \
        bash "$SCRIPT_DIR/analyze-cluster.sh" 2>&1 || true

    # Capture per-second per-replica resources from process_samples.
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
    fi

    # Try a Grafana render of the dashboard at the END of the run, with
    # the full window. Grafana's /render endpoint requires basic auth.
    if [ -n "$TEST_START_ISO" ]; then
        echo "→ attempting Grafana dashboard render (post-run snapshot)…"
        local now_ms=$(date +%s%3N)
        local start_ms=$(date -d "$TEST_START_ISO" +%s%3N 2>/dev/null || echo 0)
        if [ "$start_ms" != "0" ]; then
            curl -fsS -u admin:admin \
                "http://127.0.0.1:$GRAFANA_PORT/render/d/loomcycle-overview/loomcycle-overview?orgId=1&from=${start_ms}&to=${now_ms}&width=1600&height=2400&kiosk=1" \
                -o "$RESULTS_DIR/snapshots/grafana-overview.png" 2>/dev/null && \
                echo "  ✓ dashboard PNG saved (size=$(stat -c%s $RESULTS_DIR/snapshots/grafana-overview.png 2>/dev/null) bytes)" || \
                echo "  (render not available — Grafana needs the image-renderer plugin for PNG export; use the browser instead)"
        fi
    fi

    if [ "$NO_TEARDOWN" = "1" ]; then
        echo "→ --no-teardown: stack left up. Tear down with:"
        echo "    docker compose -p $PROJECT -f $COMPOSE_FILE down -v"
    else
        echo "→ tearing down stack…"
        dc down -v >/dev/null 2>&1 || true
    fi
}
trap teardown EXIT

echo "→ docker compose up (project=$PROJECT)…"
dc up -d >/dev/null

# ─── Cluster readiness ──────────────────────────────────────────────
echo "→ waiting for $REPLICAS replicas to register heartbeats…"
DEADLINE=$(( SECONDS + 180 ))
for i in $(seq 1 "$REPLICAS"); do
    PORT=$(( REPLICA_BASE_PORT + i ))
    while :; do
        N=$(curl -fsS -m 2 -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
              "http://127.0.0.1:${PORT}/healthz" 2>/dev/null | jq -r '.replicas | length' 2>/dev/null || echo 0)
        [ "$N" = "$REPLICAS" ] && { echo "  replica-${i} healthy"; break; }
        [ "$SECONDS" -ge "$DEADLINE" ] && { echo "✗ replica-${i} not ready" >&2; exit 1; }
        sleep 2
    done
done

echo "→ waiting for LB on :$LB_PORT…"
until curl -fsS -m 2 "http://127.0.0.1:$LB_PORT/healthz" >/dev/null 2>&1; do sleep 1; done

echo "→ waiting for Grafana on :$GRAFANA_PORT…"
until curl -fsS -m 2 "http://127.0.0.1:$GRAFANA_PORT/api/health" >/dev/null 2>&1; do sleep 1; done

echo
echo "════════ STACK UP ════════"
echo "  Grafana:    http://127.0.0.1:$GRAFANA_PORT  (admin/admin, or anonymous Viewer)"
echo "              → Dashboards → Loomcycle → \"Loomcycle overview\""
echo "  Prometheus: http://127.0.0.1:$PROMETHEUS_PORT"
echo "  Tempo:      http://127.0.0.1:$TEMPO_PORT"
echo "  Loomcycle:  http://127.0.0.1:$LB_PORT/ui?token=$LOOMCYCLE_AUTH_TOKEN"
echo

# Pre snapshot of the JSON metrics endpoint.
curl -fsS -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
    "http://127.0.0.1:$LB_PORT/v1/_metrics/summary" \
    >"$RESULTS_DIR/metrics-summary-pre.json" 2>/dev/null || true

# Build driver.
echo "→ building circuit-stress driver…"
(cd "$CIRCUIT_DIR" && go build -o circuit-stress .)
DRIVER_BIN="$CIRCUIT_DIR/circuit-stress"

# ─── Sustained-load phase loop (same as run-cluster-sustained.sh) ───
TEST_START_ISO="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
TEST_START_EPOCH=$SECONDS
WAVES_CSV="$RESULTS_DIR/waves.csv"
echo "phase_idx,phase_scale,wave_idx,wave_start_epoch_offset_s,wave_wall_s,circuits,completed,failed,silent_regression,p50_ms,p95_ms,p99_ms,max_ms" > "$WAVES_CSV"

phase_idx=0; wave_idx=0
echo
echo "════════ STARTING SUSTAINED LOAD (OTEL traces flowing live) ════════"
echo "  start: $TEST_START_ISO"
echo "  open Grafana NOW to watch the dashboards populate."
echo

for phase in ${PHASES//,/ }; do
    phase_idx=$(( phase_idx + 1 ))
    P_SCALE="${phase%:*}"; P_MIN="${phase##*:}"
    P_END=$(( SECONDS + P_MIN * 60 ))
    echo "── PHASE $phase_idx: scale=$P_SCALE  duration=${P_MIN}m  (offset T+$(( SECONDS - TEST_START_EPOCH ))s) ──"

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
    done
    echo "── phase $phase_idx complete  (waves: $wave_idx total) ──"
    echo
done

TOTAL_WALL=$(( SECONDS - TEST_START_EPOCH ))
echo "════════ SUSTAINED LOAD COMPLETE: ${TOTAL_WALL}s, $wave_idx waves total ════════"

curl -fsS -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
    "http://127.0.0.1:$LB_PORT/v1/_metrics/summary" \
    >"$RESULTS_DIR/metrics-summary-post.json" 2>/dev/null || true

echo
echo "→ results dir: $RESULTS_DIR"
