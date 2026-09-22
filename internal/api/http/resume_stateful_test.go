package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func evOf(t *testing.T, typ string, payload any) store.Event {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return store.Event{Type: typ, Payload: b}
}

func userInputEv(t *testing.T, text string) store.Event {
	return evOf(t, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: text}}}})
}

func markerEv(t *testing.T, sigma map[string]any, action string) store.Event {
	return evOf(t, "context_state", providers.Event{Type: providers.EventContextState,
		ContextState: &providers.ContextStateEventInfo{State: sigma, Action: action}})
}

func callEv(t *testing.T, id, name string) store.Event {
	return evOf(t, "tool_call", providers.Event{Type: providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: id, Name: name, Input: json.RawMessage(`{}`)}})
}

func resultEv(t *testing.T, id, name, text string, isErr bool) store.Event {
	return evOf(t, "tool_result", providers.Event{Type: providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: id, Name: name, Input: json.RawMessage(`{}`)}, Text: text, IsError: isErr})
}

func textEv(t *testing.T, text string) store.Event {
	return evOf(t, "text", providers.Event{Type: providers.EventText, Text: text})
}

// multiTurnStatefulTranscript is what two turns of a stateful chat actually
// leave: an action per turn, an answer, the operator's second message, and a
// pause after the second action returned. The fixture that let the first fix
// through had one user message and two markers — too small to hold either of
// the failures this guards against.
func multiTurnStatefulTranscript(t *testing.T) []store.Event {
	return []store.Event{
		userInputEv(t, "explain DDRAM"),
		markerEv(t, map[string]any{"goal": "explain DDRAM"}, "Echo"),
		callEv(t, "es-act-0", "Echo"),
		resultEv(t, "es-act-0", "Echo", "a capacitor per bit", false),
		markerEv(t, map[string]any{"goal": "explain DDRAM", "found": "capacitors"}, ""),
		textEv(t, "ANSWER ONE: DDRAM stores bits in capacitors"),
		userInputEv(t, "and what about refresh?"),
		markerEv(t, map[string]any{"goal": "explain DDRAM", "found": "capacitors", "asked": "refresh"}, "Echo"),
		callEv(t, "es-act-2", "Echo"),
		resultEv(t, "es-act-2", "Echo", "refreshed every 64ms", false),
	}
}

// ⚠️ A STATEFUL RUN'S HISTORY IS NOT ITS MESSAGES, and neither is what it was
// looking at. Paused after an action returned, the run resumes looking at THAT
// result — not at the whole conversation relabelled "Task:".
func TestStatefulSeed_PausedAfterAnActionResumesOnItsResult(t *testing.T) {
	seed := statefulSeedFromEvents(multiTurnStatefulTranscript(t), nil, true)
	if !seed.HasState || seed.Sigma["asked"] != "refresh" {
		t.Fatalf("Σ = %v, want the last marker's state", seed.Sigma)
	}
	if seed.Observation != "refreshed every 64ms" {
		t.Errorf("observation = %q, want exactly the pending action's result", seed.Observation)
	}
	if seed.StartParked {
		t.Error("a run with an action result to act on must not park")
	}
}

// Paused after the operator's message was delivered: that message is the
// observation, framed as the live park frames it.
func TestStatefulSeed_PausedAfterAnOperatorTurnResumesOnTheMessage(t *testing.T) {
	events := multiTurnStatefulTranscript(t)[:7] // through "and what about refresh?"
	seed := statefulSeedFromEvents(events, nil, true)
	if seed.Observation != "operator: and what about refresh?" {
		t.Errorf("observation = %q", seed.Observation)
	}
	if seed.Sigma["found"] != "capacitors" {
		t.Errorf("Σ = %v, want the turn-1 state", seed.Sigma)
	}
}

// Paused while waiting for the operator: nothing is pending, so an interactive
// run parks — and nothing about the conversation is fed back.
func TestStatefulSeed_AnIdleInteractiveRunParks(t *testing.T) {
	events := multiTurnStatefulTranscript(t)[:6] // through ANSWER ONE
	seed := statefulSeedFromEvents(events, nil, true)
	if seed.Observation != "" || !seed.StartParked {
		t.Errorf("observation=%q parked=%v, want nothing pending and a park", seed.Observation, seed.StartParked)
	}
	if auto := statefulSeedFromEvents(events, nil, false); auto.StartParked {
		t.Error("an autonomous run has no operator to park for")
	}
}

