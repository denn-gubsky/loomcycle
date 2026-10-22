package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// runSetFixture is one tenant's worth of rows in every table DeleteRunSet
// touches, plus the usage ledger it must leave alone. census() LISTS those
// tables, so a test compares what is stored before and after a delete rather
// than believing the counts the delete returned.
type runSetFixture struct {
	t      *testing.T
	s      store.Store
	tenant string
	nonce  string
	runs   []string // every run the fixture ever created, deleted or not
	evals  map[string]bool
}

func newRunSetFixture(t *testing.T, s store.Store) *runSetFixture {
	t.Helper()
	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	return &runSetFixture{t: t, s: s, tenant: "treedel_" + nonce, nonce: nonce, evals: map[string]bool{}}
}

func (f *runSetFixture) session() store.Session {
	f.t.Helper()
	sess, err := f.s.CreateSession(context.Background(), f.tenant, "lead", "alice")
	if err != nil {
		f.t.Fatalf("CreateSession: %v", err)
	}
	if err := f.s.SessionEmbedUpsert(context.Background(), sess.ID, store.SessionEmbedding{
		Provider: "eval", Model: "m", Dimension: 2, Vector: []float32{1, 0},
	}); err != nil {
		f.t.Fatalf("SessionEmbedUpsert: %v", err)
	}
	return sess
}

// run creates a run in sess with one row in each per-run table. finish=false
// leaves it running.
func (f *runSetFixture) run(sess store.Session, parentRunID string, finish bool) store.Run {
	f.t.Helper()
	ctx := context.Background()
	n := len(f.runs)
	r, err := f.s.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: fmt.Sprintf("a_%s_%d", f.nonce, n), ParentRunID: parentRunID, UserID: "alice", TenantID: f.tenant,
	})
	if err != nil {
		f.t.Fatalf("CreateRun: %v", err)
	}
	f.runs = append(f.runs, r.ID)
	for _, typ := range []string{"user_input", "text"} {
		if err := f.s.AppendEvent(ctx, r.ID, typ, []byte(`{"text":"canary-`+r.ID+`"}`)); err != nil {
			f.t.Fatalf("AppendEvent: %v", err)
		}
	}
	if _, err := f.s.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: store.MintInterruptID(time.Now()) + fmt.Sprintf("_%d", n), RunID: r.ID, UserID: "alice",
		Question: "Q?", Priority: store.InterruptPriorityNormal, CreatedAt: time.Now(),
	}); err != nil {
		f.t.Fatalf("InterruptCreate: %v", err)
	}
	f.eval(r.ID, "")
	f.pending(sess.ID, r.ID)
	if err := f.s.RecordCallUsage(ctx, store.TokenUsageRow{
		RunID: r.ID, SessionID: sess.ID, TenantID: f.tenant, Provider: "p", Model: "m",
		CredentialSource: "operator", InputTokens: 10,
	}); err != nil {
		f.t.Fatalf("RecordCallUsage: %v", err)
	}
	if finish {
		if err := f.s.FinishRun(ctx, r.ID, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, ""); err != nil {
			f.t.Fatalf("FinishRun: %v", err)
		}
	}
	return r
}

// eval files an evaluation OF runID, emitted by emitterRunID ("" = external).
func (f *runSetFixture) eval(runID, emitterRunID string) string {
	f.t.Helper()
	// The id starts with the judged run's, so a census row says whose it is.
	id := fmt.Sprintf("%s/ev%d", runID, len(f.evals))
	if _, err := f.s.EvaluationSubmit(context.Background(), store.EvaluationRow{
		EvalID: id, RunID: runID, Score: 0.5, EmitterRole: "external", EmitterRunID: emitterRunID,
	}); err != nil {
		f.t.Fatalf("EvaluationSubmit: %v", err)
	}
	f.evals[id] = true
	return id
}

// runSetPendingSeq keeps pending ids unique across the contract's parallel
// subtests.
var runSetPendingSeq atomic.Int64

