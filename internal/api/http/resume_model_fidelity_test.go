package http

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

// tierServerForResume builds a server whose agent routes by TIER (not a pin),
// with two candidates. The pin path deliberately has no cascade, so only a
// tiered agent exercises the Gap 5 restore.
func tierServerForResume(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		Agents: map[string]config.AgentDef{
			"router": {Tier: "middle", SystemPrompt: "p", Tools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	srv, _ := makeServer(t, completingProvider(), cfg)
	res := resolve.NewResolver([]string{"primary", "secondary"}, map[string][]resolve.Candidate{
		"middle": {
			{Provider: "primary", Model: "model-a"},
			{Provider: "secondary", Model: "model-b"},
		},
	})
	res.SetReachable("primary", true, []string{"model-a"}, "")
	res.SetReachable("secondary", true, []string{"model-b"}, "")
	srv.SetResolver(res)
	return srv
}

// RFC DD Gap 5. The runs row records the model a run STARTED on; resume used to
// ignore it and re-derive from the definition, so a run came back on whatever
// the tier resolves to now — silently, with nothing in the transcript saying it
// moved.
func TestResume_RestoresTheModelTheRunStartedOn(t *testing.T) {
	srv := tierServerForResume(t)
	def := srv.cfg().Agents["router"]
	ctx := context.Background()

	// Sanity: the definition's own answer is model-a. Without this the test
	// could pass while restoring nothing, because "restored" and "re-derived"
	// would be the same string.
	_, derived, _, err := srv.resolveAgentDef(ctx, def, "", "alice", "router", "", false)
	if err != nil {
		t.Fatalf("resolveAgentDef: %v", err)
	}
	if derived != "model-a" {
		t.Fatalf("fixture drifted: definition resolves %q, expected model-a", derived)
	}

	// The run started on the SECOND candidate.
	pid, ok := srv.providerForModel(ctx, def, "", "alice", "router", "", false, "model-b")
	if !ok {
		t.Fatal("model-b is a live candidate for this tier but was not accepted — " +
			"a resumed run would silently move to model-a")
	}
	if pid != "secondary" {
		t.Errorf("provider = %q, want secondary", pid)
	}
}

// Re-validation, not blind trust: a model the definition no longer offers must
// be refused, so a snapshot cannot reintroduce a model the operator removed.
func TestResume_RefusesAModelTheDefinitionNoLongerOffers(t *testing.T) {
	srv := tierServerForResume(t)
	def := srv.cfg().Agents["router"]

	if _, ok := srv.providerForModel(context.Background(), def, "", "alice", "router", "", false, "model-withdrawn"); ok {
		t.Error("accepted a model outside the definition's cascade — a promoted def that " +
			"dropped a model could have it carried back in through a snapshot")
	}
}

// A PINNED agent has no cascade: the definition names the model outright, so
// the re-derived value already IS the definition's answer and there is nothing
// to restore. Returning false here is what keeps the pinned path byte-identical.
func TestResume_PinnedAgentHasNothingToRestore(t *testing.T) {
	srv := tierServerForResume(t)
	pinned := config.AgentDef{Provider: "primary", Model: "model-a"}

	if _, ok := srv.providerForModel(context.Background(), pinned, "", "alice", "pinned", "", false, "model-a"); ok {
		t.Error("pinned agent reported a cascade match; the pin path must stay untouched")
	}
}

// Guards the two degenerate inputs rather than leaving them to a nil deref.
func TestResume_ProviderForModelHandlesEmptyInputs(t *testing.T) {
	srv := tierServerForResume(t)
	def := srv.cfg().Agents["router"]
	ctx := context.Background()

	if _, ok := srv.providerForModel(ctx, def, "", "alice", "router", "", false, ""); ok {
		t.Error("an empty model was accepted")
	}
	noResolver := &Server{}
	if _, ok := noResolver.providerForModel(ctx, def, "", "alice", "router", "", false, "model-b"); ok {
		t.Error("a server with no resolver reported a match")
	}
}

// --- the call site, not just the helper ---

// modelRecordingProvider captures the Model the loop actually asked for, which
// is the only way to observe which model a RESUMED run is running on.
type modelRecordingProvider struct {
	lastModel atomic.Value // string
}

func (p *modelRecordingProvider) ID() string                    { return "rec" }
func (p *modelRecordingProvider) Probe(_ context.Context) error { return nil }
func (p *modelRecordingProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"model-a", "model-b"}, nil
}
func (p *modelRecordingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{}
}
func (p *modelRecordingProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.lastModel.Store(req.Model)
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "resumed"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

// TestResumeRun_ActuallyUsesTheRestoredModel covers WHERE the restore happens.
//
// A probe confirmed the gap: disabling the restore in resumeRun with `if false`
// left every helper test green, because those exercise providerForModel and
// not its call site. That is the sixth time in this codebase that a threaded
// value's CROSSING was uncovered while both ends were tested — so this asserts
// the model the provider was actually called with, not the helper's return.
func TestResumeRun_ActuallyUsesTheRestoredModel(t *testing.T) {
	rec := &modelRecordingProvider{}
	cfg := &config.Config{
		Agents: map[string]config.AgentDef{
			"router": {Tier: "middle", SystemPrompt: "p", Tools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	srv, _ := makeServer(t, rec, cfg)
	res := resolve.NewResolver([]string{"primary", "secondary"}, map[string][]resolve.Candidate{
		"middle": {
			{Provider: "primary", Model: "model-a"},
			{Provider: "secondary", Model: "model-b"},
		},
	})
	res.SetReachable("primary", true, []string{"model-a"}, "")
	res.SetReachable("secondary", true, []string{"model-b"}, "")
	srv.SetResolver(res)

	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "router", "alice")
	if err != nil {
		t.Fatal(err)
	}
	// Started on the SECOND candidate; the definition's own answer is model-a.
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_resume", UserID: "alice", Model: "model-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "carry on"}}},
	})
	if err := srv.store.SetRunPauseState(ctx, run.ID, "paused"); err != nil {
		t.Fatal(err)
	}

	n, warns := srv.ResumePausedRuns(ctx)
	if n == 0 {
		t.Fatalf("nothing resumed (warnings: %v)", warns)
	}
	deadline := time.Now().Add(5 * time.Second)
	for rec.lastModel.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rec.lastModel.Load() == nil {
		t.Fatal("provider was never called; the resume did not reach a model call")
	}

	got, _ := rec.lastModel.Load().(string)
	if got != "model-b" {
		t.Errorf("resumed run called the provider with model %q, want model-b — "+
			"the run's started model was not restored and it silently moved", got)
	}
}
