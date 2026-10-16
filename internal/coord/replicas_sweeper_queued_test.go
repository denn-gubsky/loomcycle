package coord

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedQueuedRun inserts a queued row owned by replicaID that holds its
// request and both of its dedup keys, as a queued run does while it waits.
func seedQueuedRun(t *testing.T, pool *pgxpool.Pool, runID, sessionID, replicaID, userID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO runs (id, session_id, status, started_at, replica_id, user_id,
			queued_input, idempotency_key, delivery_alt_key)
		VALUES ($1, $2, 'queued', now(), $3, $4, '{"prompt":"x"}'::jsonb, $5, $6)
	`, runID, sessionID, replicaID, userID, "idem-"+runID, "alt-"+runID)
	if err != nil {
		t.Fatalf("seed queued run: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runs WHERE id = $1`, runID)
	})
}

type sweptQueuedRow struct {
	status, errMsg, stopReason string
	hasInput, hasIdem, hasAlt  bool
	completed                  bool
}

func readSweptQueuedRow(t *testing.T, pool *pgxpool.Pool, runID string) sweptQueuedRow {
	t.Helper()
	var r sweptQueuedRow
	if err := pool.QueryRow(context.Background(), `
		SELECT status, COALESCE(error, ''), COALESCE(stop_reason, ''),
		       queued_input IS NOT NULL, idempotency_key IS NOT NULL,
		       delivery_alt_key IS NOT NULL, completed_at IS NOT NULL
		  FROM runs WHERE id = $1`, runID).Scan(
		&r.status, &r.errMsg, &r.stopReason, &r.hasInput, &r.hasIdem, &r.hasAlt, &r.completed); err != nil {
		t.Fatalf("read run: %v", err)
	}
	return r
}

// A dead replica's queued runs have nobody left to start them: no other
// replica restarts a row stamped with another replica's id. They are failed
// with their dedup keys released, so a caller's retry runs the work instead of
// being answered with a run that never moves.
func TestReplicasSweeper_FailsADeadReplicasQueuedRunsAndReleasesTheirKeys(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	sessionID := seedSession(t, pool)
	replicaID := testReplicaID(t, "test-rs-queued-")
	runID := "rq-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "-")

	seedStaleReplica(t, pool, replicaID)
	seedQueuedRun(t, pool, runID, sessionID, replicaID, "u-queued")

	sw := NewReplicasSweeper(pool, ReplicasSweeperConfig{StaleAfter: 60 * time.Second})
	if _, err := sw.sweepOnce(context.Background()); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}

	got := readSweptQueuedRow(t, pool, runID)
	if got.status != "failed" || got.errMsg == "" || !got.completed {
		t.Errorf("dead replica's queued run = status %q error %q completed %v, want failed with a reason and a completion time", got.status, got.errMsg, got.completed)
	}
	if got.hasInput || got.hasIdem || got.hasAlt {
		t.Errorf("dead replica's queued run still holds queued_input %v idempotency_key %v delivery_alt_key %v, want all released", got.hasInput, got.hasIdem, got.hasAlt)
	}
}

// A queued run has not taken a run slot, so failing it must not be counted as
// a slot the dead replica leaked: only its running rows give theirs back.
func TestReplicasSweeper_QueuedRunsOfADeadReplicaReturnNoRunSlot(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	sessionID := seedSession(t, pool)
	replicaID := testReplicaID(t, "test-rs-qslot-")
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "-")
	userID := "u-qslot-" + suffix

	seedStaleReplica(t, pool, replicaID)
	seedRun(t, pool, "rr-"+suffix, sessionID, replicaID, userID, "running")
	seedQueuedRun(t, pool, "rq-"+suffix, sessionID, replicaID, userID)
	// Two slots held: the dead replica's running run, and one run elsewhere.
	seedUserQuota(t, pool, userID, 2)

	sw := NewReplicasSweeper(pool, ReplicasSweeperConfig{StaleAfter: 60 * time.Second})
	if _, err := sw.sweepOnce(context.Background()); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}

	if got := readSweptQueuedRow(t, pool, "rq-"+suffix); got.status != "failed" {
		t.Fatalf("queued run status = %q, want failed", got.status)
	}
	var quotaCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT active_count FROM user_quotas WHERE tenant_id = '' AND user_id = $1`, userID).Scan(&quotaCount); err != nil {
		t.Fatalf("read quota: %v", err)
	}
	if quotaCount != 1 {
		t.Errorf("quota = %d, want 1 (only the running run's slot is returned)", quotaCount)
	}
}
