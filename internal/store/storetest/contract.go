// Package storetest is the shared behavioural test suite every Store adapter
// runs against. It exists so the SQLite and Postgres adapters can't drift
// silently — every contract here is asserted against both backends in CI.
//
// Adapter packages call Run(t, factory) from their own *_test.go file:
//
//	func TestStoreContract(t *testing.T) {
//	    storetest.Run(t, func(t *testing.T) (store.Store, func()) {
//	        s := newTestStore(t)
//	        return s, func() { _ = s.Close() }
//	    })
//	}
//
// The factory must return a FRESH, empty store on each call. Tests assume
// they own the schema and that no rows exist at start. Cleanup runs after
// the individual test (not the parent t.Run) so each subtest is isolated.
//
// Adapter-specific tests (idempotent re-migration, NULL column verification
// via direct SQL, advisory-lock contention, etc.) stay in the adapter's own
// _test.go file. The contract here is the abstract Store surface only.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Factory builds a fresh Store and a cleanup function. The store must be
// empty (no sessions/runs/events) and isolated from any other concurrent
// store instances the test suite might create. Cleanup releases the
// backend resources (drops temp tables, closes connection pools, etc.).
type Factory func(t *testing.T) (store.Store, func())

// Run executes the full Store contract against the factory. Call this from
// the adapter's own test file. Each subtest gets a fresh Store via the
// factory; one adapter bug surfaces as one failed subtest, not a cascade.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(*testing.T, store.Store)
	}{
		{"CreateAndGetSession", testCreateAndGetSession},
		{"GetSessionNotFound", testGetSessionNotFound},
		{"RunLifecycle", testRunLifecycle},
		{"AppendEventOnUnknownRun", testAppendEventOnUnknownRun},
		{"CreateRunOnUnknownSession", testCreateRunOnUnknownSession},
		{"FinishRunIdempotent", testFinishRunIdempotent},
		{"GetTranscriptEmpty", testGetTranscriptEmpty},
		{"CreateSessionUserIDRoundTrip", testCreateSessionUserIDRoundTrip},
		{"CreateRunIdentityRoundTrip", testCreateRunIdentityRoundTrip},
		{"GetRunByAgentIDNotFound", testGetRunByAgentIDNotFound},
		{"GetRunByAgentIDReturnsMostRecent", testGetRunByAgentIDReturnsMostRecent},
		{"ListActiveRunsByUser", testListActiveRunsByUser},
		{"ListRunsByParentAgentID", testListRunsByParentAgentID},
		{"UpdateHeartbeat", testUpdateHeartbeat},
		{"FinishRunCancelledTerminal", testFinishRunCancelledTerminal},
		{"TranscriptOrderedAcrossRuns", testTranscriptOrderedAcrossRuns},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, cleanup := factory(t)
			defer cleanup()
			tc.fn(t, s)
		})
	}
}

func testCreateAndGetSession(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "tenant-a", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "" || sess.TenantID != "tenant-a" || sess.Agent != "default" {
		t.Errorf("session: %+v", sess)
	}
	if sess.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sess.ID || got.TenantID != "tenant-a" || got.Agent != "default" {
		t.Errorf("got: %+v, want: %+v", got, sess)
	}
}

func testGetSessionNotFound(t *testing.T, s store.Store) {
	_, err := s.GetSession(context.Background(), "s_nope")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v (%T), want *store.ErrNotFound", err, err)
	}
	if nf != nil && nf.Kind != "session" {
		t.Errorf("Kind = %q", nf.Kind)
	}
}

func testRunLifecycle(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")

	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	if run.SessionID != sess.ID || run.Status != store.RunRunning {
		t.Errorf("run: %+v", run)
	}

	for i, payload := range [][]byte{
		[]byte(`{"type":"started"}`),
		[]byte(`{"type":"text","text":"hi"}`),
		[]byte(`{"type":"done","stop_reason":"end_turn"}`),
	} {
		typ := []string{"started", "text", "done"}[i]
		if err := s.AppendEvent(ctx, run.ID, typ, payload); err != nil {
			t.Fatalf("AppendEvent[%d]: %v", i, err)
		}
	}

	if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn",
		store.Usage{InputTokens: 10, OutputTokens: 5, Model: "fake-model"}, ""); err != nil {
		t.Fatal(err)
	}

	transcript, err := s.GetTranscript(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 3 {
		t.Fatalf("transcript len = %d, want 3", len(transcript))
	}
	wantTypes := []string{"started", "text", "done"}
	for i, want := range wantTypes {
		if transcript[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, transcript[i].Type, want)
		}
		if transcript[i].SessionID != sess.ID {
			t.Errorf("event %d session_id = %q", i, transcript[i].SessionID)
		}
		if transcript[i].RunID != run.ID {
			t.Errorf("event %d run_id = %q", i, transcript[i].RunID)
		}
	}
	for i := 1; i < len(transcript); i++ {
		if transcript[i].Seq <= transcript[i-1].Seq {
			t.Errorf("seq not ascending at %d: %d -> %d", i, transcript[i-1].Seq, transcript[i].Seq)
		}
	}
}