// pending enqueues a consolidation row naming the session and, when given,
// the run.
func (f *runSetFixture) pending(sessionID, runID string) string {
	f.t.Helper()
	// The id starts with the run it names, else the session.
	seq := runSetPendingSeq.Add(1)
	owner := runID
	if owner == "" {
		owner = sessionID
	}
	id := fmt.Sprintf("%s/mp%d", owner, seq)
	if err := f.s.MemoryPendingEnqueue(context.Background(), store.MemoryPendingRow{
		ID: id, TenantID: f.tenant, Scope: store.MemoryScopeUser, ScopeID: "alice",
		Payload: json.RawMessage(`{"m":"hi"}`), SourceSessionID: sessionID, SourceRunID: runID,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		f.t.Fatalf("MemoryPendingEnqueue: %v", err)
	}
	return id
}

// census lists what the fixture's tenant holds, table by table.
func (f *runSetFixture) census() map[string][]string {
	f.t.Helper()
	ctx := context.Background()
	out := map[string][]string{}
	add := func(table, row string) { out[table] = append(out[table], row) }

	sessions, _, err := f.s.ListSessions(ctx,
		store.SessionFilter{TenantID: f.tenant, IncludeConfigured: true, IncludeArchived: true}, 1000, 0)
	if err != nil {
		f.t.Fatalf("ListSessions: %v", err)
	}
	for _, sess := range sessions {
		add("sessions", sess.SessionID)
		runs, err := f.s.RunsForSession(ctx, sess.SessionID)
		if err != nil {
			f.t.Fatalf("RunsForSession: %v", err)
		}
		for _, r := range runs {
			add("runs", r.ID)
		}
	}
	events, _, err := f.s.ListEvents(ctx, store.EventFilter{TenantID: f.tenant}, 1000, 0)
	if err != nil {
		f.t.Fatalf("ListEvents: %v", err)
	}
	for _, ev := range events {
		add("events", fmt.Sprintf("%s/%s/%d", ev.RunID, ev.Type, ev.Seq))
	}
	embedded, err := f.s.SessionEmbedSearch(ctx, store.SessionFilter{TenantID: f.tenant}, []float32{1, 0}, 1000)
	if err != nil {
		f.t.Fatalf("SessionEmbedSearch: %v", err)
	}
	for _, e := range embedded {
		add("session_embeddings", e.SessionID)
	}
	// By every run the fixture EVER made: a row that outlived its run must
	// still be found here.
	for _, runID := range f.runs {
		ints, err := f.s.InterruptListByRun(ctx, runID, "")
		if err != nil {
			f.t.Fatalf("InterruptListByRun: %v", err)
		}
		for _, in := range ints {
			add("interrupts", in.RunID+"/"+in.InterruptID)
		}
		usage, err := f.s.TokenUsageForRun(ctx, runID)
		if err != nil {
			f.t.Fatalf("TokenUsageForRun: %v", err)
		}
		for range usage {
			add("token_usage", runID)
		}
	}
	evals, err := f.s.SnapshotReadEvaluations(ctx)
	if err != nil {
		f.t.Fatalf("SnapshotReadEvaluations: %v", err)
	}
	for _, ev := range evals {
		if f.evals[ev.EvalID] {
			add("evaluations", ev.EvalID)
		}
	}
	pending, err := f.s.SnapshotReadMemoryPending(ctx)
	if err != nil {
		f.t.Fatalf("SnapshotReadMemoryPending: %v", err)
	}
	for _, p := range pending {
		if p.TenantID == f.tenant {
			add("memory_pending", p.ID)
		}
	}
	for _, rows := range out {
		sort.Strings(rows)
	}
	return out
}

// without returns census c minus every row that is, or belongs to, one of the
// ids. The usage ledger is exempt: it is kept whatever is deleted.
func without(c map[string][]string, ids []string) map[string][]string {
	out := map[string][]string{}
	for table, list := range c {
	next:
		for _, row := range list {
			for _, id := range ids {
				if row == id || strings.HasPrefix(row, id+"/") {
					continue next
				}
			}
			out[table] = append(out[table], row)
		}
	}
	out["token_usage"] = c["token_usage"]
	return out
}

func assertCensus(t *testing.T, what string, got, want map[string][]string) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	for _, table := range []string{"sessions", "runs", "events", "session_embeddings", "interrupts", "evaluations", "memory_pending", "token_usage"} {
		if !reflect.DeepEqual(got[table], want[table]) {
			t.Errorf("%s: table %s holds\n  %v\nwant\n  %v", what, table, got[table], want[table])
		}
	}
}

