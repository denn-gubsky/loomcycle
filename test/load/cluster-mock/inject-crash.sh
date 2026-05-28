#!/usr/bin/env bash
# inject-crash.sh — replica-crash injector for cluster-mock.
#
# Mid-load, docker-kills one replica. Captures the full
# (status, stop_reason) distribution for the victim's runs at multiple
# checkpoints, so the analysis can name the code path that actually
# clears the victim's in-flight rows (the dead-replica reaper is only
# ONE possible path — heartbeat sweeper, driver-side cancel via the
# cancel coordinator, run-loop context cancellation on store error,
# etc. all leave different stop_reason signatures).
#
# Outputs:
#   $RESULTS_DIR/inject-crash.jsonl       — timeline events (kill, snapshots)
#   $RESULTS_DIR/inject-crash.summary     — pass/fail markers at exit
#   $RESULTS_DIR/inject-crash-snapshots.csv — full distribution per checkpoint
#   $RESULTS_DIR/inject-crash-final.txt   — post-test forensic dump (replicas
#                                            table, run sample, etc.)
#
# Env (set by run-cluster-mock.sh): PROJECT, COMPOSE_FILE, PG_DSN,
# REPLICAS, RESULTS_DIR.
#
# For the dead-replica reaper to fire in-window, the parent script sets
# LOOMCYCLE_REPLICAS_SWEEP_INTERVAL_MS=5000 +
# LOOMCYCLE_REPLICAS_STALE_AFTER_MS=15000.

set -uo pipefail
: "${PROJECT:?}"; : "${COMPOSE_FILE:?}"; : "${PG_DSN:?}"; : "${REPLICAS:?}"; : "${RESULTS_DIR:?}"

OUT="$RESULTS_DIR/inject-crash.jsonl"; : > "$OUT"
SUMMARY="$RESULTS_DIR/inject-crash.summary"
SNAPSHOTS="$RESULTS_DIR/inject-crash-snapshots.csv"
FORENSIC="$RESULTS_DIR/inject-crash-final.txt"
DELAY_MS="${CRASH_INJECT_DELAY_MS:-3000}"
# Watch a generous window — the prior 30s missed late-firing transitions.
WAIT_TOTAL_S="${CRASH_INJECT_WAIT_TOTAL_S:-90}"

VICTIM_NUM="${CRASH_INJECT_VICTIM:-2}"
[ "$VICTIM_NUM" -gt "$REPLICAS" ] && VICTIM_NUM="$REPLICAS"
VICTIM="replica-${VICTIM_NUM}"
CONTAINER="${PROJECT}-${VICTIM}-1"

log() { printf '{"t":"%s","event":"%s","detail":"%s"}\n' "$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)" "$1" "$2" >> "$OUT"; }

# snapshot_distribution($label, $elapsed) — appends one row per
# (status, stop_reason) bucket for the victim's runs to the CSV.
init_snapshot_csv() {
    echo "checkpoint,elapsed_s,status,stop_reason,run_count" > "$SNAPSHOTS"
}
snapshot_distribution() {
    local label="$1" elapsed="$2"
    psql "$PG_DSN" -tAF, -c "
        SELECT '$label' AS checkpoint, $elapsed AS elapsed_s,
               status, COALESCE(NULLIF(stop_reason,''),'(empty)') AS stop_reason,
               count(*) AS run_count
        FROM runs WHERE replica_id='$VICTIM'
        GROUP BY status, stop_reason
        ORDER BY status, stop_reason;" 2>/dev/null >> "$SNAPSHOTS" || true
}

# Wait for runs to appear, then a bit more so there's state to lose.
for _ in 1 2 3 4 5 6 7 8 9 10; do
    n=$(psql "$PG_DSN" -tAc "SELECT count(*) FROM runs WHERE status='running'" 2>/dev/null || echo 0)
    [ "$n" -gt 0 ] && break
    sleep 0.5
done
sleep "0.$(printf '%03d' "$DELAY_MS")"

init_snapshot_csv

