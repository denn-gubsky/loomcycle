package decisionbuild

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	// Config load checks each providers: entry's driver against the registry.
	_ "github.com/denn-gubsky/loomcycle/internal/providers/ollama"
	_ "github.com/denn-gubsky/loomcycle/internal/providers/openai"
)

// endpoint is a /v1/systemone double that records the model and the key of
// every call and answers question "q".
type endpoint struct {
	mu     sync.Mutex
	models []string
	auth   []string
	url    string
	// delay holds every reply back; inFlight and peak count the calls it held.
	delay          time.Duration
	inFlight, peak int
}

func newEndpoint(t *testing.T) *endpoint {
	t.Helper()
	e := &endpoint{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		e.mu.Lock()
		e.models = append(e.models, body.Model)
		e.auth = append(e.auth, r.Header.Get("Authorization"))
		e.inFlight++
		if e.inFlight > e.peak {
			e.peak = e.inFlight
		}
		delay := e.delay
		e.mu.Unlock()
		time.Sleep(delay)
		e.mu.Lock()
		e.inFlight--
		e.mu.Unlock()
		_, _ = io.WriteString(w, `{"model":"`+body.Model+`","answers":{"q":{"type":"noul","noul":0.5}},"usage":{"input_tokens":11,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	e.url = srv.URL
	return e
}

func (e *endpoint) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.models)
}

var oneQuestion = map[string]decision.Question{"q": {Type: decision.TypeNoul, Instructions: "Is it?"}}

// load loads yaml as the server does (validated), with both Ollama providers
// served by e.
func load(t *testing.T, e *endpoint, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.LoadLayers(config.Layer{Name: "test", Data: []byte(`
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
providers:
  ollama-local: { driver: ollama, base_url: "` + e.url + `" }
  ollama:       { driver: ollama, base_url: "` + e.url + `", api_key_env: OLLAMA_API_KEY }
  openai:       { driver: openai, api_key_env: OPENAI_API_KEY }
models:
  decide:      { provider: ollama-local, model: nimble, kind: decision }
  decide-deep: { provider: ollama-local, model: clef, kind: decision }
  hosted:      { provider: ollama, model: nimble, kind: decision }
  plain:       { provider: ollama-local, model: nimble }
` + yaml)})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

func build(t *testing.T, cfg *config.Config) *decision.Service {
	t.Helper()
	s, err := Build(cfg, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if s == nil {
		t.Fatal("Build returned no service for a declared block")
	}
	return s
}

// TestBuild_NoBlockIsNoService — the capability is off unless declared.
func TestBuild_NoBlockIsNoService(t *testing.T) {
	if s, err := Build(load(t, newEndpoint(t), ""), nil); err != nil || s != nil {
		t.Errorf("Build = %v, %v; want nil, nil", s, err)
	}
}

// TestBuild_AnAliasReachesTheProviderAsItsModel — the name a caller uses is the
// operator's alias; the provider is asked for the model the alias names, never
// for the alias. A plain name is served by decision.provider as written.
func TestBuild_AnAliasReachesTheProviderAsItsModel(t *testing.T) {
	e := newEndpoint(t)
	s := build(t, load(t, e, `
decision:
  default: decide
  models: [decide, decide-deep, nimble-lit]
  provider: ollama-local
`))
	for _, c := range []struct{ name, want string }{
		{"", "nimble"}, {"decide", "nimble"}, {"decide-deep", "clef"}, {"nimble-lit", "nimble-lit"},
	} {
		if _, err := s.Decide(context.Background(), c.name, map[string]any{"x": 1}, oneQuestion); err != nil {
			t.Fatalf("Decide(%q): %v", c.name, err)
		}
		if got := e.models[len(e.models)-1]; got != c.want {
			t.Errorf("Decide(%q) asked the provider for %q, want %q", c.name, got, c.want)
		}
	}
	var got []string
	for _, m := range s.Models() {
		got = append(got, m.Name+"="+m.Provider+"/"+m.Model)
	}
	want := []string{"decide=ollama-local/nimble", "decide-deep=ollama-local/clef", "nimble-lit=ollama-local/nimble-lit"}
	if !reflect.DeepEqual(got, want) || s.Default() != "decide" {
		t.Errorf("models = %v default %q, want %v default decide", got, s.Default(), want)
	}
}

// TestBuild_ModelsOmittedAllowsTheTaggedAliases — with no list a caller may name
// every alias tagged kind: decision; an untagged alias and a model's own name
// are refused without a call.
func TestBuild_ModelsOmittedAllowsTheTaggedAliases(t *testing.T) {
	t.Setenv("OLLAMA_API_KEY", "test-operator-key")
	e := newEndpoint(t)
	s := build(t, load(t, e, "decision:\n  default: decide\n"))
	for _, name := range []string{"decide", "decide-deep", "hosted"} {
		if _, err := s.Decide(context.Background(), name, nil, oneQuestion); err != nil {
			t.Errorf("Decide(%q): %v, want a tagged alias allowed", name, err)
		}
	}
	before := e.calls()
	for _, name := range []string{"plain", "nimble", "clef"} {
		_, err := s.Decide(context.Background(), name, nil, oneQuestion)
		if decision.CodeOf(err) != decision.CodeModelNotAllowed {
			t.Errorf("Decide(%q) err = %v, want model_not_allowed", name, err)
		}
	}
	if e.calls() != before {
		t.Errorf("a refused name reached the provider (%d calls)", e.calls()-before)
	}
}

// TestBuild_UsesTheProvidersOwnEndpointAndKey — the key is the provider's, by
// the rule every driver follows: the operator's key for an ordinary run, none at
// all for a run barred from it, and no key on a keyless provider.
func TestBuild_UsesTheProvidersOwnEndpointAndKey(t *testing.T) {
	t.Setenv("OLLAMA_API_KEY", "test-operator-key")
	e := newEndpoint(t)
	s := build(t, load(t, e, "decision:\n  default: decide\n  models: [decide, hosted]\n"))
	if _, err := s.Decide(context.Background(), "hosted", nil, oneQuestion); err != nil || e.auth[0] != "Bearer test-operator-key" {
		t.Fatalf("keyed provider: err %v auth %q, want the operator's key", err, e.auth)
	}
	if _, err := s.Decide(context.Background(), "decide", nil, oneQuestion); err != nil || e.auth[1] != "" {
		t.Fatalf("keyless provider: err %v auth %q, want no key", err, e.auth)
	}
	restricted := providers.WithOperatorKeyAllowed(context.Background(), false)
	_, err := s.Decide(restricted, "hosted", nil, oneQuestion)
	if !errors.Is(err, providers.ErrOperatorKeyForbidden) || e.calls() != 2 {
		t.Errorf("restricted run: err %v after %d calls, want ErrOperatorKeyForbidden and no third call", err, e.calls())
	}
}

// TestBuild_BooksEachCallsUsage — what main wires to the run ledger receives the
// provider and the model the alias named.
func TestBuild_BooksEachCallsUsage(t *testing.T) {
	e := newEndpoint(t)
	s := build(t, load(t, e, "decision:\n  default: decide-deep\n"))
	var booked []providers.Usage
	s.SetOnUsage(func(_ context.Context, u *providers.Usage) { booked = append(booked, *u) })
	if _, err := s.Decide(context.Background(), "", nil, oneQuestion); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	want := providers.Usage{InputTokens: 11, OutputTokens: 1, Model: "clef", Provider: "ollama-local", CredentialSource: "operator"}
	if len(booked) != 1 || booked[0] != want {
		t.Errorf("booked = %+v, want one record %+v", booked, want)
	}
}

// TestBuild_RefusesAProviderWithNoDecisionDriver — a config assembled without
// load's check (a test, a future caller) still cannot build a decision model on
// a driver that has no decision endpoint.
func TestBuild_RefusesAProviderWithNoDecisionDriver(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-operator-key")
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"openai": {Driver: "openai", APIKeyEnv: "OPENAI_API_KEY"}},
		Decision:  config.DecisionConfig{Default: "gpt-x", Provider: "openai"},
	}
	if _, err := Build(cfg, nil); err == nil || !strings.Contains(err.Error(), "serves no decision models") {
		t.Errorf("err = %v, want a refusal", err)
	}
}

// decisionLane is a scheduler whose only lane is ollama-local's decision lane.
func decisionLane(n int) *concurrency.CallScheduler {
	return concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"ollama-local": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindDecision: {MaxConcurrent: n}}},
	})
}

// TestBuild_TheBlocksTimeoutReachesTheDriver — timeout_ms is the block's to
// set: a slow model is cut off at it.
func TestBuild_TheBlocksTimeoutReachesTheDriver(t *testing.T) {
	e := newEndpoint(t)
	e.delay = 300 * time.Millisecond
	s := build(t, load(t, e, "decision:\n  default: decide\n  timeout_ms: 40\n"))
	_, err := s.Decide(context.Background(), "", nil, oneQuestion)
	if decision.CodeOf(err) != decision.CodeTimeout {
		t.Errorf("timeout_ms: 40 against a 300ms reply: err = %v, want a timeout", err)
	}
}

// TestBuild_DecisionsAreCountedInTheProvidersDecisionLane — the block has no
// cap of its own: the decisions in flight to a provider are bounded by that
// provider's decision lane, which every model the block serves from it shares.
func TestBuild_DecisionsAreCountedInTheProvidersDecisionLane(t *testing.T) {
	e := newEndpoint(t)
	e.delay = 30 * time.Millisecond
	sched := decisionLane(1)
	s, err := Build(load(t, e, "decision:\n  default: decide\n"), sched)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// decide and decide-deep are one provider, so they share its lane.
			name := []string{"decide", "decide-deep"}[i%2]
			if _, err := s.Decide(context.Background(), name, nil, oneQuestion); err != nil {
				t.Errorf("Decide: %v", err)
			}
		}()
	}
	wg.Wait()
	if e.calls() != 5 || e.peak != 1 {
		t.Errorf("%d calls, %d at once; want 5 calls, one at a time", e.calls(), e.peak)
	}
	st := sched.Stats()["ollama-local"].Lanes[config.ModelKindDecision]
	if st.Grants != 5 || st.Active != 0 || st.Queued != 0 {
		t.Errorf("decision lane = %+v, want 5 grants and nothing left in it", st)
	}
}

// TestBuild_AQueuedDecisionIsNotFailedByTheTimeout — timeout_ms bounds how long
// the model may take to answer, not how long a decision waits its turn. Four
// decisions of 150ms each through a lane of one: the last waits 450ms for its
// slot, well past the 300ms timeout, and is still answered.
func TestBuild_AQueuedDecisionIsNotFailedByTheTimeout(t *testing.T) {
	e := newEndpoint(t)
	e.delay = 150 * time.Millisecond
	s, err := Build(load(t, e, "decision:\n  default: decide\n  timeout_ms: 300\n"), decisionLane(1))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Decide(context.Background(), "", nil, oneQuestion); err != nil {
				t.Errorf("a decision that only waited its turn failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if e.calls() != 4 || e.peak != 1 {
		t.Errorf("%d calls, %d at once; want 4 calls, one at a time", e.calls(), e.peak)
	}
}
