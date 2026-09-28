package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func webhookDef(event hooks.Phase, url string) hooks.Def {
	return hooks.Def{Event: event, Body: hooks.DefBody{Kind: hooks.BodyKindHTTP, URL: url}}
}

// pinnedAgent is an agent in tenant acme with a HookDef reference found in
// its own tenant (gate), one found only in the shared tenant (shared), and an
// inline webhook whose URL is not for a run's readers.
func pinnedAgent() config.AgentDef {
	return config.AgentDef{OwnerTenant: "acme", Hooks: hooks.EventHooks{
		hooks.PhaseAgentStop: {{Ref: "gate"}},
		hooks.PhaseRunEnd:    {{Ref: "shared"}, {Inline: &hooks.Inline{Name: "audit", URL: "https://hooks.example/secret-path"}}},
	}}
}

// startPinnedRun starts a run of pinnedAgent's hooks and returns it re-read.
func startPinnedRun(t *testing.T, h *reviewHarness) (context.Context, store.Run) {
	t.Helper()
	ctx := context.Background()
	putHookDef(t, h.st, "acme", "gate", webhookDef(hooks.PhaseAgentStop, "https://h.example/gate-v1"))
	putHookDef(t, h.st, "", "shared", webhookDef(hooks.PhaseRunEnd, "https://h.example/shared"))
	sess, err := h.st.CreateSession(ctx, "acme", "writer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_pin", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	rctx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice", TenantID: "acme", AgentID: "a_pin"})
	if err := hooks.SetFrom(h.srv.withRunHooks(rctx, run.ID, "writer", pinnedAgent(), hooks.Additions{})).Err(); err != nil {
		t.Fatal(err)
	}
	run, _ = h.st.GetRun(ctx, run.ID)
	return rctx, run
}

func pinsOf(t *testing.T, run store.Run) *pinnedHooks {
	t.Helper()
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok || rec.PinnedHooks == nil {
		t.Fatalf("the run recorded no pinned hooks: %s", run.RunConfig)
	}
	return rec.PinnedHooks
}

func defIDs(set *hooks.Set, phase hooks.Phase) []string {
	var out []string
	for _, h := range set.Match("writer", "", phase) {
		out = append(out, h.DefID)
	}
	return out
}

// A run records what its hooks resolved to when it starts: every HookDef
// lookup it made — including the one that found nothing in its own tenant —
// and a fingerprint of its agent's hooks, never their content: the record is
// the run's spec, and an inline webhook's URL is not its readers'.
func TestRunHooks_ARunPinsItsHooksWhenItStarts(t *testing.T) {
	h := newReviewHarness(t)
	_, run := startPinnedRun(t, h)
	p := pinsOf(t, run)
	want := map[string]string{"acme/gate@0": "hdf_acme_gate", "acme/shared@0": "", "/shared@0": "hdf__shared"}
	for k, v := range want {
		if got, ok := p.Defs[k]; !ok || got != v {
			t.Errorf("pin %s = %q (recorded %v), want %q", k, got, ok, v)
		}
	}
	if p.Agent != agentHooksFingerprint(pinnedAgent()) {
		t.Errorf("fingerprint = %s", p.Agent)
	}
	if strings.Contains(string(run.RunConfig), "secret-path") {
		t.Fatalf("the run's record carries the inline webhook's URL: %s", run.RunConfig)
	}
}

// A resumed run fires the hooks it started with, not what its definitions say
// now: a newer active version of its HookDef, and a same-named HookDef created
// since in its own tenant, do not replace what it resolved at start.
func TestRunHooks_AResumedRunFiresTheHooksItStartedWith(t *testing.T) {
	h := newReviewHarness(t)
	rctx, run := startPinnedRun(t, h)
	ctx := context.Background()
	v2, err := h.st.HookDefCreate(ctx, store.HookDefRow{DefID: "hdf_acme_gate_v2", Name: "gate", TenantID: "acme", ParentDefID: "hdf_acme_gate",
		Definition: mustJSON(t, webhookDef(hooks.PhaseAgentStop, "https://h.example/gate-v2"))})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.HookDefSetActive(ctx, "acme", "gate", v2.DefID, ""); err != nil {
		t.Fatal(err)
	}
	putHookDef(t, h.st, "acme", "shared", webhookDef(hooks.PhaseRunEnd, "https://h.example/acme-shared"))

	fresh := hooks.SetFrom(h.srv.withRunHooks(rctx, "", "writer", pinnedAgent(), hooks.Additions{}))
	if got := defIDs(fresh, hooks.PhaseAgentStop); len(got) != 1 || got[0] != v2.DefID {
		t.Fatalf("a new run resolves gate to %v; the test needs it to differ from the pinned one", got)
	}
	set := hooks.SetFrom(h.srv.withResumedRunHooks(rctx, run, pinnedAgent(), pinsOf(t, run)))
	if err := set.Err(); err != nil {
		t.Fatal(err)
	}
	if got := defIDs(set, hooks.PhaseAgentStop); len(got) != 1 || got[0] != "hdf_acme_gate" {
		t.Errorf("resumed gate = %v, want the pinned version", got)
	}
	if got := defIDs(set, hooks.PhaseRunEnd); len(got) != 2 || got[0] != "hdf__shared" {
		t.Errorf("resumed run_end = %v, want the shared HookDef the run fell back to, then the inline one", got)
	}
}

// A run does not continue under hooks it did not start with: an agent whose
// hooks changed while it was paused, or a pinned HookDef version since
// deleted, stops the resumed run. A run recorded before hooks were pinned
// resolves them as it always has.
func TestRunHooks_AResumedRunStopsWhenItsHooksAreGone(t *testing.T) {
	t.Run("agent changed", func(t *testing.T) {
		h := newReviewHarness(t)
		rctx, run := startPinnedRun(t, h)
		changed := pinnedAgent()
		changed.Hooks[hooks.PhaseAgentStop] = nil
		err := hooks.SetFrom(h.srv.withResumedRunHooks(rctx, run, changed, pinsOf(t, run))).Err()
		if err == nil || !strings.Contains(err.Error(), "changed while the run was paused") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("version deleted", func(t *testing.T) {
		h := newReviewHarness(t)
		rctx, run := startPinnedRun(t, h)
		if _, err := h.st.HookDefDelete(context.Background(), "acme", "gate"); err != nil {
			t.Fatal(err)
		}
		err := hooks.SetFrom(h.srv.withResumedRunHooks(rctx, run, pinnedAgent(), pinsOf(t, run))).Err()
		if err == nil || !strings.Contains(err.Error(), "no longer exists") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("recorded before pinning", func(t *testing.T) {
		h := newReviewHarness(t)
		rctx, run := startPinnedRun(t, h)
		set := hooks.SetFrom(h.srv.withResumedRunHooks(rctx, run, pinnedAgent(), nil))
		if err := set.Err(); err != nil || len(defIDs(set, hooks.PhaseAgentStop)) != 1 {
			t.Fatalf("a run with no pins resolves as before: %v", err)
		}
	})
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// End to end: ResumePausedRuns restores a run through its pinned hooks. Its
// agent's hooks changed while it was paused, so it fails, saying why, rather
// than resuming under hooks it did not start with.
func TestResumePausedRuns_ARunWhoseHooksChangedDoesNotResume(t *testing.T) {
	started := config.AgentDef{Provider: "scripted", Model: "stub-model", SystemPrompt: "work",
		Hooks: hooks.EventHooks{hooks.PhaseRunEnd: {{Inline: &hooks.Inline{Name: "audit", URL: "https://hooks.example/a"}}}}}
	now := started
	now.Hooks = hooks.EventHooks{hooks.PhaseRunEnd: {{Inline: &hooks.Inline{Name: "audit", URL: "https://hooks.example/b"}}}}
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"worker": now},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &scriptedProvider{defaultS: []providers.Event{
		{Type: providers.EventText, Text: "done"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
	}}
	srv, _ := makeServer(t, prov, cfg)
	ctx := context.Background()
	sess, _ := srv.store.CreateSession(ctx, "", "worker", "alice")
	rec := runConfigRecord{PinnedHooks: &pinnedHooks{Agent: agentHooksFingerprint(started)}}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_w", UserID: "alice", Model: "stub-model", RunConfig: rec.marshal()})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}},
	})
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if n, w := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("re-dispatched %d; %v", n, w)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := srv.store.GetRun(ctx, run.ID)
		if got.Status == store.RunFailed {
			if !strings.Contains(got.ErrorMsg, "changed while the run was paused") {
				t.Fatalf("failed for another reason: %s", got.ErrorMsg)
			}
			return
		}
		if got.Status == store.RunCompleted {
			t.Fatal("the run resumed under hooks it did not start with")
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %q", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
