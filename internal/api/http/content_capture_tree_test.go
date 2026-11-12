package http

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// An agent that never captures content takes its spawn tree with it: a
// sub-run's input is what its never parent wrote, so a child under it
// captures nothing, whatever its own agent and its tenant say.
func TestContentCapture_ASubRunOfANeverRunCapturesNothing(t *testing.T) {
	s, _, _, _, _, _ := contentCaptureFixture(t, true)
	never := s.cfg().Agents["allowed"]
	never.Telemetry = &config.AgentTelemetry{ContentCapture: config.AgentContentCaptureNever}
	capturing := s.cfg().Agents["allowed"]
	base := lcotel.WithTenant(context.Background(), "acme")

	if lcotel.ContentCaptureFrom(s.withContentCapture(base, "acme", capturing, false)) == nil {
		t.Fatal("a capturing agent's run captures nothing: this test cannot see a leak")
	}
	parent := s.withContentCapture(base, "acme", never, false)
	if lcotel.ContentCaptureFrom(parent) != nil {
		t.Fatal("a never agent's run captures")
	}
	child := s.withContentCapture(parent, "acme", capturing, false)
	if lcotel.ContentCaptureFrom(child) != nil {
		t.Error("a sub-run of a never run captures its input")
	}
	grandchild := s.withContentCapture(child, "acme", capturing, false)
	if lcotel.ContentCaptureFrom(grandchild) != nil {
		t.Error("a sub-run two levels under a never run captures")
	}
}

// A never child's answer comes back as its parent's tool result: the
// spawning tool call is marked, so neither its span nor the parent's next
// model call captures the answer. Nothing stored changes.
func TestPrepareSubRun_ANeverChildMarksItsAnswerNotCaptured(t *testing.T) {
	for _, never := range []bool{false, true} {
		cfg := noTraceConfig()
		if never {
			c := cfg.Agents["child"]
			c.Telemetry = &config.AgentTelemetry{ContentCapture: config.AgentContentCaptureNever}
			cfg.Agents["child"] = c
		}
		srv, _ := makeServer(t, &childRouter{}, cfg)
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_parent", UserID: "alice"})
		ctx, mark := tools.WithContentMark(ctx)
		prep, err := srv.prepareSubRun(ctx, "child", "", "hi", "", false, func(providers.Event) {})
		if err != nil {
			t.Fatalf("never=%v: %v", never, err)
		}
		prep.cleanup()
		if mark.NotCaptured() != never {
			t.Errorf("never=%v: the spawning call is marked not captured = %v", never, mark.NotCaptured())
		}
		if got := neverCapturesContent(prep.LoopCtx); got != never {
			t.Errorf("never=%v: the prepared child's run says never = %v (a resident child marks its turns by it)", never, got)
		}
		if mark.NotRetained() {
			t.Errorf("never=%v: the child's answer is marked not retained: its parent would store nothing of it", never)
		}
	}
}

// What a resident child hands back at each turn — open, send, poll, cancel,
// its kept ending — goes through residentHandBack: a never child's turn marks
// the calling tool call, and a child read from the store (whose agent is not
// known here) is held to the stricter rule.
func TestResidentHandBack_ANeverChildsTurnIsMarkedNotCaptured(t *testing.T) {
	srv, _ := makeServer(t, &childRouter{}, noTraceConfig())
	call := func(hand func(ctx context.Context)) *tools.ContentMark {
		ctx, mark := tools.WithContentMark(context.Background())
		hand(ctx)
		return mark
	}
	if m := call(func(ctx context.Context) {
		_, _ = srv.residentHandBack(ctx, "r_1", "child", residentEnding{}, "turn", "awaiting_input", nil)
	}); m.NotCaptured() {
		t.Error("an ordinary resident child's turn is marked not captured")
	}
	if m := call(func(ctx context.Context) {
		_, _ = srv.residentHandBack(ctx, "r_1", "child", residentEnding{notCaptured: true}, "turn", "awaiting_input", nil)
	}); !m.NotCaptured() {
		t.Error("a never resident child's turn is not marked")
	}
	rc := &residentChild{agentName: "child", notCaptured: true, state: "completed"}
	if m := call(func(ctx context.Context) {
		_, _, _ = srv.handBackEnding(ctx, "r_1", rc.tombstone(time.Now()))
	}); !m.NotCaptured() {
		t.Error("a never resident child's kept ending is not marked")
	}
	stored := storedResident{run: store.Run{ID: "r_2", Agent: "child", Status: store.RunRunning}, state: "awaiting_input", output: "turn"}
	if m := call(func(ctx context.Context) { _, _, _ = srv.handBackStored(ctx, stored) }); !m.NotCaptured() {
		t.Error("a resident child read from the store is not held to the stricter rule")
	}
}