// ⚠️ NEVER RE-RUN AN ACTION ON THE MODEL'S BEHALF. A process that stopped
// during a call leaves a tool_call with no result, and whether it took effect
// is unknown: the model is told so, and decides.
func TestStatefulSeed_AnInterruptedActionIsReportedNotRepeated(t *testing.T) {
	events := multiTurnStatefulTranscript(t)[:9] // the second call, no result
	seed := statefulSeedFromEvents(events, nil, true)
	if !strings.HasPrefix(seed.Observation, "ERROR: the action `Echo` was interrupted") {
		t.Errorf("observation = %q", seed.Observation)
	}
}

// A step may name an action AND set done; the loop ends the turn. The seed
// reads what happened, not what the marker's action field says.
func TestStatefulSeed_DoneWithAnActionNamedIsATurnEnd(t *testing.T) {
	events := []store.Event{
		userInputEv(t, "hi"),
		markerEv(t, map[string]any{"n": 1}, "Echo"),
		textEv(t, "all done"),
		userInputEv(t, "thanks, one more"),
	}
	seed := statefulSeedFromEvents(events, nil, true)
	if seed.Observation != "operator: thanks, one more" {
		t.Errorf("observation = %q", seed.Observation)
	}
}

// A continuation's new message follows whatever was still pending, and is
// framed by who is speaking.
func TestStatefulSeed_AContinuationAddsItsMessage(t *testing.T) {
	events := multiTurnStatefulTranscript(t)[:6]
	fresh := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "next question"}}}}
	if seed := statefulSeedFromEvents(events, fresh, false); seed.Observation != "Task: next question" {
		t.Errorf("autonomous continuation observation = %q", seed.Observation)
	}
	if seed := statefulSeedFromEvents(events, fresh, true); seed.Observation != "operator: next question" {
		t.Errorf("interactive continuation observation = %q", seed.Observation)
	}
}

// A marker that will not parse costs only itself, and one whose state was
// dropped (masking made it unparseable) keeps the Σ before it.
func TestStatefulSeed_ABadMarkerCostsOnlyItself(t *testing.T) {
	events := []store.Event{
		markerEv(t, map[string]any{"goal": "kept"}, ""),
		{Type: "context_state", Payload: []byte(`{truncated`)},
		markerEv(t, nil, ""),
	}
	if seed := statefulSeedFromEvents(events, nil, true); seed.Sigma["goal"] != "kept" {
		t.Errorf("Σ = %v; a bad or empty marker discarded the good one before it", seed.Sigma)
	}
}