func testAppendEventOnUnknownRun(t *testing.T, s store.Store) {
	err := s.AppendEvent(context.Background(), "r_nope", "text", []byte(`{}`))
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v (%T), want *store.ErrNotFound", err, err)
	}
	if nf != nil && nf.Kind != "run" {
		t.Errorf("Kind = %q", nf.Kind)
	}
}

func testCreateRunOnUnknownSession(t *testing.T, s store.Store) {
	_, err := s.CreateRun(context.Background(), "s_nope", store.RunIdentity{})
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("got %v (%T), want *store.ErrNotFound", err, err)
	}
}

// FinishRun is idempotent: calling FinishRun(cancelled) on an
// already-completed run must NOT clobber the terminal status. The
// status='running' guard in the UPDATE clause is what enforces this.
func testFinishRunIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_idem"})

	if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, run.ID, store.RunCancelled, "cancelled", store.Usage{}, ""); err != nil {
		t.Fatalf("idempotent FinishRun should not error: %v", err)
	}
	got, err := s.GetRunByAgentID(ctx, "a_idem")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunCompleted {
		t.Errorf("status clobbered: got %q, want completed", got.Status)
	}
}

func testGetTranscriptEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "default", "")
	transcript, err := s.GetTranscript(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 0 {
		t.Errorf("len = %d, want 0", len(transcript))
	}
}

// Empty userID round-trips as empty-string from GetSession (NULL in the
// underlying column; verified per-adapter via direct SQL).
func testCreateSessionUserIDRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "tenant-x", "agent-x", "user-42")
	if err != nil {
		t.Fatal(err)
	}
	if sess.UserID != "user-42" {
		t.Errorf("CreateSession returned UserID %q, want user-42", sess.UserID)
	}
	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != "user-42" {
		t.Errorf("GetSession UserID = %q, want user-42", got.UserID)
	}

	emptySess, _ := s.CreateSession(ctx, "t", "a", "")
	gotEmpty, err := s.GetSession(ctx, emptySess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotEmpty.UserID != "" {
		t.Errorf("empty UserID round-trip: got %q, want \"\"", gotEmpty.UserID)
	}
}

func testCreateRunIdentityRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "agent-x", "user-1")

	identity := store.RunIdentity{
		AgentID:       "a_top",
		ParentAgentID: "a_grandparent",
		ParentRunID:   "r_grandparent",
		UserID:        "user-1",
	}
	run, err := s.CreateRun(ctx, sess.ID, identity)
	if err != nil {
		t.Fatal(err)
	}
	if run.AgentID != "a_top" || run.UserID != "user-1" || run.ParentAgentID != "a_grandparent" || run.ParentRunID != "r_grandparent" {
		t.Errorf("CreateRun did not return the supplied identity: %+v", run)
	}

	got, err := s.GetRunByAgentID(ctx, "a_top")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != run.ID {
		t.Errorf("GetRunByAgentID returned wrong run: got %s, want %s", got.ID, run.ID)
	}
	if got.AgentID != "a_top" || got.UserID != "user-1" {
		t.Errorf("identity not preserved through GetRunByAgentID: %+v", got)
	}
}

func testGetRunByAgentIDNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, id := range []string{"a_does_not_exist", ""} {
		_, err := s.GetRunByAgentID(ctx, id)
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			t.Errorf("agent_id %q: got %v (%T), want *store.ErrNotFound", id, err, err)
		}
	}
}

