package coord

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The dead-replica reap gives back only the per-user slots its failed runs
// took (RFC ES C4): a run whose row records a slot (user_slot TRUE), and a
// top-level run whose row records nothing (written before the column).
// Sub-runs never take one, nor does a run admitted with the per-user cap off;
// decrementing for those took back other runs' slots.
func TestReplicasSweeper_QuotaReapGivesBackOnlyTheSlotsTheRunsTook(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	sessionID := seedSession(t, pool)
	stamp := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	replicaID := testReplicaID(t, "test-rs-slot-")
	userID := "u-slot-" + stamp
	seedStaleReplica(t, pool, replicaID)

	// id -> (user_slot SQL literal, parent run)
	runs := []struct {
		id, slot string
		sub      bool
	}{
		{"r-held-" + stamp, "TRUE", false},
		{"r-capoff-" + stamp, "FALSE", false},
		{"r-legacy-" + stamp, "NULL", false},
		{"r-sublegacy-" + stamp, "NULL", true},
		{"r-sub-" + stamp, "FALSE", true},
	}
	for _, r := range runs {
		seedRun(t, pool, r.id, sessionID, replicaID, userID, "running")
		parent := "NULL"
		if r.sub {
			parent = "'r-held-" + stamp + "'"
		}
		if _, err := pool.Exec(context.Background(),
			`UPDATE runs SET user_slot = `+r.slot+`, parent_run_id = `+parent+` WHERE id = $1`, r.id); err != nil {
			t.Fatalf("stamp %s: %v", r.id, err)
		}
	}
	seedUserQuota(t, pool, userID, 10)

	sw := NewReplicasSweeper(pool, ReplicasSweeperConfig{StaleAfter: 60 * time.Second})
	if _, err := sw.sweepOnce(context.Background()); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	var quota int
	if err := pool.QueryRow(context.Background(),
		`SELECT active_count FROM user_quotas WHERE tenant_id = '' AND user_id = $1`, userID).Scan(&quota); err != nil {
		t.Fatalf("read quota: %v", err)
	}
	if quota != 8 {
		t.Errorf("quota = %d, want 8: the held run's slot and the legacy top-level run's, nothing for the sub-runs or the cap-off run", quota)
	}
}
