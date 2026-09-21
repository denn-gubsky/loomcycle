package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

type inertResp struct {
	Fields map[string]effectiveValue    `json:"fields"`
	Inert  []config.InertContextSetting `json:"inert"`
}

// parkedInertRun is parkedRoutedRun with two changes: the agent carries the one
// advisory this endpoint reports (compaction.memory_flush under a mode whose
// banking is gated on context.harvest_to_memory, which the definition does not
// set), and the run's own config record is whatever the caller passes.
func parkedInertRun(t *testing.T, runCfg string) (*httptest.Server, store.Run) {
	t.Helper()
	yes := true
	recap := config.ContextModeRecap
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"ctxagent": {
			Tier: "middle", Tools: []string{}, SystemPrompt: "you chat",
			Context:    &config.Context{Mode: &recap},
			Compaction: &config.Compaction{Enabled: &yes, MemoryFlush: &yes},
		},
	}
	cfg.Tiers = map[string][]config.TierCandidate{
		"middle": {{Provider: "primary", Model: "model-a"}},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "inert.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: &recordingScriptedProvider{defaultS: endTurn()}},
		[]tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	res := resolve.NewResolver([]string{"primary"}, map[string][]resolve.Candidate{
		"middle": {{Provider: "primary", Model: "model-a"}},
	})
	res.SetReachable("primary", true, []string{"model-a"}, "")
	srv.SetResolver(res)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "", "ctxagent", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_inert", UserID: "alice", Model: "model-a", Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runCfg != "" {
		if err := st.SetRunConfig(ctx, run.ID, json.RawMessage(runCfg)); err != nil {
			t.Fatal(err)
		}
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
	})
	appendResumeEvent(t, srv, run.ID, "text", providers.Event{Type: providers.EventText, Text: "hello"})
	appendResumeEvent(t, srv, run.ID, "done", providers.Event{
		Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{},
	})
	if err := st.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if n, warns := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("resumed %d, want 1 (warnings: %v)", n, warns)
	}
	waitFor(t, "the restored chat to park", func() bool {
		return strings.Contains(runTranscriptText(t, srv.store, run.SessionID, run.ID), "awaiting_input")
	})
	return ts, run
}

func effectiveInert(t *testing.T, ts *httptest.Server, runID string) inertResp {
	t.Helper()
	code, body := getEffective(t, ts, runID)
	if code != 200 {
		t.Fatalf("effective-config: %d %s", code, strings.TrimSpace(body))
	}
	var got inertResp
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return got
}

// ⚠️ ONE PAYLOAD MUST NOT CARRY TWO VERDICTS ABOUT ONE SETTING.
//
// `inert` is computed from the definition the report builds, and its whole
// justification over the boot-time warning is that it can see a PER-RUN
// override introducing — or clearing — a trap on a definition that is not that
// way by itself. The handler passed Routing, Resources and Tuning into that
// merge and NOT Context, so the array answered from the stored definition while
// `fields.context` in the same response answered from the run.
//
// Asserted through the ENDPOINT rather than on effectiveDef, because the
// omission was at the call site: the merge helper was always capable of this
// and was simply never handed the field.
func TestEffectiveConfig_InertIsComputedAgainstTheRunsOwnContext(t *testing.T) {
	// Non-vacuity: the definition alone IS the trap, so the report says so.
	ts, run := parkedInertRun(t, "")
	if got := effectiveInert(t, ts, run.ID); len(got.Inert) != 1 ||
		got.Inert[0].Setting != "compaction.memory_flush" {
		t.Fatalf("the definition's own trap is not reported, so this fixture proves "+
			"nothing about the override: inert=%v", got.Inert)
	}

	// The run clears it. harvest_to_memory is exactly the fix the advisory
	// names, so a run that sets it has no trap left to report.
	ts2, run2 := parkedInertRun(t, `{"context":{"harvest_to_memory":true}}`)
	got := effectiveInert(t, ts2, run2.ID)
	if len(got.Inert) != 0 {
		t.Errorf("the run set context.harvest_to_memory and `inert` still reports it: %v\n\n"+
			"The array is being computed against the stored definition while "+
			"fields.context in the same response is computed against the run.", got.Inert)
	}
}