func testGetRunByAgentIDReturnsMostRecent(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")

	older, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_reused", UserID: "u"})
	_ = s.FinishRun(ctx, older.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	time.Sleep(2 * time.Millisecond)
	newer, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_reused", UserID: "u"})

	got, err := s.GetRunByAgentID(ctx, "a_reused")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != newer.ID {
		t.Errorf("got run %s, want most-recent %s (older was %s)", got.ID, newer.ID, older.ID)
	}
}

func testListActiveRunsByUser(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "alice")

	r1, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_1", UserID: "alice"})
	r2, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_2", UserID: "alice"})
	_, _ = s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_other", UserID: "bob"})
	_ = s.FinishRun(ctx, r1.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	active, err := s.ListActiveRunsByUser(ctx, "alice", store.RunRunning)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != r2.ID {
		ids := make([]string, len(active))
		for i, r := range active {
			ids[i] = r.ID
		}
		t.Errorf("running for alice: got %v, want [%s]", ids, r2.ID)
	}

	all, _ := s.ListActiveRunsByUser(ctx, "alice", "")
	if len(all) != 2 {
		t.Errorf("all for alice: got %d, want 2", len(all))
	}
	for _, r := range all {
		if r.UserID == "bob" {
			t.Errorf("bob's run leaked into alice's list: %s", r.ID)
		}
	}

	if got, _ := s.ListActiveRunsByUser(ctx, "", store.RunRunning); len(got) != 0 {
		t.Errorf("empty userID should return no rows, got %d", len(got))
	}
}

func testListRunsByParentAgentID(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")

	c1, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_child1", ParentAgentID: "a_parent", UserID: "u"})
	c2, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_child2", ParentAgentID: "a_parent", UserID: "u"})
	_, _ = s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_gc", ParentAgentID: "a_child1", UserID: "u"})

	got, err := s.ListRunsByParentAgentID(ctx, "a_parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (direct children only)", len(got))
	}
	ids := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !ids[c1.ID] || !ids[c2.ID] {
		t.Errorf("expected children %s and %s, got %+v", c1.ID, c2.ID, got)
	}
}

func testUpdateHeartbeat(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_hb", UserID: "u"})

	if err := s.UpdateHeartbeat(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := s.GetRunByAgentID(ctx, "a_hb")
	if first.LastHeartbeatAt.IsZero() {
		t.Fatal("first heartbeat should be non-zero")
	}

	time.Sleep(2 * time.Millisecond)
	_ = s.UpdateHeartbeat(ctx, run.ID)
	second, _ := s.GetRunByAgentID(ctx, "a_hb")
	if !second.LastHeartbeatAt.After(first.LastHeartbeatAt) {
		t.Errorf("heartbeat not monotonic: first=%v second=%v", first.LastHeartbeatAt, second.LastHeartbeatAt)
	}

	// Finish the run, then try another heartbeat — must be a no-op.
	_ = s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, "")
	final, _ := s.GetRunByAgentID(ctx, "a_hb")
	time.Sleep(2 * time.Millisecond)
	_ = s.UpdateHeartbeat(ctx, run.ID)
	afterFinish, _ := s.GetRunByAgentID(ctx, "a_hb")
	if !afterFinish.LastHeartbeatAt.Equal(final.LastHeartbeatAt) {
		t.Errorf("heartbeat advanced after run terminal: %v vs %v",
			afterFinish.LastHeartbeatAt, final.LastHeartbeatAt)
	}
}

func testFinishRunCancelledTerminal(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")
	run, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_x", UserID: "u"})

	if err := s.FinishRun(ctx, run.ID, store.RunCancelled, "user_clicked_stop", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRunByAgentID(ctx, "a_x")
	if got.Status != store.RunCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	if got.StopReason != "user_clicked_stop" {
		t.Errorf("stop_reason = %q", got.StopReason)
	}

	_ = s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, "")
	again, _ := s.GetRunByAgentID(ctx, "a_x")
	if again.Status != store.RunCancelled {
		t.Errorf("late completed should not overwrite cancelled, got %q", again.Status)
	}
}

// Transcript ordering across two runs in the same session — events from
// run B must come after run A's events, regardless of how the adapter
// generates seq values.
func testTranscriptOrderedAcrossRuns(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, _ := s.CreateSession(ctx, "t", "a", "u")

	runA, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{})
	if err := s.AppendEvent(ctx, runA.ID, "text", []byte(`{"i":0}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, runA.ID, "text", []byte(`{"i":1}`)); err != nil {
		t.Fatal(err)
	}
	_ = s.FinishRun(ctx, runA.ID, store.RunCompleted, "end_turn", store.Usage{}, "")

	runB, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{})
	if err := s.AppendEvent(ctx, runB.ID, "text", []byte(`{"i":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, runB.ID, "text", []byte(`{"i":3}`)); err != nil {
		t.Fatal(err)
	}

	transcript, err := s.GetTranscript(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 4 {
		t.Fatalf("len = %d, want 4", len(transcript))
	}
	for i := 1; i < len(transcript); i++ {
		if transcript[i].Seq <= transcript[i-1].Seq {
			t.Errorf("seq not ascending at %d: %d -> %d", i, transcript[i-1].Seq, transcript[i].Seq)
		}
	}
	// First two events belong to run A, second two to run B.
	if transcript[0].RunID != runA.ID || transcript[1].RunID != runA.ID {
		t.Errorf("first two events should belong to runA")
	}
	if transcript[2].RunID != runB.ID || transcript[3].RunID != runB.ID {
		t.Errorf("last two events should belong to runB")
	}
}
