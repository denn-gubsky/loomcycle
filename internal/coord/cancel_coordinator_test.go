package coord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// stubCancelRunStore implements cancelRunStore for tests without
// standing up a Postgres fixture.
type stubCancelRunStore struct {
	runs       map[string]store.Run // keyed by agent_id
	finishErr  error
	finishCall struct {
		runID  string
		status store.RunStatus
		reason string
	}
}

func (s *stubCancelRunStore) GetRunByAgentID(_ context.Context, agentID string) (store.Run, error) {
	r, ok := s.runs[agentID]
	if !ok {
		return store.Run{}, &store.ErrNotFound{Kind: "run", ID: agentID}
	}
	return r, nil
}

func (s *stubCancelRunStore) FinishRun(_ context.Context, runID string, status store.RunStatus, stopReason string, _ store.Usage, _ string) error {
	s.finishCall.runID = runID
	s.finishCall.status = status
	s.finishCall.reason = stopReason
	if s.finishErr != nil {
		return s.finishErr
	}
	// Update the in-memory row so the timeout-recheck path can see
	// a terminal status if the test set finish before timeout.
	for agentID, r := range s.runs {
		if r.ID == runID {
			r.Status = status
			s.runs[agentID] = r
			break
		}
	}
	return nil
}

func TestCancelCoordinator_LocalFallback_NoClusterCanceller(t *testing.T) {
	// Single-replica path: a Registry with no ClusterCanceller set
	// returns (false) on local miss — unchanged from v0.11.x.
	reg := cancel.NewRegistry()
	res, ok := reg.Cancel("does-not-exist", "test")
	if ok || res.Cancelled {
		t.Errorf("local miss with no cluster canceller should return (false), got (%v, ok=%v)", res, ok)
	}
}

// TestCancelCoordinator_NotFound_ReturnsMiss verifies that a CancelRemote
// for an unknown agent_id returns (CancelResult{}, false, nil) so the
// HTTP handler falls through to the store's 404 path.
func TestCancelCoordinator_NotFound_ReturnsMiss(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	pool := freshUserQuotasPool(t, dsn)
	bp, err := NewPostgresBackplane(PostgresBackplaneConfig{
		Pool: pool, DSN: dsn, ReplicaID: "test-cc-nf-" + time.Now().Format("150405.000"),
	})
	if err != nil {
		t.Fatalf("backplane: %v", err)
	}
	defer bp.Close()

	stub := &stubCancelRunStore{runs: map[string]store.Run{}}
	rs := NewReplicaStore(pool)
	cc, err := NewCancelCoordinator(CancelCoordinatorConfig{
		Backplane:    bp,
		ReplicaID:    "test-cc-nf-A",
		Store:        stub,
		ReplicaStore: rs,
		AckTimeout:   500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}

	res, ok, err := cc.CancelRemote(context.Background(), "unknown", "")
	if err != nil {
		t.Errorf("CancelRemote error: %v", err)
	}
	if ok {
		t.Errorf("ok=true for unknown agent, want false (handler should 404)")
	}
	if res.Cancelled {
		t.Errorf("Cancelled=true for unknown agent, want false")
	}
}

// TestCancelCoordinator_AlreadyTerminal_ReturnsIdempotent verifies a
// CancelRemote for a run that is already in a terminal state returns
// Cancelled=false without broadcasting.
func TestCancelCoordinator_AlreadyTerminal_ReturnsIdempotent(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	pool := freshUserQuotasPool(t, dsn)
	bp, _ := NewPostgresBackplane(PostgresBackplaneConfig{
		Pool: pool, DSN: dsn, ReplicaID: "test-cc-term",
	})
	defer bp.Close()

	stub := &stubCancelRunStore{runs: map[string]store.Run{
		"a_done": {ID: "r1", Status: store.RunCompleted, ReplicaID: "other-replica"},
	}}
	cc, _ := NewCancelCoordinator(CancelCoordinatorConfig{
		Backplane:    bp,
		ReplicaID:    "test-cc-term",
		Store:        stub,
		ReplicaStore: NewReplicaStore(pool),
		AckTimeout:   500 * time.Millisecond,
	})
	res, ok, err := cc.CancelRemote(context.Background(), "a_done", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Errorf("ok=false for already-terminal run, want true (handler should 200 with cancelled=false)")
	}
	if res.Cancelled {
		t.Errorf("Cancelled=true for terminal run, want false (idempotent)")
	}
}

// TestCancelCoordinator_DeadOwner_MarksRunFailed verifies the
// short-circuit: when the owning replica's heartbeat is stale, the
// coordinator marks the run failed in the DB and returns success
// without broadcasting.
func TestCancelCoordinator_DeadOwner_MarksRunFailed(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	pool := freshUserQuotasPool(t, dsn)
	bp, _ := NewPostgresBackplane(PostgresBackplaneConfig{
		Pool: pool, DSN: dsn, ReplicaID: "test-cc-dead",
	})
	defer bp.Close()

	stub := &stubCancelRunStore{runs: map[string]store.Run{
		"a_dead": {ID: "r2", Status: store.RunRunning, ReplicaID: "dead-replica"},
	}}
	rs := NewReplicaStore(pool)
	// Seed a backdated heartbeat row for dead-replica so IsReplicaAlive
	// returns false.
	deadID := "dead-replica-" + time.Now().Format("150405.000")
	stub.runs["a_dead"] = store.Run{ID: "r2", Status: store.RunRunning, ReplicaID: deadID}
	_, _ = pool.Exec(context.Background(),
		`INSERT INTO replicas (id, hostname, started_at, last_heartbeat_at, version)
		 VALUES ($1, 'h', now() - interval '1 hour', now() - interval '10 minutes', 'v')
		 ON CONFLICT (id) DO UPDATE SET last_heartbeat_at = now() - interval '10 minutes'`, deadID)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM replicas WHERE id = $1`, deadID)
	})

	cc, _ := NewCancelCoordinator(CancelCoordinatorConfig{
		Backplane:    bp,
		ReplicaID:    "test-cc-dead",
		Store:        stub,
		ReplicaStore: rs,
		AckTimeout:   500 * time.Millisecond,
	})
	res, ok, err := cc.CancelRemote(context.Background(), "a_dead", "user-stop")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok || !res.Cancelled {
		t.Errorf("dead-owner: got ok=%v Cancelled=%v, want ok=true Cancelled=true", ok, res.Cancelled)
	}
	if res.Reason != "owner_dead_marked_failed" {
		t.Errorf("Reason = %q, want owner_dead_marked_failed", res.Reason)
	}
	if stub.finishCall.runID != "r2" || stub.finishCall.status != store.RunFailed {
		t.Errorf("FinishRun call = %+v, want runID=r2 status=failed", stub.finishCall)
	}
}

// TestCancelCoordinator_Timeout_ReturnsOwnerUnreachable verifies the
// 5s (here 500ms) ack timeout returns the unreachable reason when no
// owning replica responds.
func TestCancelCoordinator_Timeout_ReturnsOwnerUnreachable(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	pool := freshUserQuotasPool(t, dsn)
	bp, _ := NewPostgresBackplane(PostgresBackplaneConfig{
		Pool: pool, DSN: dsn, ReplicaID: "test-cc-timeout",
	})
	defer bp.Close()

	// Owner row: a fake replica with a fresh heartbeat (so the alive
	// check passes and we proceed to broadcast). No subscriber will
	// respond, so we hit timeout.
	freshOwner := "fresh-fake-owner-" + time.Now().Format("150405.000")
	_, _ = pool.Exec(context.Background(),
		`INSERT INTO replicas (id, hostname, started_at, last_heartbeat_at, version)
		 VALUES ($1, 'h', now(), now(), 'v')
		 ON CONFLICT (id) DO UPDATE SET last_heartbeat_at = now()`, freshOwner)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM replicas WHERE id = $1`, freshOwner)
	})

	stub := &stubCancelRunStore{runs: map[string]store.Run{
		"a_timeout": {ID: "r3", Status: store.RunRunning, ReplicaID: freshOwner},
	}}
	rs := NewReplicaStore(pool)
	cc, _ := NewCancelCoordinator(CancelCoordinatorConfig{
		Backplane:    bp,
		ReplicaID:    "test-cc-timeout",
		Store:        stub,
		ReplicaStore: rs,
		AckTimeout:   400 * time.Millisecond,
	})

	start := time.Now()
	res, ok, err := cc.CancelRemote(context.Background(), "a_timeout", "")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Error("ok=false on timeout, want true (handler should 200 with cancelled=false)")
	}
	if res.Cancelled {
		t.Error("Cancelled=true on timeout, want false")
	}
	if res.Reason != "owner_replica_unreachable" {
		t.Errorf("Reason = %q, want owner_replica_unreachable", res.Reason)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("timeout fired too early: %s", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("timeout took too long: %s (recheck context blocked?)", elapsed)
	}
}

