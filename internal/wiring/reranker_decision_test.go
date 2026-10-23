package wiring

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/reranker"
	// Registered so the listwise refusal of a decision block is what fails, not a
	// missing driver: unguarded, it would happily build an Ollama chat driver for
	// nimble.
	_ "github.com/denn-gubsky/loomcycle/internal/providers/ollama"
)

// systemOneRequest is the wire shape of the one request a decision rerank
// sends, as the endpoint double decodes it (the reranker package pins it to the
// byte).
type systemOneRequest struct {
	Model     string `json:"model"`
	Questions map[string]struct {
		Criteria map[string]string `json:"criteria"`
	} `json:"questions"`
}

// fakeSystemOne is an Ollama /v1/systemone double: it records every request and
// answers each with the next scripted reply (the last one repeats).
type fakeSystemOne struct {
	mu      sync.Mutex
	reqs    []systemOneRequest
	replies []func(w http.ResponseWriter, r systemOneRequest)
}

func (f *fakeSystemOne) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			t.Errorf("request %s %s, want POST /v1/systemone", r.Method, r.URL.Path)
		}
		var req systemOneRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("request body: %v", err)
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		i := len(f.reqs) - 1
		if i >= len(f.replies) {
			i = len(f.replies) - 1
		}
		reply := f.replies[i]
		f.mu.Unlock()
		reply(w, req)
	}
}

func (f *fakeSystemOne) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// probabilities answers with the given probability per letter and some usage.
func probabilities(p map[string]float64) func(http.ResponseWriter, systemOneRequest) {
	return func(w http.ResponseWriter, _ systemOneRequest) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "nimble",
			"answers": map[string]any{"best": map[string]any{"type": "choice", "probabilities": p}},
			"usage":   map[string]int{"input_tokens": 321, "output_tokens": 1},
		})
	}
}

var fiveTexts = []string{"a one", "b two", "c three", "d four", "e five"}

// decisionConfig declares a decision reranker on an Ollama provider served by url.
func decisionConfig(url string, rc config.RerankerConfig) *config.Config {
	rc.Kind = config.RerankerKindDecision
	if rc.Provider == "" {
		rc.Provider = "ollama-local"
	}
	if rc.Model == "" {
		rc.Model = "nimble"
	}
	return &config.Config{
		Providers: map[string]config.ProviderConfig{
			"ollama-local": {Driver: "ollama", BaseURL: url},
			"ollama":       {Driver: "ollama", BaseURL: url, APIKeyEnv: "OLLAMA_API_KEY"},
			"openai":       {Driver: "openai", APIKeyEnv: "OPENAI_API_KEY"},
		},
		Memory: config.MemoryConfig{Reranker: rc},
	}
}

func newDecision(t *testing.T, url string, rc config.RerankerConfig) *reranker.Decision {
	t.Helper()
	r, err := Reranker(decisionConfig(url, rc), nil)
	if err != nil {
		t.Fatalf("Reranker: %v", err)
	}
	d, ok := r.(*reranker.Decision)
	if !ok {
		t.Fatalf("Reranker built %T, want *reranker.Decision", r)
	}
	return d
}

// TestReranker_ADecisionAliasReachesTheProviderAsItsModel — `memory.reranker.model`
// naming a models: alias asks the provider for the model the alias names. The
// alias name itself is not a model the provider serves.
func TestReranker_ADecisionAliasReachesTheProviderAsItsModel(t *testing.T) {
	f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){
		probabilities(map[string]float64{"A": 0.1, "B": 0.9}),
	}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	cfg := decisionConfig(srv.URL, config.RerankerConfig{Model: "decide"})
	cfg.Memory.Reranker.Provider = ""
	cfg.Models = map[string]config.ModelRef{"decide": {Provider: "ollama-local", Model: "nimble", Kind: config.ModelKindDecision}}
	r, err := Reranker(cfg, nil)
	if err != nil {
		t.Fatalf("Reranker: %v", err)
	}
	if _, rep := r.Rank(context.Background(), "q", fiveTexts[:2], 0); !rep.Applied {
		t.Fatalf("report = %+v, want applied", rep)
	}
	if got := f.reqs[0].Model; got != "nimble" || r.ModelID() != "nimble" || r.ProviderID() != "ollama-local" {
		t.Errorf("asked for %q as %s/%s, want nimble on ollama-local", got, r.ProviderID(), r.ModelID())
	}
}

// TestReranker_DecisionReranksAreCountedInTheProvidersDecisionLane — the decision
// kind has no cap of its own: its calls take the decision lane of the provider
// the block resolved to, so that lane's max_concurrent is what bounds them.
func TestReranker_DecisionReranksAreCountedInTheProvidersDecisionLane(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	held := func(w http.ResponseWriter, r systemOneRequest) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		probabilities(map[string]float64{"A": 0.1, "B": 0.9})(w, r)
	}
	f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){held}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"ollama-local": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindDecision: {MaxConcurrent: 1}}},
	})
	d, err := Reranker(decisionConfig(srv.URL, config.RerankerConfig{}), sched)
	if err != nil {
		t.Fatalf("Reranker: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, rep := d.Rank(context.Background(), "q", fiveTexts, 0); !rep.Applied {
				t.Errorf("report = %+v, want applied", rep)
			}
		}()
	}
	wg.Wait()
	if f.calls() != 4 || peak != 1 {
		t.Errorf("%d calls, %d at once; want 4 calls, one at a time", f.calls(), peak)
	}
	if st := sched.Stats()["ollama-local"].Lanes[config.ModelKindDecision]; st.Grants != 4 || st.Active != 0 {
		t.Errorf("decision lane = %+v, want 4 grants and none in flight", st)
	}
}