// No marker at all: nothing to restore, and the caller keeps its path.
func TestStatefulSeed_NoMarkerMeansNoState(t *testing.T) {
	seed := statefulSeedFromEvents([]store.Event{userInputEv(t, "hi")}, nil, false)
	if seed.HasState || seed.Sigma != nil {
		t.Errorf("seed = %+v for a transcript with no context_state", seed)
	}
	if seed.Observation != "Task: hi" {
		t.Errorf("observation = %q, want the task", seed.Observation)
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
	// Two turns of a real stateful chat, paused after the second action
	// returned. See multiTurnStatefulTranscript for why the size matters.
	for _, ev := range multiTurnStatefulTranscript(t) {
		if err := srv.store.AppendEvent(ctx, run.ID, ev.Type, ev.Payload); err != nil {
			t.Fatal(err)
		}
	}

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
	for _, want := range []string{`"found":"capacitors"`, `"asked":"refresh"`} {
		if !prov.sawFed(want) {
			t.Errorf("the resumed run was never fed %s — it restarted from an empty Σ:\n%v",
				want, prov.fed)
		}
	}
	// ⚠️ AND WHAT IT WAS LOOKING AT. The first observation is the result of
	// the action it chose before the pause — not the conversation so far,
	// replayed and relabelled as a task.
	prov.mu.Lock()
	first := ""
	if len(prov.fed) > 0 {
		first = prov.fed[0]
	}
	prov.mu.Unlock()
	if !strings.HasSuffix(first, "Latest observation:\nrefreshed every 64ms") {
		t.Errorf("the resumed run's first observation is not the pending action's result:\n%s", first)
	}
	for _, leaked := range []string{"Task:", "ANSWER ONE", "and what about refresh?"} {
		if strings.Contains(first, leaked) {
			t.Errorf("the resumed run was fed the replayed conversation (%q):\n%s", leaked, first)
		}
	}
}

// ⚠️ A CONTINUATION NEVER RESTORED Σ. Only resume did, so every continuation
// of a stateful session started from an EMPTY state with the whole replayed
// transcript as its first observation — and in the embedded terminal that was
// every message after the first.
func TestMessages_AStatefulContinuationStartsFromTheSessionsState(t *testing.T) {
	mode := config.ContextModeStateful
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"statefulchat": {Model: "stub-model", Tools: []string{}, SystemPrompt: "you chat",
			Context: &config.Context{Mode: &mode}},
	}
	prov := &sigmaFedProvider{}
	srv, _ := makeServer(t, prov, cfg)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	ctx := context.Background()

	sess, err := srv.store.CreateSession(ctx, "", "statefulchat", "alice")
	if err != nil {
		t.Fatal(err)
	}
	prior, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_prior", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	// A finished first turn: the model answered and the run ended.
	for _, ev := range multiTurnStatefulTranscript(t)[:6] {
		if err := srv.store.AppendEvent(ctx, prior.ID, ev.Type, ev.Payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.store.FinishRun(ctx, prior.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.URL+"/v1/sessions/"+sess.ID+"/messages", "application/json", strings.NewReader(
		`{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"and what about refresh?"}]}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}

	prov.mu.Lock()
	first := ""
	if len(prov.fed) > 0 {
		first = prov.fed[0]
	}
	prov.mu.Unlock()
	if !strings.Contains(first, `"found":"capacitors"`) {
		t.Errorf("the continuation started from an empty Σ:\n%s", first)
	}
	if !strings.HasSuffix(first, "Latest observation:\nTask: and what about refresh?") {
		t.Errorf("the continuation's first observation is not its own message:\n%s", first)
	}
	if strings.Contains(first, "ANSWER ONE") || strings.Contains(first, "explain DDRAM\n") {
		t.Errorf("the continuation was fed the replayed conversation:\n%s", first)
	}
}

// The same continuation through RunOnce — the universal path gRPC, MCP,
// webhooks and the scheduler use, which no HTTP test reaches.
func TestRunOnce_AStatefulContinuationStartsFromTheSessionsState(t *testing.T) {
	mode := config.ContextModeStateful
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"statefulchat": {Model: "stub-model", Tools: []string{}, SystemPrompt: "you chat",
			Context: &config.Context{Mode: &mode}},
	}
	prov := &sigmaFedProvider{}
	srv, _ := makeServer(t, prov, cfg)
	ctx := context.Background()

	sess, err := srv.store.CreateSession(ctx, "", "statefulchat", "alice")
	if err != nil {
		t.Fatal(err)
	}
	prior, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_prior", UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range multiTurnStatefulTranscript(t)[:6] {
		if err := srv.store.AppendEvent(ctx, prior.ID, ev.Type, ev.Payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.store.FinishRun(ctx, prior.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}

	if err := srv.RunOnce(ctx, runner.RunInput{
		Agent: "statefulchat", SessionID: sess.ID, UserID: "alice",
		Segments: []loop.PromptSegment{{Role: "user",
			Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "and what about refresh?"}}}},
	}, runner.RunCallbacks{OnEvent: func(providers.Event) {}}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	prov.mu.Lock()
	first := ""
	if len(prov.fed) > 0 {
		first = prov.fed[0]
	}
	prov.mu.Unlock()
	if !strings.Contains(first, `"found":"capacitors"`) {
		t.Errorf("the continuation started from an empty Σ:\n%s", first)
	}
	if !strings.HasSuffix(first, "Latest observation:\nTask: and what about refresh?") {
		t.Errorf("the continuation's first observation is not its own message:\n%s", first)
	}
}
