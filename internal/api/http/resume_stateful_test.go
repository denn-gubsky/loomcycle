package http

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func stateEvent(t *testing.T, sigma map[string]any) store.Event {
	t.Helper()
	b, err := json.Marshal(providers.Event{
		Type:         providers.EventContextState,
		ContextState: &providers.ContextStateEventInfo{State: sigma},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store.Event{Type: "context_state", Payload: b}
}

// ⚠️ A STATEFUL RUN'S HISTORY IS NOT ITS MESSAGES. replayTranscript rebuilds a
// conversation, which is exactly what stateful mode exists to not have — so a
// resumed run handed only PriorMessages started from an EMPTY Σ and cheerfully
// continued a conversation whose every established fact it had forgotten. That
// is worse than refusing to resume: it looks like it worked.
func TestStatefulSigmaFromTranscript_TakesTheLastMarker(t *testing.T) {
	events := []store.Event{
		{Type: "text", Payload: []byte(`{"type":"text","text":"hi"}`)},
		stateEvent(t, map[string]any{"goal": "first"}),
		{Type: "tool_result", Payload: []byte(`{"type":"tool_result"}`)},
		stateEvent(t, map[string]any{"goal": "first", "progress": "second"}),
	}
	got := statefulSigmaFromTranscript(events)
	if got == nil {
		t.Fatal("no Σ recovered from a transcript that carries two markers")
	}
	// The LAST marker, not the first and not a merge of them: each one carries
	// the whole post-merge Σ, so replaying the sequence would be both redundant
	// and a second implementation of statepatch.Merge to keep in step.
	if got["progress"] != "second" || got["goal"] != "first" {
		t.Errorf("Σ = %v, want the last marker's whole state", got)
	}
}

// A non-stateful run must come back nil, so the resume path can pass the result
// unconditionally rather than branching on a mode it could get wrong.
func TestStatefulSigmaFromTranscript_NilWithoutMarkers(t *testing.T) {
	events := []store.Event{
		{Type: "text", Payload: []byte(`{"type":"text","text":"hi"}`)},
		{Type: "done", Payload: []byte(`{"type":"done"}`)},
	}
	if got := statefulSigmaFromTranscript(events); got != nil {
		t.Errorf("Σ = %v for a run with no context_state markers, want nil", got)
	}
}

// A row that will not parse must cost only itself. Losing the whole Σ because
// one marker was truncated would turn a cosmetic storage fault into total
// amnesia for the run.
func TestStatefulSigmaFromTranscript_SkipsAnUnparseableMarker(t *testing.T) {
	events := []store.Event{
		stateEvent(t, map[string]any{"goal": "kept"}),
		{Type: "context_state", Payload: []byte(`{truncated`)},
	}
	got := statefulSigmaFromTranscript(events)
	if got == nil || got["goal"] != "kept" {
		t.Errorf("Σ = %v; a bad row discarded the good one before it", got)
	}
}

// sigmaFedProvider answers one stateful step and records the Σ it was fed.
type sigmaFedProvider struct {
	mu  sync.Mutex
	fed []string
}

func (p *sigmaFedProvider) ID() string                                   { return "scripted" }
func (p *sigmaFedProvider) Probe(context.Context) error                  { return nil }
func (p *sigmaFedProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *sigmaFedProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *sigmaFedProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	for _, m := range req.Messages {
		for _, c := range m.Content {
			p.fed = append(p.fed, c.Text)
		}
	}
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
		ID: "t", Name: "emit_state", Input: json.RawMessage(`{"patch":{},"done":true,"final":"resumed"}`)}}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}
func (p *sigmaFedProvider) sawFed(sub string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.fed {
		if strings.Contains(f, sub) {
			return true
		}
	}
	return false
}

// ⚠️ THE CROSSING, and the one thing the unit tests either side of it cannot
// see. statefulSigmaFromTranscript is tested, seeding Σ from InitialState is
// tested — and the JOIN between them lived only in a struct-literal field,
// where deleting it is caught by the compiler today and by nothing at all the
// moment anyone else reads that variable.
//
// A resumed stateful run that started from an empty Σ would continue the
// conversation having forgotten everything it established, which is worse than
// failing to resume: it looks like it worked.
func TestResumePausedRuns_StatefulRunRecoversItsState(t *testing.T) {
	mode := config.ContextModeStateful
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"statefulresumer": {
				Provider: "scripted", Model: "stub-model", SystemPrompt: "you resume work",
				Tools: []string{}, Context: &config.Context{Mode: &mode},
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &sigmaFedProvider{}
	srv, _ := makeServer(t, prov, cfg)
	ctx := context.Background()

	sess, err := srv.store.CreateSession(ctx, "", "statefulresumer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_sfresume", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "explain DDRAM"}}},
	})
	// The transcript a stateful run leaves: a Σ marker per step.
	appendResumeEvent(t, srv, run.ID, "context_state", providers.Event{
		Type:         providers.EventContextState,
		ContextState: &providers.ContextStateEventInfo{State: map[string]any{"goal": "explain DDRAM"}},
	})
	appendResumeEvent(t, srv, run.ID, "context_state", providers.Event{
		Type: providers.EventContextState,
		ContextState: &providers.ContextStateEventInfo{
			State: map[string]any{"goal": "explain DDRAM", "decisions": "capacitors, refreshed"}},
	})

	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if n, warnings := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("ResumePausedRuns re-dispatched %d, want 1 (warnings: %v)", n, warnings)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, gerr := srv.store.GetRun(ctx, run.ID)
		if gerr != nil {
			t.Fatalf("GetRun: %v", gerr)
		}
		if got.Status == store.RunCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resumed stateful run never completed (status %q)", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Asserted on the PROMPT, because that is the only thing the model sees. A
	// Σ recovered into a struct and absent from the request is the failure this
	// phase exists to prevent.
	for _, want := range []string{`"goal":"explain DDRAM"`, `"decisions":"capacitors, refreshed"`} {
		if !prov.sawFed(want) {
			t.Errorf("the resumed run was never fed %s — it restarted from an empty Σ:\n%v",
				want, prov.fed)
		}
	}
}