// TestReranker_ADecisionRerankThatWaitsPastItsTimeoutKeepsSearchOrder —
// timeout_ms bounds a rerank, its wait for a lane slot included: a search does
// not block behind a full lane, it keeps its own order and says the rerank timed
// out.
func TestReranker_ADecisionRerankThatWaitsPastItsTimeoutKeepsSearchOrder(t *testing.T) {
	release := make(chan struct{})
	held := func(w http.ResponseWriter, r systemOneRequest) {
		<-release
		probabilities(map[string]float64{"A": 0.1, "B": 0.9})(w, r)
	}
	f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){held}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	defer close(release) // before srv.Close, which waits for the held handler
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"ollama-local": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindDecision: {MaxConcurrent: 1}}},
	})
	d, err := Reranker(decisionConfig(srv.URL, config.RerankerConfig{TimeoutMs: 2000}), sched)
	if err != nil {
		t.Fatalf("Reranker: %v", err)
	}
	go d.Rank(context.Background(), "holds the slot", fiveTexts[:2], 0)
	deadline := time.Now().Add(3 * time.Second)
	for f.calls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first rerank never reached the endpoint")
		}
		time.Sleep(time.Millisecond)
	}
	quick, err := Reranker(decisionConfig(srv.URL, config.RerankerConfig{TimeoutMs: 60}), sched)
	if err != nil {
		t.Fatalf("Reranker: %v", err)
	}
	order, rep := quick.Rank(context.Background(), "waits", fiveTexts[:2], 0)
	if rep.Applied || rep.Reason != memory.RerankTimeout {
		t.Errorf("report = %+v, want a timeout", rep)
	}
	if len(order) != 2 || order[0] != 0 || order[1] != 1 {
		t.Errorf("order = %v, want search's own", order)
	}
	if n := f.calls(); n != 1 {
		t.Errorf("the endpoint saw %d calls, want 1: the waiting rerank must not reach it", n)
	}
}

// TestReranker_TheDefaultPoolFollowsTheKind — the crossing from config to the
// pool a search shows: a listwise block is shown 40 candidates by default and a
// decision block 20 (it names at most 26 options, so more would only be clamped).
// An agent's own candidates win over either.
func TestReranker_TheDefaultPoolFollowsTheKind(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-operator-key")
	listwise, err := Reranker(openAIConfig(config.RerankerConfig{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	decision := newDecision(t, "http://ollama.test:11434", config.RerankerConfig{})
	var o memory.RerankOptions
	if n := o.EffectiveCandidates(listwise); n != 40 {
		t.Errorf("listwise default = %d, want 40", n)
	}
	if n := o.EffectiveCandidates(decision); n != 20 {
		t.Errorf("decision default = %d, want 20", n)
	}
	o.Candidates = 12
	if o.EffectiveCandidates(listwise) != 12 || o.EffectiveCandidates(decision) != 12 {
		t.Errorf("explicit candidates = %d/%d, want 12 for both kinds",
			o.EffectiveCandidates(listwise), o.EffectiveCandidates(decision))
	}
}

// TestReranker_BuildsTheDeclaredKind — the default kind is listwise; decision
// needs an Ollama provider and calls its endpoint, or the block's own base_url; no
// block is an untyped nil, so main never holds a Reranker that would be called.
func TestReranker_BuildsTheDeclaredKind(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-operator-key")
	t.Setenv("OLLAMA_API_KEY", "test-ollama-key")
	if r, err := Reranker(&config.Config{}, nil); err != nil || r != nil {
		t.Fatalf("no block: %v, %v; want nil, nil", r, err)
	}
	r, err := Reranker(openAIConfig(config.RerankerConfig{}), nil)
	if err != nil || r.Kind() != config.RerankerKindListwise {
		t.Fatalf("default kind: %v, %v; want a listwise reranker", r, err)
	}
	if _, err := Reranker(decisionConfig("http://ollama.test:11434", config.RerankerConfig{Provider: "openai"}), nil); err == nil ||
		!strings.Contains(err.Error(), "Ollama provider") {
		t.Errorf("decision on openai: err = %v, want an Ollama-provider refusal", err)
	}
	if d := newDecision(t, "http://ollama.test:11434/", config.RerankerConfig{}); d.Kind() != config.RerankerKindDecision {
		t.Errorf("decision: kind %q, want decision", d.Kind())
	}
	// What the block resolves to (the reranker package tests what it does with it).
	for _, c := range []struct {
		name               string
		rc                 config.RerankerConfig
		wantBase, wantName string
	}{
		{"provider endpoint", config.RerankerConfig{}, "http://ollama.test:11434", ""},
		{"keyed provider", config.RerankerConfig{Provider: "ollama"}, "http://ollama.test:11434", "OLLAMA_API_KEY"},
		{"own endpoint", config.RerankerConfig{BaseURL: "http://gpu.test:11434"}, "http://gpu.test:11434", rerankerKeyEnvName},
	} {
		cfg := decisionConfig("http://ollama.test:11434", c.rc)
		rc, driver, opts, err := decisionEndpoint(cfg, cfg.Memory.Reranker)
		if err != nil {
			t.Fatalf("%s: decisionEndpoint: %v", c.name, err)
		}
		if driver != "ollama" || opts.BaseURL != c.wantBase || opts.KeyEnvName != c.wantName || rc.Model != "nimble" {
			t.Errorf("%s: driver %q base %q key name %q model %q; want ollama, %q, %q, nimble",
				c.name, driver, opts.BaseURL, opts.KeyEnvName, rc.Model, c.wantBase, c.wantName)
		}
	}
	if _, err := listwiseReranker(decisionConfig("http://ollama.test:11434", config.RerankerConfig{}), nil); err == nil {
		t.Error("the listwise build accepted a decision block; it must refuse rather than build a chat driver for it")
	}
}