// A never child's answer is stored as it is, with the mark beside it: a run
// rebuilt from its transcript (a resume, a continuation) records a marker for
// it as the live run did.
func TestReplayTranscript_KeepsTheNotCapturedMarkOfAStoredResult(t *testing.T) {
	srv, _ := makeServer(t, &childRouter{}, noTraceConfig())
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "parent", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_p", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	emit := srv.makeRecordingEmit(ctx, run.ID, tools.RunIdentityValue{AgentID: "a_p", UserID: "alice"}, sess.ID, runStateMeta{}, func(providers.Event) {})
	for i, notCaptured := range []bool{true, false} {
		id := []string{"tu_never", "tu_plain"}[i]
		emit(providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: "Agent", Input: json.RawMessage(`{}`)}})
		emit(providers.Event{Type: providers.EventToolResult, ToolUse: &providers.ToolUse{ID: id, Name: "Agent"},
			Text: "child answer " + id, ContentNotCaptured: notCaptured})
	}
	events, err := srv.store.GetTranscript(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]providers.ContentBlock{}
	for _, m := range replayTranscript(events) {
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				got[b.ToolUseID] = b
			}
		}
	}
	if b, ok := got["tu_never"]; !ok || !b.NotCaptured || b.Text != "child answer tu_never" {
		t.Errorf("replayed never result = %+v (found %v), want its text kept and marked not captured", b, ok)
	}
	if b, ok := got["tu_plain"]; !ok || b.NotCaptured {
		t.Errorf("replayed ordinary result = %+v (found %v), want it unmarked", b, ok)
	}
}

// A resume starts from a fresh ctx: a paused child of a never parent reads
// the never back from its ancestors' rows, and captures nothing; a child of a
// capturing parent captures as it did live.
func TestResumePausedRuns_AResumedChildOfANeverParentCapturesNothing(t *testing.T) {
	for _, never := range []bool{true, false} {
		s, _, st, router, got, _ := contentCaptureFixture(t, true)
		parentDef := s.cfg().Agents["launcher"]
		if never {
			parentDef.Telemetry = &config.AgentTelemetry{ContentCapture: config.AgentContentCaptureNever}
		}
		s.cfg().Agents["launcher"] = parentDef
		ctx := context.Background()
		file := func(agent string, ident store.RunIdentity) store.Run {
			sess, err := st.CreateSession(ctx, "acme", agent, "alice")
			if err != nil {
				t.Fatal(err)
			}
			ident.TenantID, ident.UserID, ident.Model = "acme", "alice", "stub-model"
			run, err := st.CreateRun(ctx, sess.ID, ident)
			if err != nil {
				t.Fatal(err)
			}
			return run
		}
		parent := file("launcher", store.RunIdentity{AgentID: "a_quiet_parent"})
		child := file("allowed", store.RunIdentity{AgentID: "a_child", ParentAgentID: "a_quiet_parent", ParentRunID: parent.ID})
		pauseMidTurn(t, s, child)
		resumeOne(t, s)
		waitRunStatus(t, st, child.ID, store.RunCompleted)
		spans := waitForSpan(t, router, got, lcotel.SpanRun, lcotel.AttrRunID, child.ID)
		captured := false
		for _, sp := range spans {
			for _, k := range lcotel.ContentAttributeKeys {
				if _, ok := sp.attrs[string(k)]; ok {
					captured = true
				}
			}
		}
		if never && captured {
			t.Error("a resumed child of a never parent captured content")
		}
		if !never && !captured {
			t.Fatal("a resumed child of a capturing parent captured nothing: this test cannot see a leak")
		}
	}
}
