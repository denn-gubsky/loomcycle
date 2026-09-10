package retention

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestSnapshot_SpanIsCopiedOntoFactsBEFORETheSessionIsDeleted.
//
// A fact keeps a live reference to the turn it was distilled from, and
// retrieval can hand that turn back verbatim — for exactly as long as the turn
// exists. The chats sweeper deletes an aged session with its runs and events,
// so that reference dangles, and a fact with a dangling reference and no
// source_quote can no longer answer "why does the store believe this" at all.
//
// FOUR PROPERTIES, and the ordering is the one that cannot be recovered if it
// is wrong:
//
//  1. ORDERING — the snapshot must run while the source is still alive. A crash
//     between snapshot and delete leaves a live source and a fact that can be
//     snapshotted again next tick; the other order leaves a fact with neither
//     the reference nor the span. Asserted by having the snapshot's own Exec
//     look the transcript back up: if the delete had already run it would come
//     back empty.
//  2. KEYED ON run_id — measured on a live store: across 1,096 fact-provenance
//     rows, run_id was 100% populated and session_id 0%, because the queue
//     enqueues with the ingesting run's id and there is no session-id helper on
//     the tools context. Keying on session_id would match nothing.
//  3. NEVER OVERWRITES a span that already exists. One written at extraction
//     time came from the text the extractor actually saw, which is tighter than
//     a whole conversation; replacing it would trade precise evidence for
//     coarse.
//  4. The span carries the conversation's own words.
func TestSnapshot_SpanIsCopiedOntoFactsBEFORETheSessionIsDeleted(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()

	sess, err := st.CreateSession(ctx, "acme", "chat", "u1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_chat"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	seg, _ := json.Marshal([]map[string]any{{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": "I moved to Berlin in July for a new job."}},
	}})
	if err := st.AppendEvent(ctx, run.ID, "user_input", seg); err != nil {
		t.Fatalf("append event: %v", err)
	}

	// The snapshot's Exec re-reads the transcript, which is the ordering proof:
	// a non-empty transcript at that moment means the session had not yet been
	// deleted.
	var sourceAliveAtSnapshot bool
	fake := &fakeSQLMem{
		scopes:   []sqlmem.ScopeKey{{Tenant: "acme", Scope: "user", ScopeID: "u1"}},
		affected: 2,
	}
	sw := New(st, Config{SQLMem: fake, Logger: quietLogger, Now: futureHour})
	fake.onExec = func() {
		ev, err := st.GetTranscript(context.Background(), sess.ID)
		sourceAliveAtSnapshot = err == nil && len(ev) > 0
	}

	sw.snapshotSpansBeforeArchive(ctx, sess.ID)

	if len(fake.execs) != 1 {
		t.Fatalf("snapshot ran %d statement(s), want 1 (one per scope carrying entity content)", len(fake.execs))
	}
	if !sourceAliveAtSnapshot {
		t.Error("the snapshot ran with the transcript already gone — snapshot must precede the " +
			"delete, or a crash between them leaves a fact with neither its reference nor its span")
	}

	got := fake.execs[0]
	if !strings.Contains(got.Statement, "chunk_memory_meta") || !strings.Contains(got.Statement, "source_quote") {
		t.Errorf("statement does not write a fact's span: %q", got.Statement)
	}
	if !strings.Contains(got.Statement, "run_id IN") {
		t.Errorf("statement is not keyed on run_id: %q — session_id is 0%% populated on a live "+
			"store, so keying on it would match nothing", got.Statement)
	}
	if !strings.Contains(got.Statement, "source_quote IS NULL OR source_quote = ''") {
		t.Errorf("statement lacks the never-overwrite guard: %q — a span written at extraction "+
			"time is tighter than a whole conversation and must survive", got.Statement)
	}
	// args[0] is the span; the rest are the run ids.
	if len(got.Args) < 2 {
		t.Fatalf("args = %v, want the span plus at least one run id", got.Args)
	}
	span, _ := got.Args[0].(string)
	if !strings.Contains(span, "I moved to Berlin in July") {
		t.Errorf("span does not carry the conversation's own words: %q", span)
	}
	var sawRun bool
	for _, a := range got.Args[1:] {
		if s, _ := a.(string); s == run.ID {
			sawRun = true
		}
	}
	if !sawRun {
		t.Errorf("the session's run %q is not among the keyed args %v", run.ID, got.Args[1:])
	}
}

// TestSnapshot_NoSQLMemoryIsANoOp — a deployment without SQL Memory has no
// chunk provenance to snapshot, and must not fail the sweep over it.
func TestSnapshot_NoSQLMemoryIsANoOp(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sw := New(st, Config{Logger: quietLogger, Now: futureHour})
	sw.snapshotSpansBeforeArchive(ctx, "s-missing") // must not panic
}

// TestSnapshot_TheSWEEPERSnapshotsBeforeItDeletes.
//
// The test above drives snapshotSpansBeforeArchive directly, so it proves the
// function reads a live source — but it CANNOT catch the sweeper calling it in
// the wrong place. Moving the call after DeleteSessionCascade leaves that test
// green while destroying the property, which makes this second test the one
// that actually pins the ordering.
//
// It runs the real chats sweep against a real store and asserts, from inside
// the snapshot's own write, that the session's transcript is still readable. If
// the delete had already run, the cascade would have taken the events with it.
func TestSnapshot_TheSWEEPERSnapshotsBeforeItDeletes(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()

	sess, err := st.CreateSession(ctx, "acme", "chat", "u1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_chat"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	seg, _ := json.Marshal([]map[string]any{{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": "We celebrated ten years in June."}},
	}})
	if err := st.AppendEvent(ctx, run.ID, "user_input", seg); err != nil {
		t.Fatalf("append event: %v", err)
	}
	// Terminal, so the session is prunable at all.
	if err := st.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	var transcriptAliveAtWrite, sawWrite bool
	fake := &fakeSQLMem{
		scopes:   []sqlmem.ScopeKey{{Tenant: "acme", Scope: "user", ScopeID: "u1"}},
		affected: 1,
	}
	fake.onExec = func() {
		sawWrite = true
		ev, err := st.GetTranscript(context.Background(), sess.ID)
		transcriptAliveAtWrite = err == nil && len(ev) > 0
	}

	// A future clock makes the just-created session "aged" without sleeping.
	sw := New(st, Config{
		ChatsMode: "prune", ChatsMaxAge: time.Minute,
		SQLMem: fake, Logger: quietLogger, Now: futureHour,
	})
	if _, _, err := sw.sweepChatsOnce(ctx); err != nil {
		t.Fatalf("sweepChatsOnce: %v", err)
	}

	if !sawWrite {
		t.Fatalf("the sweep deleted a session without snapshotting any span — a fact derived from " +
			"it keeps a dangling reference and no evidence")
	}
	if !transcriptAliveAtWrite {
		t.Error("the sweeper snapshotted AFTER deleting the session: the span was written from an " +
			"already-empty transcript, so the fact ends up with neither its reference nor its span")
	}
	// And the session really is gone afterwards — otherwise the ordering claim is
	// vacuous because nothing was archived at all.
	if ev, err := st.GetTranscript(ctx, sess.ID); err == nil && len(ev) > 0 {
		t.Errorf("the session was not pruned, so this test did not exercise archival (%d events left)", len(ev))
	}
}
