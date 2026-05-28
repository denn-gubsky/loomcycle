#!/usr/bin/env bash
# analyze-cluster.sh — per-replica rollups for a cluster-mock run.
#
# Reads the cluster Postgres directly (the same way the single-replica
# load-test analysis read process_samples) and writes a human-readable
# summary + the raw rollup tables into the results dir. Invoked by
# run-cluster-mock.sh after the driver finishes; also runnable
# standalone against a --no-teardown cluster:
#
#   PG_DSN="postgres://postgres:loomcycle@127.0.0.1:15433/loomcycle?sslmode=disable" \
#     RESULTS_DIR=/tmp/x REPLICAS=3 bash analyze-cluster.sh

set -euo pipefail
: "${PG_DSN:?PG_DSN required}"
: "${RESULTS_DIR:?RESULTS_DIR required}"
REPLICAS="${REPLICAS:-?}"
OUT="$RESULTS_DIR/cluster-analysis.txt"

q() { psql "$PG_DSN" -tAF $'\t' -c "$1" 2>/dev/null; }

{
echo "════════ cluster-mock analysis (replicas=$REPLICAS) ════════"
echo

echo "── run status totals ──"
q "SELECT status, count(*) FROM runs GROUP BY status ORDER BY 2 DESC;"
echo

echo "── load distribution: completed runs per replica ──"
echo "(even split across replicas = the LB + ownership stamping work)"
q "SELECT COALESCE(replica_id,'(none)') AS replica, count(*) AS runs
   FROM runs WHERE status='completed' GROUP BY replica_id ORDER BY replica;"
echo

echo "── per-replica resource peaks (process_samples) ──"
echo "(replica_id populated = Phase-1 substrate change working)"
q "SELECT COALESCE(replica_id,'(none)') AS replica,
          count(*) AS samples,
          round(max(loomcycle_cpu_pct_x100)/100.0,0) AS peak_cpu_pct,
          round(avg(loomcycle_cpu_pct_x100)/100.0,0) AS avg_cpu_pct,
          round(max(loomcycle_rss_bytes)/1024.0/1024,0) AS peak_rss_mb,
          max(loomcycle_num_goroutines) AS peak_goros,
          max(active_runs) AS peak_active
   FROM process_samples GROUP BY replica_id ORDER BY replica;"
echo

echo "── cluster-wide system view ──"
q "SELECT round(max(system_cpu_pct_x100)/100.0,0) AS peak_sys_cpu_pct,
          max(system_mem_used_mb) AS peak_sys_mem_mb,
          count(*) AS total_samples
   FROM process_samples;"
echo

echo "── cross-replica circuits (channel fan-out evidence) ──"
echo "(circuits whose agents span ≥2 replicas → editor/evaluator subscribe"
echo " traversed the loomcycle.channel LISTEN/NOTIFY backplane)"
SPAN=$(q "SELECT count(*) FROM (
            SELECT split_part(agent_id,'-',2) AS circuit
            FROM runs WHERE agent_id LIKE '%-c%' AND replica_id IS NOT NULL
            GROUP BY 1 HAVING count(DISTINCT replica_id) >= 2) t;")
TOTAL=$(q "SELECT count(DISTINCT split_part(agent_id,'-',2)) FROM runs WHERE agent_id LIKE '%-c%';")
echo "cross-replica circuits: ${SPAN:-0} / ${TOTAL:-0}"
echo "sample of cross-replica circuits (circuit → replicas):"
q "SELECT split_part(agent_id,'-',2) AS circuit, string_agg(DISTINCT replica_id, ',' ORDER BY replica_id) AS replicas
   FROM runs WHERE agent_id LIKE '%-c%' AND replica_id IS NOT NULL
   GROUP BY 1 HAVING count(DISTINCT replica_id) >= 2 LIMIT 5;"
echo

echo "── crash-recovery markers ──"
echo "(One of three substrate clear-paths fired:"
echo "   replica_died        = coord/replicas_sweeper.go (the explicit"
echo "                         dead-replica reaper — expected path)"
echo "   heartbeat_timeout   = internal/heartbeat/sweeper.go (per-run"
echo "                         heartbeat sweeper — slower, default 10 min)"
echo "   owner_replica_dead  = coord/cancel_coordinator.go (a cancel POST"
echo "                         arrived for a run on a dead replica)"
echo " Empty = no crash this run. Normal completions use end_turn and are"
echo " excluded here.)"
q "SELECT stop_reason, count(*) FROM runs
   WHERE stop_reason IN ('replica_died','heartbeat_timeout','owner_replica_dead')
   GROUP BY stop_reason ORDER BY 2 DESC;"
echo

echo "── stuck-running guard ──"
STUCK=$(q "SELECT count(*) FROM runs WHERE status='running';")
echo "runs still 'running' after test: ${STUCK:-?}  (want 0)"
echo

echo "── user_quotas leak guard ──"
q "SELECT COALESCE(sum(active_count),0) AS total_active_quota,
          count(*) FILTER (WHERE active_count > 0) AS users_with_active
   FROM user_quotas;"
echo "(total_active_quota should be 0 after a clean drain; >0 = quota leak)"
} | tee "$OUT"

echo
echo "→ analysis written to $OUT"
