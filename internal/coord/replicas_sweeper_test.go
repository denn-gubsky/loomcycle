package coord

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedSession creates a sessions row for the replicas-sweeper tests
// since runs has a FK on session_id. Returns the session id.
func seedSession(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := "test-sess-" + time.Now().Format("150405.000000")
	_, err := pool.Exec(context.Background(),
		`INSERT INTO sessions (id, tenant_id, user_id, created_at, agent) VALUES ($1, '', '', now(), 'test-agent')`, id)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sessions WHERE id = $1`, id)
	})
	return id
}

func seedStaleReplica(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO replicas (id, hostname, started_at, last_heartbeat_at, version)
		 VALUES ($1, 'h', now() - interval '1 hour', now() - interval '10 minutes', 'v')
		 ON CONFLICT (id) DO UPDATE SET last_heartbeat_at = now() - interval '10 minutes'`, id)
	if err != nil {
		t.Fatalf("seed stale replica: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM replicas WHERE id = $1`, id)
	})
}

func seedRun(t *testing.T, pool *pgxpool.Pool, runID, sessionID, replicaID, userID, status string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO runs (id, session_id, status, started_at, replica_id, user_id)
		VALUES ($1, $2, $3, now(), $4, $5)
	`, runID, sessionID, status, replicaID, userID)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runs WHERE id = $1`, runID)
	})
}

func seedUserQuota(t *testing.T, pool *pgxpool.Pool, userID string, count int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO user_quotas (user_id, active_count, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id) DO UPDATE SET active_count = $2
	`, userID, count)
	if err != nil {
		t.Fatalf("seed quota: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_quotas WHERE user_id = $1`, userID)
	})
}

func TestReplicasSweeper_ReapsStaleReplica(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	sessionID := seedSession(t, pool)

	replicaID := "test-rs-stale-" + time.Now().Format("150405.000000")
	userID := "u-" + time.Now().Format("150405.000000")
	runID := "r-" + time.Now().Format("150405.000000")

	seedStaleReplica(t, pool, replicaID)
	seedRun(t, pool, runID, sessionID, replicaID, userID, "running")
	seedUserQuota(t, pool, userID, 1)

	sw := NewReplicasSweeper(pool, ReplicasSweeperConfig{
		StaleAfter: 60 * time.Second, // 10-min-old heartbeat is stale at this cutoff
	})
	n, err := sw.sweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if n != 1 {
		t.Errorf("reaped = %d, want 1", n)
	}

	// Run should be marked failed.
	var status, errMsg string
	if err := pool.QueryRow(context.Background(),
		`SELECT status, error FROM runs WHERE id = $1`, runID).Scan(&status, &errMsg); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
	if errMsg != "owner replica died" {
		t.Errorf("error = %q, want 'owner replica died'", errMsg)
	}

	// Quota should be decremented.
	var quotaCount int
	_ = pool.QueryRow(context.Background(),
		`SELECT active_count FROM user_quotas WHERE user_id = $1`, userID).Scan(&quotaCount)
	if quotaCount != 0 {
		t.Errorf("quota = %d, want 0", quotaCount)
	}

	// Replica row should be deleted.
	var exists bool
	_ = pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM replicas WHERE id = $1)`, replicaID).Scan(&exists)
	if exists {
		t.Error("replica row not deleted")
	}
}

func TestReplicasSweeper_SkipsFreshReplica(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	replicaID := "test-rs-fresh-" + time.Now().Format("150405.000000")
	// Fresh heartbeat.
	_, _ = pool.Exec(context.Background(), `
		INSERT INTO replicas (id, hostname, started_at, last_heartbeat_at, version)
		VALUES ($1, 'h', now(), now(), 'v')
		ON CONFLICT (id) DO UPDATE SET last_heartbeat_at = now()
	`, replicaID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM replicas WHERE id = $1`, replicaID)
	})

	sw := NewReplicasSweeper(pool, ReplicasSweeperConfig{StaleAfter: 90 * time.Second})
	n, err := sw.sweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if n != 0 {
		t.Errorf("reaped = %d, want 0 (fresh replica)", n)
	}
}

func TestReplicasSweeper_GreatestZeroClampOnQuota(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	sessionID := seedSession(t, pool)
	replicaID := "test-rs-clamp-" + time.Now().Format("150405.000000")
	userID := "u-" + time.Now().Format("150405.000000")
	runID := "r-" + time.Now().Format("150405.000000")

	seedStaleReplica(t, pool, replicaID)
	seedRun(t, pool, runID, sessionID, replicaID, userID, "running")
	// Quota already at 0 — simulate the slot was already released.
	seedUserQuota(t, pool, userID, 0)

	sw := NewReplicasSweeper(pool, ReplicasSweeperConfig{StaleAfter: 60 * time.Second})
	if _, err := sw.sweepOnce(context.Background()); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}

	// Quota should stay at 0 (no CHECK violation, no underflow).
	var quotaCount int
	_ = pool.QueryRow(context.Background(),
		`SELECT active_count FROM user_quotas WHERE user_id = $1`, userID).Scan(&quotaCount)
	if quotaCount != 0 {
		t.Errorf("quota = %d, want 0 (GREATEST clamp)", quotaCount)
	}
}

func TestReplicasSweeper_RunExitsOnContextDone(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	sw := NewReplicasSweeper(pool, ReplicasSweeperConfig{Interval: 100 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := atomic.Bool{}
	go func() {
		sw.Run(ctx)
		done.Store(true)
	}()
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("Run did not exit within 2s of ctx cancel")
}
