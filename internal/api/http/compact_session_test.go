package http

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedContinuationSession builds the shape the run filter broke: ONE session
// with two runs, where the second is a short continuation — exactly what an
// interactive chat looks like after the first reply.
//
// bulk makes the turns worth compacting; a fixture of two-word turns "compacts"
// into something larger than it started and is now correctly refused.
func seedContinuationSession(t *testing.T, srv *Server, firstTurns, contTurns int) (sessID, firstRunID, contRunID string) {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "compactor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	bulk := strings.Repeat("with enough substance that summarising it is a saving. ", 6)
	uinput := func(text string) []loop.PromptSegment {
		return []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: text}}}}
	}
	mkRun := func(turns int, tag string) string {
		run, rerr := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
			AgentID: "a_c", UserID: "alice", Model: "stub-model",
		})
		if rerr != nil {
			t.Fatal(rerr)
		}
		for i := 1; i <= turns; i++ {
			appendResumeEvent(t, srv, run.ID, "user_input", uinput(fmt.Sprintf("%s q%d %s", tag, i, bulk)))
			appendResumeEvent(t, srv, run.ID, "text",
				providers.Event{Type: providers.EventText, Text: fmt.Sprintf("%s a%d %s", tag, i, bulk)})
			appendResumeEvent(t, srv, run.ID, "done", providers.Event{Type: providers.EventDone, StopReason: "end_turn"})
		}
		if err := srv.store.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
			t.Fatal(err)
		}
		return run.ID
	}
	firstRunID = mkRun(firstTurns, "first")
	contRunID = mkRun(contTurns, "cont")
	return sess.ID, firstRunID, contRunID
}

// B-iv: a manual compact must operate on the SESSION, which is what the loop
// holds — not on one run_id.
//
// The filter fetched the session transcript and then discarded most of it, so a
// continuation chat presented 2-3 messages and answered "nothing to compact" at
// 92% of its window. That is the bug an operator hit and could not explain.
func TestCompactRun_CompactsTheSessionNotTheRun(t *testing.T) {
	srv, _ := compactFixture(t)
	_, _, contRunID := seedContinuationSession(t, srv, 8, 1)

	res, err := srv.CompactRun(context.Background(), contRunID)
	if err != nil {
		t.Fatalf("CompactRun: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("compacting a 1-turn continuation of a 9-turn session did nothing: %+v\n"+
			"the run filter is back — the session is what the loop holds", res)
	}
	// 9 exchanges = 18 messages across the session; the continuation alone is 2.
	if res.Messages != 0 && res.Messages < 4 {
		t.Errorf("saw %d messages — that is the continuation, not the session", res.Messages)
	}
}

// The corruption half of B-iv: keepN is applied by applyCompactSummary against
// the loop's SESSION-scoped history, so computing it from a run-scoped slice
// stapled a tail sliced from a different list onto a summary of a span the
// model never held.
func TestCompactRun_KeepNMatchesTheLoopsHistory(t *testing.T) {
	srv, _ := compactFixture(t)
	sessID, _, contRunID := seedContinuationSession(t, srv, 8, 1)

	res, err := srv.CompactRun(context.Background(), contRunID)
	if err != nil {
		t.Fatalf("CompactRun: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("no compaction: %+v", res)
	}
	// What the loop would rebuild for this session.
	events, err := srv.store.GetTranscript(context.Background(), sessID)
	if err != nil {
		t.Fatal(err)
	}
	sessionMsgs := len(replayTranscript(events))
	// before_tokens must be measured over the SESSION. Measured over the
	// continuation alone it would be a small fraction of this.
	if res.BeforeTokens < sessionMsgs {
		t.Errorf("before_tokens=%d over a %d-message session — the measurement is "+
			"scoped to the run, so keep_n would be computed against the wrong list",
			res.BeforeTokens, sessionMsgs)
	}
}

// "Nothing to compact" was a lie of omission. Each noop now names its numbers
// and the action, because they call for different ones.
func TestCompactRun_NoopNamesTheReasonAndTheNumbers(t *testing.T) {
	srv, _ := compactFixture(t)
	// A session too short to compact at all.
	_, _, shortRun := seedContinuationSession(t, srv, 1, 0)
	res, err := srv.CompactRun(context.Background(), shortRun)
	if err != nil {
		t.Fatalf("CompactRun: %v", err)
	}
	if res.Compacted {
		t.Fatalf("a 1-turn session should not compact: %+v", res)
	}
	// Deliberately still "noop": this case has always returned it, and renaming
	// it would break every consumer matching it to say something the reason
	// field now says anyway. The NEW verdicts are the additive ones.
	if res.Applied != "noop" {
		t.Errorf("applied = %q, want noop — the existing value must not change", res.Applied)
	}
	if res.Reason == "" {
		t.Error("the noop carries no reason; 'nothing to compact' is what sent an " +
			"operator away from a window that was 92% full")
	}
}

// ⚠️ THE THIRD not_smaller SITE — and the one the evidence came from.
//
// The refusal was specified for three places and shipped to two: the loop's
// recap and compaction paths. This is the MANUAL path, which is exactly where
// the observed session's 14230 -> 14334 happened. Both numbers were computed
// here and never compared, so a compaction that made the context bigger was
// pushed to the loop and applied.
func TestCompactRun_RefusesWhenSummaryIsNotSmaller(t *testing.T) {
	srv, prov := compactFixture(t)
	// A summary longer than the span it replaces.
	prov.defaultS = []providers.Event{
		{Type: providers.EventText, Text: strings.Repeat("a summary somehow longer than its source. ", 200)},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
	}
	_, _, contRunID := seedContinuationSession(t, srv, 8, 1)

	res, err := srv.CompactRun(context.Background(), contRunID)
	if err != nil {
		t.Fatalf("CompactRun: %v", err)
	}
	if res.Compacted {
		t.Fatalf("a compaction that is not smaller was APPLIED: %+v", res)
	}
	if res.Applied != "noop_not_smaller" {
		t.Errorf("applied = %q, want noop_not_smaller", res.Applied)
	}
	// Both counts must ride the answer: "14230 -> 14334" is the explanation.
	if res.AfterTokens < res.BeforeTokens {
		t.Errorf("before/after = %d/%d — the refusal should report the growth it refused",
			res.BeforeTokens, res.AfterTokens)
	}
	if !strings.Contains(res.Reason, "not smaller") {
		t.Errorf("reason does not say why: %q", res.Reason)
	}
}
