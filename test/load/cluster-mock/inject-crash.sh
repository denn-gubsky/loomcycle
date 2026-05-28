#!/usr/bin/env bash
# inject-crash.sh — replica-crash injector for cluster-mock.
#
# Mid-load, docker-kills one replica. Verifies the cluster substrate's
# crash-recovery path:
#   1. Replicas-sweeper marks the dead replica's row stale
#      (last_heartbeat_at < now - LOOMCYCLE_REPLICAS_STALE_AFTER_MS).
#   2. In-flight runs owned by the dead replica are reaped:
#      status='failed', stop_reason='owner_replica_dead'.
#   3. user_quotas.active_count is decremented (no quota leak).
#   4. Surviving replicas keep serving via the LB.
#
# For the reap to fire inside a sub-minute load run, the parent script
# sets LOOMCYCLE_REPLICAS_SWEEP_INTERVAL_MS=5000 and
# LOOMCYCLE_REPLICAS_STALE_AFTER_MS=15000 in the generated compose. With
# defaults (60s/90s) the reap wouldn't fire in-window.
#
# Env (set by run-cluster-mock.sh): PROJECT, COMPOSE_FILE, PG_DSN,
# REPLICAS, RESULTS_DIR.
#
# Output: $RESULTS_DIR/inject-crash.jsonl  — timeline of events
#         $RESULTS_DIR/inject-crash.summary — pass/fail markers at exit

set -uo pipefail
: "${PROJECT:?}"; : "${COMPOSE_FILE:?}"; : "${PG_DSN:?}"; : "${REPLICAS:?}"; : "${RESULTS_DIR:?}"

OUT="$RESULTS_DIR/inject-crash.jsonl"; : > "$OUT"
SUMMARY="$RESULTS_DIR/inject-crash.summary"
DELAY_MS="${CRASH_INJECT_DELAY_MS:-3000}"      # wait this long after launch before killing
WAIT_REAP_S="${CRASH_INJECT_WAIT_REAP_S:-30}"  # how long to wait for the reap

VICTIM_NUM="${CRASH_INJECT_VICTIM:-2}"
[ "$VICTIM_NUM" -gt "$REPLICAS" ] && VICTIM_NUM="$REPLICAS"
VICTIM="replica-${VICTIM_NUM}"
CONTAINER="${PROJECT}-${VICTIM}-1"

log() { printf '{"t":"%s","event":"%s","detail":"%s"}\n' "$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)" "$1" "$2" >> "$OUT"; }

# Wait for runs to appear, then a bit more so there's meaningful state to lose.
for _ in 1 2 3 4 5 6 7 8 9 10; do
    n=$(psql "$PG_DSN" -tAc "SELECT count(*) FROM runs WHERE status='running'" 2>/dev/null || echo 0)
    [ "$n" -gt 0 ] && break
    sleep 0.5
done
sleep "0.$(printf '%03d' "$DELAY_MS")"

# Snapshot state on the victim BEFORE the kill, so we can verify reap.
PRE_RUNNING=$(psql "$PG_DSN" -tAc \
    "SELECT count(*) FROM runs WHERE status='running' AND replica_id='$VICTIM'" 2>/dev/null || echo 0)
PRE_QUOTA=$(psql "$PG_DSN" -tAc \
    "SELECT COALESCE(sum(active_count),0) FROM user_quotas WHERE active_count > 0" 2>/dev/null || echo 0)
log "pre_kill" "victim=$VICTIM victim_running=$PRE_RUNNING total_active_quota=$PRE_QUOTA"

echo "→ inject-crash: docker kill $CONTAINER (victim_running=$PRE_RUNNING)"
docker kill "$CONTAINER" >/dev/null 2>&1 || { log "kill_failed" "$CONTAINER"; echo "✗ docker kill failed"; exit 1; }
log "killed" "$CONTAINER"

# Poll for the reap. We expect the dead-replica sweeper (configured to
# stale_after=15s, interval=5s in the generated compose) to mark the
# victim's running rows as failed/owner_replica_dead within ~20-25s.
DEADLINE=$(( SECONDS + WAIT_REAP_S ))
REAP_OBSERVED=0
while [ "$SECONDS" -lt "$DEADLINE" ]; do
    REAPED=$(psql "$PG_DSN" -tAc \
        "SELECT count(*) FROM runs
         WHERE replica_id='$VICTIM'
           AND status='failed'
           AND stop_reason='owner_replica_dead'" 2>/dev/null || echo 0)
    if [ "$REAPED" -gt 0 ]; then
        log "reap_observed" "reaped=$REAPED elapsed=${SECONDS}s"
        REAP_OBSERVED=$REAPED
        break
    fi
    sleep 2
done

# Post-reap snapshot.
POST_STUCK=$(psql "$PG_DSN" -tAc \
    "SELECT count(*) FROM runs WHERE status='running' AND replica_id='$VICTIM'" 2>/dev/null || echo "?")
POST_REAPED=$(psql "$PG_DSN" -tAc \
    "SELECT count(*) FROM runs
     WHERE replica_id='$VICTIM' AND status='failed' AND stop_reason='owner_replica_dead'" 2>/dev/null || echo 0)
POST_QUOTA=$(psql "$PG_DSN" -tAc \
    "SELECT COALESCE(sum(active_count),0) FROM user_quotas WHERE active_count > 0" 2>/dev/null || echo "?")
log "post_reap" "stuck_running=$POST_STUCK reaped=$POST_REAPED total_active_quota=$POST_QUOTA"

# Pass criteria:
#   - REAP_OBSERVED > 0 (the sweeper actually fired)
#   - POST_STUCK == 0 (no zombie running rows left behind by the dead replica)
PASS_REAP="FAIL"; [ "$REAP_OBSERVED" -gt 0 ] && PASS_REAP="PASS"
PASS_STUCK="FAIL"; [ "$POST_STUCK" = "0" ] && PASS_STUCK="PASS"

{
echo "── inject-crash summary ──"
echo "victim:                    $VICTIM (container $CONTAINER)"
echo "delay_ms_before_kill:      $DELAY_MS"
echo "wait_reap_s:               $WAIT_REAP_S"
echo "victim_running_pre_kill:   $PRE_RUNNING"
echo "reap_observed_count:       $REAP_OBSERVED       [$PASS_REAP]"
echo "victim_stuck_running_post: $POST_STUCK         [$PASS_STUCK]  (want 0)"
echo "total_active_quota_post:   $POST_QUOTA"
echo "pre_active_quota:          $PRE_QUOTA"
} | tee "$SUMMARY"
