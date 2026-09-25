package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC DI D5 — configured (created, not started) runs.

func newDraft(t *testing.T, s store.Store, tenant, user, agentID string) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, tenant, "default", user)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := s.CreateConfiguredRun(ctx, sess.ID,
		store.RunIdentity{AgentID: agentID, UserID: user, TenantID: tenant},
		json.RawMessage(`{"segments":[{"role":"user"}]}`))
	if err != nil {
		t.Fatalf("CreateConfiguredRun: %v", err)
	}
	return run
}

// A draft is a run in status configured that holds its raw request; FinishRun
// cannot end it; start makes it an ordinary running run with what start
// resolved, and clears the draft.
func testConfiguredRunLifecycle(t *testing.T, s store.Store) {
	ctx := context.Background()
	run := newDraft(t, s, "t", "alice", "a_draft")
	if run.Status != store.RunConfigured {
		t.Fatalf("created status = %q, want configured", run.Status)
	}
	got, err := s.GetRun(ctx, run.ID)
	if err != nil || got.Status != store.RunConfigured {
		t.Fatalf("GetRun = %+v (%v), want configured", got, err)
	}
	draft, err := s.GetRunDraft(ctx, run.ID)
	if err != nil || !jsonEqual(draft, `{"segments":[{"role":"user"}]}`) {
		t.Errorf("GetRunDraft = %q (%v)", draft, err)
	}
	if err := s.UpdateRunDraft(ctx, run.ID, json.RawMessage(`{"segments":[],"metadata":{"k":"v"}}`)); err != nil {
		t.Fatalf("UpdateRunDraft: %v", err)
	}
	if draft, _ := s.GetRunDraft(ctx, run.ID); !jsonEqual(draft, `{"segments":[],"metadata":{"k":"v"}}`) {
		t.Errorf("draft after update = %q", draft)
	}

	// FinishRun moves only running rows: a draft is not ended by it.
	if err := s.FinishRun(ctx, run.ID, store.RunFailed, "", store.Usage{}, "boom"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if got, _ := s.GetRun(ctx, run.ID); got.Status != store.RunConfigured {
		t.Errorf("FinishRun changed a draft to %q", got.Status)
	}

	createdAt := run.StartedAt
	time.Sleep(5 * time.Millisecond)
	started, err := s.StartConfiguredRun(ctx, run.ID, store.RunIdentity{
		Model: "m-1", AgentDefID: "def-1", UserTier: "pro", Interactive: true,
		RunConfig: json.RawMessage(`{"tool_choice":{"mode":"required"}}`),
	}, nil)
	if err != nil {
		t.Fatalf("StartConfiguredRun: %v", err)
	}
	if started.Status != store.RunRunning || started.Model != "m-1" || started.AgentDefID != "def-1" ||
		started.UserTier != "pro" || !started.Interactive || !jsonEqual(started.RunConfig, `{"tool_choice":{"mode":"required"}}`) {
		t.Errorf("started run = %+v, want running with the start-time identity", started)
	}
	if !started.StartedAt.After(createdAt) {
		t.Errorf("started_at %v not re-stamped past creation %v", started.StartedAt, createdAt)
	}
	if started.AgentID != "a_draft" || started.UserID != "alice" || started.TenantID != "t" {
		t.Errorf("identity fixed at create changed: %+v", started)
	}
	if _, err := s.GetRunDraft(ctx, run.ID); !errors.Is(err, store.ErrRunNotConfigured) {
		t.Errorf("GetRunDraft after start = %v, want ErrRunNotConfigured", err)
	}
	// Now an ordinary run: FinishRun ends it.
	if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRun(ctx, run.ID); got.Status != store.RunCompleted {
		t.Errorf("status after finish = %q", got.Status)
	}
}

// Of two starts exactly one wins; a draft op on a started run, or on no run,
// says which of the two it is.
func testConfiguredRunStartIsGuarded(t *testing.T, s store.Store) {
	ctx := context.Background()
	run := newDraft(t, s, "t", "bob", "a_guard")
	if _, err := s.StartConfiguredRun(ctx, run.ID, store.RunIdentity{}, nil); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, err := s.StartConfiguredRun(ctx, run.ID, store.RunIdentity{}, nil); !errors.Is(err, store.ErrRunNotConfigured) {
		t.Errorf("second start = %v, want ErrRunNotConfigured", err)
	}
	if err := s.UpdateRunDraft(ctx, run.ID, json.RawMessage(`{}`)); !errors.Is(err, store.ErrRunNotConfigured) {
		t.Errorf("UpdateRunDraft on a running run = %v, want ErrRunNotConfigured", err)
	}
	var nf *store.ErrNotFound
	if _, err := s.StartConfiguredRun(ctx, "r_missing", store.RunIdentity{}, nil); !errors.As(err, &nf) {
		t.Errorf("start of a missing run = %v, want ErrNotFound", err)
	}
	if err := s.UpdateRunDraft(ctx, "r_missing", json.RawMessage(`{}`)); !errors.As(err, &nf) {
		t.Errorf("UpdateRunDraft of a missing run = %v, want ErrNotFound", err)
	}
	if _, err := s.GetRunDraft(ctx, "r_missing"); !errors.As(err, &nf) {
		t.Errorf("GetRunDraft of a missing run = %v, want ErrNotFound", err)
	}
	if err := s.DeleteConfiguredRun(ctx, "r_missing"); !errors.As(err, &nf) {
		t.Errorf("DeleteConfiguredRun of a missing run = %v, want ErrNotFound", err)
	}
}

// Discarding a draft takes its session with it; a running run cannot be
// discarded (cancel is its verb) and is left intact.
func testConfiguredRunDeleteTakesItsSession(t *testing.T, s store.Store) {
	ctx := context.Background()
	run := newDraft(t, s, "t", "carol", "a_del")
	if err := s.DeleteConfiguredRun(ctx, run.ID); err != nil {
		t.Fatalf("DeleteConfiguredRun: %v", err)
	}
	var nf *store.ErrNotFound
	if _, err := s.GetRun(ctx, run.ID); !errors.As(err, &nf) {
		t.Errorf("run after delete: %v, want ErrNotFound", err)
	}
	if _, err := s.GetSession(ctx, run.SessionID); !errors.As(err, &nf) {
		t.Errorf("session after delete: %v, want ErrNotFound", err)
	}

	sess, _ := s.CreateSession(ctx, "t", "default", "carol")
	live, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_live"})
	if err := s.DeleteConfiguredRun(ctx, live.ID); !errors.Is(err, store.ErrRunNotConfigured) {
		t.Errorf("DeleteConfiguredRun of a running run = %v, want ErrRunNotConfigured", err)
	}
	if got, err := s.GetRun(ctx, live.ID); err != nil || got.Status != store.RunRunning {
		t.Errorf("running run after refused delete = %+v (%v)", got, err)
	}
}

// The per-user draft count is per (tenant, user); the expiry sweep takes only
// drafts older than the cutoff, never a running run however old.
func testConfiguredRunCountAndExpiry(t *testing.T, s store.Store) {
	ctx := context.Background()
	old := newDraft(t, s, "t1", "dan", "a_old")
	newDraft(t, s, "t2", "dan", "a_other_tenant")
	sess, _ := s.CreateSession(ctx, "t1", "default", "dan")
	running, _ := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_running", UserID: "dan", TenantID: "t1"})
	time.Sleep(5 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(5 * time.Millisecond)
	fresh := newDraft(t, s, "t1", "dan", "a_fresh")

	if n, err := s.CountConfiguredRuns(ctx, "t1", "dan"); err != nil || n != 2 {
		t.Errorf("CountConfiguredRuns(t1, dan) = %d (%v), want 2", n, err)
	}
	if n, _ := s.CountConfiguredRuns(ctx, "t2", "dan"); n != 1 {
		t.Errorf("CountConfiguredRuns(t2, dan) = %d, want 1", n)
	}
	swept, err := s.SweepExpiredConfiguredRuns(ctx, cutoff)
	if err != nil || swept != 2 {
		t.Fatalf("SweepExpiredConfiguredRuns = %d (%v), want the two drafts made before the cutoff", swept, err)
	}
	var nf *store.ErrNotFound
	if _, err := s.GetRun(ctx, old.ID); !errors.As(err, &nf) {
		t.Errorf("expired draft survived: %v", err)
	}
	if got, err := s.GetRun(ctx, fresh.ID); err != nil || got.Status != store.RunConfigured {
		t.Errorf("fresh draft = %+v (%v), want it kept", got, err)
	}
	if got, err := s.GetRun(ctx, running.ID); err != nil || got.Status != store.RunRunning {
		t.Errorf("running run = %+v (%v), want it untouched by the draft sweep", got, err)
	}
}

// A draft's session is not a chat until the draft starts.
func testConfiguredRunHiddenFromChatListings(t *testing.T, s store.Store) {
	ctx := context.Background()
	draft := newDraft(t, s, "tl", "erin", "a_hidden")
	sess, _ := s.CreateSession(ctx, "tl", "default", "erin")
	if _, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{UserID: "erin", TenantID: "tl"}); err != nil {
		t.Fatal(err)
	}
	listed := func() map[string]bool {
		rows, _, err := s.ListSessions(ctx, store.SessionFilter{TenantID: "tl"}, 50, 0)
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		out := map[string]bool{}
		for _, r := range rows {
			out[r.SessionID] = true
		}
		return out
	}
	got := listed()
	if got[draft.SessionID] || !got[sess.ID] {
		t.Errorf("listed = %v, want the chat but not the draft's session", got)
	}
	if _, err := s.StartConfiguredRun(ctx, draft.ID, store.RunIdentity{}, nil); err != nil {
		t.Fatal(err)
	}
	if !listed()[draft.SessionID] {
		t.Error("a started draft's session is still hidden")
	}
}

// A caller that must account for everything a subject owns — erasure, the
// directory's count — opts in and sees the draft's session, in the page and in
// the total.
func testConfiguredRunListedWhenIncluded(t *testing.T, s store.Store) {
	ctx := context.Background()
	draft := newDraft(t, s, "ti", "fay", "a_included")
	sess, _ := s.CreateSession(ctx, "ti", "default", "fay")
	if _, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{UserID: "fay", TenantID: "ti"}); err != nil {
		t.Fatal(err)
	}
	rows, total, err := s.ListSessions(ctx, store.SessionFilter{TenantID: "ti", UserID: "fay", IncludeConfigured: true}, 50, 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.SessionID] = true
	}
	if !got[draft.SessionID] || !got[sess.ID] || total != 2 {
		t.Errorf("listed = %v (total %d), want both the chat and the draft's session", got, total)
	}
}