# ─── Pre-kill snapshot ───────────────────────────────────────────────
T0=$SECONDS
PRE_RUNNING=$(psql "$PG_DSN" -tAc \
    "SELECT count(*) FROM runs WHERE status='running' AND replica_id='$VICTIM'" 2>/dev/null || echo 0)
PRE_QUOTA=$(psql "$PG_DSN" -tAc \
    "SELECT COALESCE(sum(active_count),0) FROM user_quotas WHERE active_count > 0" 2>/dev/null || echo 0)
snapshot_distribution "pre_kill" 0
log "pre_kill" "victim=$VICTIM victim_running=$PRE_RUNNING total_active_quota=$PRE_QUOTA"

echo "→ inject-crash: docker kill $CONTAINER (victim_running=$PRE_RUNNING)"
docker kill "$CONTAINER" >/dev/null 2>&1 || { log "kill_failed" "$CONTAINER"; echo "✗ docker kill failed"; exit 1; }
KILL_AT=$SECONDS
log "killed" "$CONTAINER"

# ─── Polling loop: snapshot distribution every 5 s up to WAIT_TOTAL_S ─
#
# Three distinct stop_reasons can mark a victim's running row terminal:
#   replica_died        ← coord/replicas_sweeper.go (the explicit reaper)
#   heartbeat_timeout   ← internal/heartbeat/sweeper.go (per-run sweeper)
#   owner_replica_dead  ← coord/cancel_coordinator.go (cancel arrives for
#                         a run whose owner replica is dead)
# We track each separately and report which path actually cleared the
# victim's in-flight runs.
REAP_OBSERVED=0
REAP_AT_S=""
last_snap=0
while [ $(( SECONDS - KILL_AT )) -lt "$WAIT_TOTAL_S" ]; do
    sleep 2
    elapsed=$(( SECONDS - KILL_AT ))
    if [ $(( elapsed - last_snap )) -ge 5 ]; then
        snapshot_distribution "t+${elapsed}s" "$elapsed"
        last_snap=$elapsed
        # First observation of ANY clear-path marker = reap fired.
        if [ -z "$REAP_AT_S" ]; then
            REAPED=$(psql "$PG_DSN" -tAc "
                SELECT count(*) FROM runs
                WHERE replica_id='$VICTIM' AND status='failed'
                  AND stop_reason IN ('replica_died','heartbeat_timeout','owner_replica_dead')" 2>/dev/null || echo 0)
            if [ "$REAPED" -gt 0 ]; then
                REAP_AT_S=$elapsed
                REAP_OBSERVED=$REAPED
                log "reap_observed" "reaped=$REAPED elapsed=${elapsed}s"
            fi
        fi
        STILL_RUNNING=$(psql "$PG_DSN" -tAc \
            "SELECT count(*) FROM runs WHERE replica_id='$VICTIM' AND status='running'" 2>/dev/null || echo "?")
        if [ "$STILL_RUNNING" = "0" ] && [ "$elapsed" -ge 20 ]; then
            log "all_drained" "elapsed=${elapsed}s"
            break
        fi
    fi
done

# Final snapshot.
elapsed=$(( SECONDS - KILL_AT ))
snapshot_distribution "final" "$elapsed"

# ─── Forensic dump ───────────────────────────────────────────────────
{
echo "════════ post-crash forensic dump ════════"
echo "VICTIM: $VICTIM   container: $CONTAINER   pre_running: $PRE_RUNNING"
echo
echo "── replicas table (is the victim row still there?) ──"
psql "$PG_DSN" -c "SELECT id, hostname,
       to_char(last_heartbeat_at AT TIME ZONE 'UTC','HH24:MI:SS') AS last_hb_utc,
       round(EXTRACT(EPOCH FROM (now() - last_heartbeat_at)),1) AS stale_seconds
   FROM replicas ORDER BY id;" 2>/dev/null
echo
echo "── victim's run rows: full (status, stop_reason) breakdown ──"
psql "$PG_DSN" -c "SELECT status, COALESCE(NULLIF(stop_reason,''),'(empty)') AS stop_reason,
       count(*) AS runs
   FROM runs WHERE replica_id='$VICTIM'
   GROUP BY status, stop_reason ORDER BY runs DESC;" 2>/dev/null
echo
echo "── victim's run rows: error_msg fingerprint (top 10) ──"
psql "$PG_DSN" -c "SELECT count(*) AS runs,
       COALESCE(NULLIF(stop_reason,''),'(empty)') AS stop_reason,
       LEFT(COALESCE(NULLIF(error_msg,''),'(no error_msg)'), 110) AS error_fingerprint
   FROM runs WHERE replica_id='$VICTIM'
   GROUP BY stop_reason, error_fingerprint ORDER BY runs DESC LIMIT 10;" 2>/dev/null
echo
echo "── 5 sample victim run rows (oldest by completed_at) ──"
psql "$PG_DSN" -c "SELECT id, status, stop_reason,
       to_char(started_at AT TIME ZONE 'UTC','HH24:MI:SS') AS started_utc,
       to_char(completed_at AT TIME ZONE 'UTC','HH24:MI:SS') AS completed_utc,
       to_char(last_heartbeat_at AT TIME ZONE 'UTC','HH24:MI:SS') AS last_hb_utc,
       LEFT(COALESCE(NULLIF(error_msg,''),''),60) AS error_msg
   FROM runs WHERE replica_id='$VICTIM' AND status != 'running'
   ORDER BY completed_at NULLS LAST LIMIT 5;" 2>/dev/null
echo
echo "── user_quotas (any nonzero = leak) ──"
psql "$PG_DSN" -c "SELECT COALESCE(sum(active_count),0) AS total_active,
       count(*) FILTER (WHERE active_count > 0) AS users_with_active
   FROM user_quotas;" 2>/dev/null
} > "$FORENSIC"

# ─── Summary ─────────────────────────────────────────────────────────
POST_STUCK=$(psql "$PG_DSN" -tAc \
    "SELECT count(*) FROM runs WHERE status='running' AND replica_id='$VICTIM'" 2>/dev/null || echo "?")
POST_REAPED=$(psql "$PG_DSN" -tAc \
    "SELECT count(*) FROM runs
     WHERE replica_id='$VICTIM' AND status='failed'
       AND stop_reason IN ('replica_died','heartbeat_timeout','owner_replica_dead')" 2>/dev/null || echo 0)
POST_QUOTA=$(psql "$PG_DSN" -tAc \
    "SELECT COALESCE(sum(active_count),0) FROM user_quotas WHERE active_count > 0" 2>/dev/null || echo "?")
TOP_REASON=$(psql "$PG_DSN" -tAc "
    SELECT COALESCE(NULLIF(stop_reason,''),'(empty)') || ' x ' || count(*)
    FROM runs WHERE replica_id='$VICTIM' AND status <> 'running'
    GROUP BY stop_reason ORDER BY count(*) DESC LIMIT 1;" 2>/dev/null || echo "?")

PASS_REAP="FAIL"; [ "$REAP_OBSERVED" -gt 0 ] && PASS_REAP="PASS"
PASS_STUCK="FAIL"; [ "$POST_STUCK" = "0" ] && PASS_STUCK="PASS"

{
echo "── inject-crash summary ──"
echo "victim:                    $VICTIM (container $CONTAINER)"
echo "delay_ms_before_kill:      $DELAY_MS"
echo "wait_total_s:              $WAIT_TOTAL_S  (elapsed: ${elapsed}s)"
echo "victim_running_pre_kill:   $PRE_RUNNING"
echo "reap_observed_count:       $REAP_OBSERVED       [$PASS_REAP]"
echo "reap_first_seen_s:         ${REAP_AT_S:-(never)}"
echo "victim_stuck_running_post: $POST_STUCK         [$PASS_STUCK]  (want 0)"
echo "total_active_quota_post:   $POST_QUOTA"
echo "pre_active_quota:          $PRE_QUOTA"
echo "top_stop_reason:           $TOP_REASON"
echo
echo "→ full (status,stop_reason) distribution per checkpoint: $SNAPSHOTS"
echo "→ post-test forensic dump:                              $FORENSIC"
} | tee "$SUMMARY"