// DeleteRunSet removes the named runs' rows from every table and nothing
// else: an unrelated tree, a session's other run, and the usage ledger stay.
func testDeleteRunSetRemovesExactlyTheSetsRows(t *testing.T, s store.Store) {
	ctx := context.Background()
	f := newRunSetFixture(t, s)

	// The tree: root → child → grandchild, each in its own session.
	sessA, sessA1, sessA2 := f.session(), f.session(), f.session()
	rA := f.run(sessA, "", true)
	rA1 := f.run(sessA1, rA.ID, true)
	rA2 := f.run(sessA2, rA1.ID, true)
	// A queue row naming the child's SESSION and no run: it goes with the session.
	orphanPending := f.pending(sessA1.ID, "")

	// A continued session: one run is in the tree, the other is not. The
	// session, its embedding and the other run's rows must stay.
	sessC := f.session()
	rC1 := f.run(sessC, rA.ID, true)
	rC2 := f.run(sessC, "", true)
	keptPending := f.pending(sessC.ID, "")

	// An unrelated tree in the same tenant.
	sessB, sessB1 := f.session(), f.session()
	rB := f.run(sessB, "", true)
	rB1 := f.run(sessB1, rB.ID, true)
	// A judgement the deleted root filed ABOUT the unrelated run belongs to
	// that run's record.
	evalByDeleted := f.eval(rB.ID, rA.ID)

	before := f.census()
	if n := len(before["runs"]); n != 7 {
		t.Fatalf("fixture has %d runs, want 7 (the census is not listing them)", n)
	}
	if n := len(before["events"]); n != 14 {
		t.Fatalf("fixture has %d events, want 14", n)
	}

	set := []string{rA.ID, rA1.ID, rA2.ID, rC1.ID}
	got, err := s.DeleteRunSet(ctx, set, nil)
	if err != nil {
		t.Fatalf("DeleteRunSet: %v", err)
	}
	wantRemoved := []string{sessA.ID, sessA1.ID, sessA2.ID}
	sort.Strings(wantRemoved)
	want := store.RunSetDeletion{
		Runs: 4, Events: 8, Sessions: 3, SessionEmbeddings: 3,
		Interrupts: 4, Evaluations: 4, MemoryPending: 5, RemovedSessionIDs: wantRemoved,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DeleteRunSet = %+v\nwant %+v", got, want)
	}

	after := f.census()
	expect := without(before, append(append([]string{}, set...), wantRemoved...))
	assertCensus(t, "after the delete", after, expect)
	if len(after["token_usage"]) != 7 {
		t.Errorf("usage rows after = %d, want all 7 kept", len(after["token_usage"]))
	}
	// Spelled out, the cases the census comparison covers.
	if hasRow(after["memory_pending"], orphanPending) {
		t.Errorf("the queue row naming only a removed session is still there")
	}
	if !hasRow(after["memory_pending"], keptPending) {
		t.Errorf("the queue row naming only the surviving session was deleted")
	}
	if !hasRow(after["evaluations"], evalByDeleted) {
		t.Errorf("the evaluation of an unrelated run was deleted because a deleted run emitted it")
	}
	if len(after["runs"]) != 3 || len(after["events"]) != 6 || len(after["evaluations"]) != 4 || len(after["memory_pending"]) != 4 {
		t.Errorf("left %d runs, %d events, %d evaluations, %d queue rows; want 3, 6, 4, 4",
			len(after["runs"]), len(after["events"]), len(after["evaluations"]), len(after["memory_pending"]))
	}

	// The surviving session still reads back, with only its other run's events.
	if _, err := s.GetSession(ctx, sessC.ID); err != nil {
		t.Errorf("the continued session was removed though a run of it is left: %v", err)
	}
	evs, err := s.GetTranscript(ctx, sessC.ID)
	if err != nil {
		t.Fatalf("GetTranscript: %v", err)
	}
	for _, ev := range evs {
		if ev.RunID != rC2.ID {
			t.Errorf("surviving session still holds an event of run %s", ev.RunID)
		}
	}
	if len(evs) != 2 {
		t.Errorf("surviving session has %d events, want the other run's 2", len(evs))
	}
	for _, id := range set {
		if _, err := s.GetRun(ctx, id); err == nil {
			t.Errorf("run %s still present", id)
		}
	}
	_ = rB1

	// A second call finds nothing and changes nothing.
	again, err := s.DeleteRunSet(ctx, set, nil)
	if err != nil {
		t.Fatalf("second DeleteRunSet: %v", err)
	}
	if !reflect.DeepEqual(again, store.RunSetDeletion{}) {
		t.Errorf("second DeleteRunSet = %+v, want all zero", again)
	}
	assertCensus(t, "after the second delete", f.census(), expect)
}