// A start reads the draft before it waits for admission. An edit that lands
// in between must not be discarded: the start conditioned on the draft it read
// is refused with ErrDraftChanged and leaves the row a draft holding the edit;
// a start conditioned on the current draft goes through.
func testConfiguredRunStartIsConditionalOnTheDraft(t *testing.T, s store.Store) {
	ctx := context.Background()
	run := newDraft(t, s, "tc", "hal", "a_cond")
	read, err := s.GetRunDraft(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	edited := json.RawMessage(`{"agent":"a","segments":[{"role":"user","content":[{"type":"trusted-text","text":"edited"}]}]}`)
	if err := s.UpdateRunDraft(ctx, run.ID, edited); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartConfiguredRun(ctx, run.ID, store.RunIdentity{}, read); !errors.Is(err, store.ErrDraftChanged) {
		t.Fatalf("start with the pre-edit draft = %v, want ErrDraftChanged", err)
	}
	got, err := s.GetRun(ctx, run.ID)
	if err != nil || got.Status != store.RunConfigured {
		t.Fatalf("row after the refused start = %+v (%v), want still configured", got.Status, err)
	}
	current, err := s.GetRunDraft(ctx, run.ID)
	if err != nil || !jsonEqual(current, string(edited)) {
		t.Fatalf("draft after the refused start = %s (%v), want the edit kept", current, err)
	}
	if started, err := s.StartConfiguredRun(ctx, run.ID, store.RunIdentity{}, current); err != nil || started.Status != store.RunRunning {
		t.Errorf("start with the current draft = %+v (%v), want running", started.Status, err)
	}
}

// A draft is not a crashed run: the stale sweeper leaves it alone, and once it
// starts, the re-stamped heartbeat keeps a draft that waited a long time from
// being reaped as one that never heartbeated.
func testConfiguredRunNotReapedAsStale(t *testing.T, s store.Store) {
	ctx := context.Background()
	run := newDraft(t, s, "t", "fay", "a_stale")
	time.Sleep(5 * time.Millisecond)
	cutoff := time.Now()
	if _, err := s.SweepStaleRuns(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRun(ctx, run.ID); got.Status != store.RunConfigured {
		t.Errorf("stale sweep changed a draft to %q", got.Status)
	}
	if _, err := s.StartConfiguredRun(ctx, run.ID, store.RunIdentity{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SweepStaleRuns(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRun(ctx, run.ID); got.Status != store.RunRunning {
		t.Errorf("a just-started draft was reaped as stale: %q", got.Status)
	}
}

// A NUL in a run's answer or in a draft's prompt is model- or caller-produced
// text the store must hold, not a reason to refuse the write: a FinishRun that
// failed would leave a finished run `running` with its answer lost. The
// character may be replaced (JSONB cannot store U+0000) but the text around it
// survives and the run is terminal.
func testRunJSONColumnsAcceptNUL(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "tn", "default", "gus")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_nul", UserID: "gus", TenantID: "tn",
		RunConfig: json.RawMessage(`{"sampling":{"stop":["x\u0000y"]}}`)})
	if err != nil {
		t.Fatalf("CreateRun with a NUL in run_config: %v", err)
	}
	result := json.RawMessage(`{"final_text":"before\u0000after","state":{"k":"v\u0000w"}}`)
	if err := s.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{Result: result}, ""); err != nil {
		t.Fatalf("FinishRun with a NUL in the result: %v", err)
	}
	got, err := s.GetRun(ctx, run.ID)
	if err != nil || got.Status != store.RunCompleted {
		t.Fatalf("run after FinishRun = %+v (%v), want completed", got.Status, err)
	}
	var rec struct {
		FinalText string `json:"final_text"`
	}
	if err := json.Unmarshal(got.Result, &rec); err != nil || !strings.HasPrefix(rec.FinalText, "before") || !strings.HasSuffix(rec.FinalText, "after") {
		t.Errorf("result = %s (%v), want the text around the NUL kept", got.Result, err)
	}

	draftBody := json.RawMessage(`{"agent":"a","segments":[{"role":"user","content":[{"type":"trusted-text","text":"p\u0000q"}]}]}`)
	draft, err := s.CreateConfiguredRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_nul_draft", UserID: "gus", TenantID: "tn"}, draftBody)
	if err != nil {
		t.Fatalf("CreateConfiguredRun with a NUL in the draft: %v", err)
	}
	if err := s.UpdateRunDraft(ctx, draft.ID, draftBody); err != nil {
		t.Fatalf("UpdateRunDraft with a NUL in the draft: %v", err)
	}
	if d, err := s.GetRunDraft(ctx, draft.ID); err != nil || !json.Valid(d) || !strings.Contains(string(d), "trusted-text") {
		t.Errorf("draft = %s (%v), want it stored", d, err)
	}
}
