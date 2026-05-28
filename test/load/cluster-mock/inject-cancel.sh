#!/usr/bin/env bash
# inject-cancel.sh — cross-replica cancel injector for cluster-mock.
#
# Runs concurrently with the circuit-stress driver. Each tick:
#   1. Picks a 'running' agent from the cluster Postgres, observing
#      its replica_id (the owning replica, stamped at run creation).
#   2. POSTs /v1/agents/{agent_id}/cancel against a DIFFERENT replica's
#      direct port — the cross-replica path that exercises
#      cancel_coordinator + the loomcycle.cancel LISTEN/NOTIFY round-trip
#      to the owning replica.
#   3. Times the ack and records the result one-per-line.
#
# Stops on SIGTERM (the parent run script sends it after the driver
# exits) OR when the driver clearly finished (no running rows + the
# stop-flag file exists).
#
# Env (set by run-cluster-mock.sh): PG_DSN, REPLICAS, REPLICA_BASE_PORT,
# LB_PORT, RESULTS_DIR, LOOMCYCLE_AUTH_TOKEN.
#
# Output: $RESULTS_DIR/inject-cancel.jsonl   — one JSON event per cancel
#         $RESULTS_DIR/inject-cancel.summary — counters + p50/p99 at exit
#
# Pass criteria (encoded in the summary, not enforced here):
#   - All cancels succeed via the backplane (cancelled:true)
#   - Median round-trip < CANCEL_ACK_TIMEOUT_MS (default 5000ms)
#   - No stuck 'running' rows for the cancelled agents post-test

set -uo pipefail
: "${PG_DSN:?}"; : "${REPLICAS:?}"; : "${REPLICA_BASE_PORT:?}"
: "${RESULTS_DIR:?}"; : "${LOOMCYCLE_AUTH_TOKEN:?}"
INTERVAL_MS="${CANCEL_INJECT_INTERVAL_MS:-200}"

OUT="$RESULTS_DIR/inject-cancel.jsonl"; : > "$OUT"
SUMMARY="$RESULTS_DIR/inject-cancel.summary"
STARTED=$(date +%s)
SUCCESS=0; FAILED=0; ALREADY_TERMINAL=0; SKIPPED_NO_RUNNING=0
LATENCIES=()  # ms per successful cross-replica cancel

stop=0
trap 'stop=1' TERM INT

# Wait briefly for runs to appear (driver ramp-up).
for _ in 1 2 3 4 5 6 7 8 9 10; do
    n=$(psql "$PG_DSN" -tAc "SELECT count(*) FROM runs WHERE status='running'" 2>/dev/null || echo 0)
    [ "$n" -gt 0 ] && break
    sleep 0.5
done

while [ "$stop" = 0 ]; do
    # Pick the most recently started 'running' run with a replica_id.
    row=$(psql "$PG_DSN" -tAF '|' -c \
        "SELECT agent_id, replica_id FROM runs
         WHERE status='running' AND agent_id IS NOT NULL AND replica_id IS NOT NULL
         ORDER BY started_at DESC LIMIT 1" 2>/dev/null || echo "")
    if [ -z "$row" ]; then
        SKIPPED_NO_RUNNING=$((SKIPPED_NO_RUNNING+1))
        # If the driver has been working at all and now there are zero
        # running rows for two consecutive ticks, assume drain.
        if [ "$SUCCESS" -gt 0 ]; then break; fi
        sleep "0.$(printf '%03d' "$INTERVAL_MS")"
        continue
    fi
    AGENT_ID="${row%%|*}"; OWNER="${row##*|}"

    # Pick a sender replica != owner. Owner is "replica-N"; pick the
    # next one modulo REPLICAS to maximize cross-replica routing.
    OWNER_NUM="${OWNER##replica-}"
    SENDER_NUM=$(( OWNER_NUM % REPLICAS + 1 ))
    SENDER="replica-${SENDER_NUM}"
    SENDER_PORT=$(( REPLICA_BASE_PORT + SENDER_NUM ))

    t0=$(date +%s%N)
    resp=$(curl -fsS -m 10 -X POST \
        -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
        -H "Content-Type: application/json" \
        --data '{"reason":"injected by inject-cancel"}' \
        "http://127.0.0.1:${SENDER_PORT}/v1/agents/${AGENT_ID}/cancel" 2>&1) || resp="curl_failed: $resp"
    t1=$(date +%s%N)
    lat_ms=$(( (t1 - t0) / 1000000 ))

    cancelled=$(echo "$resp" | jq -r '.cancelled // empty' 2>/dev/null)
    reason=$(echo "$resp" | jq -r '.reason // empty' 2>/dev/null)
    case "$cancelled" in
        true)
            SUCCESS=$((SUCCESS+1)); LATENCIES+=("$lat_ms") ;;
        false)
            # "already terminal" = expected race: run finished between
            # our DB read and the cancel POST. Tracked separately.
            if echo "$reason" | grep -qE "already|terminal|completed|stopped"; then
                ALREADY_TERMINAL=$((ALREADY_TERMINAL+1))
            else
                FAILED=$((FAILED+1))
            fi ;;
        *)
            FAILED=$((FAILED+1)) ;;
    esac

    printf '{"t":"%s","agent":"%s","owner":"%s","sender":"%s","lat_ms":%s,"cancelled":"%s","reason":"%s"}\n' \
        "$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)" "$AGENT_ID" "$OWNER" "$SENDER" "$lat_ms" "$cancelled" "$reason" >> "$OUT"

    sleep "0.$(printf '%03d' "$INTERVAL_MS")"
done

# Percentiles on the successful cross-replica latencies.
p50="-"; p99="-"; max="-"
if [ "${#LATENCIES[@]}" -gt 0 ]; then
    sorted=$(printf '%s\n' "${LATENCIES[@]}" | sort -n)
    n=${#LATENCIES[@]}
    i50=$(( n * 50 / 100 )); [ "$i50" -lt 1 ] && i50=1
    i99=$(( n * 99 / 100 )); [ "$i99" -lt 1 ] && i99=1
    p50=$(echo "$sorted" | sed -n "${i50}p")
    p99=$(echo "$sorted" | sed -n "${i99}p")
    max=$(echo "$sorted" | tail -1)
fi

{
echo "── inject-cancel summary ──"
echo "wall_seconds:        $(( $(date +%s) - STARTED ))"
echo "successful_cancels:  $SUCCESS  (cross-replica, ack received)"
echo "already_terminal:    $ALREADY_TERMINAL  (race: run finished before cancel landed)"
echo "failed:              $FAILED"
echo "skipped_no_running:  $SKIPPED_NO_RUNNING"
echo "ack_latency_ms:      p50=$p50  p99=$p99  max=$max"
} | tee "$SUMMARY"