func hasRow(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// A set holding a run that is running, paused or pausing is refused whole,
// and so is a set that is no longer a whole tree. A never-started draft is
// not live.
func testDeleteRunSetRefusesALiveSet(t *testing.T, s store.Store) {
	ctx := context.Background()
	f := newRunSetFixture(t, s)
	sessA, sessA1 := f.session(), f.session()
	root := f.run(sessA, "", true)
	child := f.run(sessA1, root.ID, false) // still running
	before := f.census()

	refused := func(what string, ids []string, wantID string, wantOutside bool) {
		t.Helper()
		_, err := s.DeleteRunSet(ctx, ids, nil)
		var live *store.ErrRunLive
		if !errors.As(err, &live) {
			t.Fatalf("%s: DeleteRunSet err = %v, want *store.ErrRunLive", what, err)
		}
		if live.RunID != wantID || live.Outside != wantOutside {
			t.Errorf("%s: refusal names %q outside=%v, want %q outside=%v", what, live.RunID, live.Outside, wantID, wantOutside)
		}
		assertCensus(t, what, f.census(), before)
	}
	refused("a running child", []string{root.ID, child.ID}, child.ID, false)
	// The root alone is finished, but its child is not in the set.
	refused("a child outside the set", []string{root.ID}, child.ID, true)

	if err := s.FinishRun(ctx, child.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{store.PauseStatePausing, store.PauseStatePaused} {
		if err := s.SetRunPauseState(ctx, child.ID, state); err != nil {
			t.Fatalf("SetRunPauseState(%s): %v", state, err)
		}
		refused("a "+state+" child", []string{root.ID, child.ID}, child.ID, false)
	}
	if err := s.SetRunPauseState(ctx, child.ID, store.PauseStateRunning); err != nil {
		t.Fatal(err)
	}

	// A draft never ran: it is deleted with the tree.
	sessD := f.session()
	draft, err := s.CreateConfiguredRun(ctx, sessD.ID,
		store.RunIdentity{AgentID: "a_draft_" + f.nonce, ParentRunID: root.ID, UserID: "alice", TenantID: f.tenant},
		json.RawMessage(`{"agent":"lead"}`))
	if err != nil {
		t.Fatalf("CreateConfiguredRun: %v", err)
	}
	got, err := s.DeleteRunSet(ctx, []string{root.ID, child.ID, draft.ID}, nil)
	if err != nil {
		t.Fatalf("DeleteRunSet with a draft: %v", err)
	}
	if got.Runs != 3 || got.Sessions != 3 {
		t.Errorf("deleted %d runs and %d sessions, want 3 and 3", got.Runs, got.Sessions)
	}
	left := f.census()
	if len(left["runs"])+len(left["sessions"])+len(left["events"]) != 0 {
		t.Errorf("rows left after deleting the whole fixture: %v", left)
	}
}

// sessionIDs names sessions to remove when they hold no run: an empty one
// goes, one whose runs are not in the set stays.
func testDeleteRunSetRemovesANamedSessionOnlyWhenEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	f := newRunSetFixture(t, s)
	empty, busy := f.session(), f.session()
	kept := f.run(busy, "", true)
	queued := f.pending(empty.ID, "")

	got, err := s.DeleteRunSet(ctx, nil, []string{empty.ID, busy.ID, "s_no_such_session"})
	if err != nil {
		t.Fatalf("DeleteRunSet: %v", err)
	}
	want := store.RunSetDeletion{Sessions: 1, SessionEmbeddings: 1, MemoryPending: 1, RemovedSessionIDs: []string{empty.ID}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DeleteRunSet = %+v\nwant %+v", got, want)
	}
	c := f.census()
	if !reflect.DeepEqual(c["sessions"], []string{busy.ID}) || !reflect.DeepEqual(c["runs"], []string{kept.ID}) {
		t.Errorf("left sessions %v runs %v, want only the session that holds a run", c["sessions"], c["runs"])
	}
	if hasRow(c["memory_pending"], queued) {
		t.Errorf("the queue row naming the removed session is still there")
	}
}
