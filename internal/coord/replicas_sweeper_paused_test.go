package coord

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A paused run is not running anywhere: its loop parked, and its row waits for
// a resume pass to re-dispatch it — a dead owner is exactly the case that pass
// exists for. Failing it as "owner replica died" would lose a run a shutdown
// parked on purpose. Only the dead replica's other runs are failed — a
// 'pausing' row among them: no resume pass lists one — and only their slots
// are given back here; the paused run's goes back when a resume claims it.
func TestReplicasSweeper_LeavesADeadReplicasPausedRunsForResume(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	sessionID := seedSession(t, pool)
	stamp := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	replicaID := testReplicaID(t, "test-rs-paused-")
	userID := "u-paused-" + stamp
	live, paused, pausing := "r-live-"+stamp, "r-paused-"+stamp, "r-pausing-"+stamp

	seedStaleReplica(t, pool, replicaID)
	seedRun(t, pool, live, sessionID, replicaID, userID, "running")
	seedRun(t, pool, paused, sessionID, replicaID, userID, "running")
	seedRun(t, pool, pausing, sessionID, replicaID, userID, "running")
	for id, state := range map[string]string{paused: "paused", pausing: "pausing"} {
		if _, err := pool.Exec(context.Background(), `UPDATE runs SET pause_state = $2 WHERE id = $1`, id, state); err != nil {
			t.Fatalf("set pause_state: %v", err)
		}
	}
	seedUserQuota(t, pool, userID, 3)

	sw := NewReplicasSweeper(pool, ReplicasSweeperConfig{StaleAfter: 60 * time.Second})
	if _, err := sw.sweepOnce(context.Background()); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}

	read := func(id string) (status, pauseState string) {
		t.Helper()
		if err := pool.QueryRow(context.Background(),
			`SELECT status, COALESCE(pause_state, '') FROM runs WHERE id = $1`, id).Scan(&status, &pauseState); err != nil {
			t.Fatalf("read run %s: %v", id, err)
		}
		return
	}
	for _, id := range []string{live, pausing} {
		if st, _ := read(id); st != "failed" {
			t.Errorf("the dead replica's run %s = %q, want failed", id, st)
		}
	}
	if st, ps := read(paused); st != "running" || ps != "paused" {
		t.Errorf("the dead replica's paused run = status %q pause_state %q, want it left running and paused for resume", st, ps)
	}
	var quota int
	if err := pool.QueryRow(context.Background(),
		`SELECT active_count FROM user_quotas WHERE tenant_id = '' AND user_id = $1`, userID).Scan(&quota); err != nil {
		t.Fatalf("read quota: %v", err)
	}
	if quota != 1 {
		t.Errorf("quota = %d, want 1 (the two failed runs' slots given back; the paused run's waits for its resume)", quota)
	}
}