// TestCancelCoordinator_NewCoordinator_ValidatesConfig
func TestCancelCoordinator_NewCoordinator_ValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  CancelCoordinatorConfig
		want string
	}{
		{"nil backplane", CancelCoordinatorConfig{}, "backplane"},
		{"nil store", CancelCoordinatorConfig{Backplane: &PostgresBackplane{}}, "store"},
		{"nil replica store", CancelCoordinatorConfig{Backplane: &PostgresBackplane{}, Store: &stubCancelRunStore{}}, "replica store"},
		{"bad replica id", CancelCoordinatorConfig{Backplane: &PostgresBackplane{}, Store: &stubCancelRunStore{}, ReplicaStore: &ReplicaStore{}, ReplicaID: "has space"}, "replica id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCancelCoordinator(tc.cfg)
			if err == nil {
				t.Fatal("expected error")
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || (indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Sentinel: NewCancelCoordinator returns the right error class.
func TestNewCancelCoordinator_ErrorType(t *testing.T) {
	_, err := NewCancelCoordinator(CancelCoordinatorConfig{})
	if err == nil {
		t.Fatal("expected error")
	}
	// Just verify it's a real error (no specific sentinel for this).
	var anyErr error = errors.New("")
	if err == anyErr {
		t.Fatal("unexpected")
	}
}

// TestCancelRegistry_CancelLocal_NeverDelegates pins review-1 finding #1's
// fix: CancelLocal must NEVER call ClusterCanceller, even when one is
// wired. RunCancelSubscriber uses CancelLocal to dispatch incoming
// backplane events — using Cancel would re-broadcast and produce a
// O(replicas × ack_timeout) cascade of cancel events nobody can honor.
func TestCancelRegistry_CancelLocal_NeverDelegates(t *testing.T) {
	// Register a stub ClusterCanceller that PANICS if called — proves
	// CancelLocal never reaches it.
	reg := cancel.NewRegistry()
	reg.SetClusterCanceller(panickingCanceller{})

	res, ok := reg.CancelLocal("does-not-exist", "test")
	if ok || res.Cancelled {
		t.Errorf("CancelLocal on miss should return (false), got (%v, ok=%v)", res, ok)
	}
}

type panickingCanceller struct{}

func (panickingCanceller) CancelRemote(_ context.Context, agentID, _ string) (cancel.CancelResult, bool, error) {
	panic("ClusterCanceller.CancelRemote must not be called from CancelLocal: " + agentID)
}
