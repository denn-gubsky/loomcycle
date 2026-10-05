package reranker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	// Registered so Build's refusal of a decision block is what fails, not a missing
	// driver: unguarded, Build would happily build an Ollama chat driver for nimble.
	_ "github.com/denn-gubsky/loomcycle/internal/providers/ollama"
)

// fakeSystemOne is an Ollama /v1/systemone double: it records every request and
// answers each with the next scripted reply (the last one repeats).
type fakeSystemOne struct {
	mu      sync.Mutex
	reqs    []systemOneRequest
	auth    []string
	replies []func(w http.ResponseWriter, r systemOneRequest)
}

func (f *fakeSystemOne) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != decisionPath || r.Method != http.MethodPost {
			t.Errorf("request %s %s, want POST %s", r.Method, r.URL.Path, decisionPath)
		}
		var req systemOneRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("request body: %v", err)
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
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

func status(code int, body string) func(http.ResponseWriter, systemOneRequest) {
	return func(w http.ResponseWriter, _ systemOneRequest) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

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

func newDecision(t *testing.T, url string, rc config.RerankerConfig) *Decision {
	t.Helper()
	r, err := BuildRanker(decisionConfig(url, rc))
	if err != nil {
		t.Fatalf("BuildRanker: %v", err)
	}
	d, ok := r.(*Decision)
	if !ok {
		t.Fatalf("BuildRanker built %T, want *Decision", r)
	}
	return d
}

var fiveTexts = []string{"a one", "b two", "c three", "d four", "e five"}

// TestDecision_OrdersByProbability — one choice over the candidates, ordered by the
// probability per option; ties keep pool order; the model is asked the measured
// question over each candidate's text, cut to max_chars; the tokens are booked.
func TestDecision_OrdersByProbability(t *testing.T) {
	f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){
		probabilities(map[string]float64{"A": 0.05, "B": 0.30, "C": 0.05, "D": 0.55, "E": 0.05}),
	}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	d := newDecision(t, srv.URL, config.RerankerConfig{})
	var booked []*providers.Usage
	d.SetOnUsage(func(_ context.Context, u *providers.Usage) { booked = append(booked, u) })

	order, rep := d.Rank(context.Background(), "which?", fiveTexts, 3)
	if fmt.Sprint(order) != "[3 1 0 2 4]" {
		t.Errorf("order = %v, want [3 1 0 2 4] (D, B, then the tied A, C, E in pool order)", order)
	}
	if !rep.Applied || rep.Candidates != 5 {
		t.Errorf("report = %+v, want applied over 5", rep)
	}
	req := f.reqs[0]
	q := req.Questions["best"]
	if req.Model != "nimble" || req.State["question"] != "which?" || q.Type != "choice" || q.Instructions != decisionInstruction {
		t.Errorf("request = %+v, want the measured choice for model nimble", req)
	}
	if len(q.Criteria) != 5 || q.Criteria["A"] != "a o" || q.Criteria["E"] != "e f" {
		t.Errorf("criteria = %v, want A..E cut to 3 runes", q.Criteria)
	}
	if len(booked) != 1 || booked[0].InputTokens != 321 || booked[0].Model != "nimble" || booked[0].Provider != "ollama-local" {
		t.Errorf("usage booked = %+v, want one record of 321 input tokens for ollama-local/nimble", booked)
	}
}

// TestDecision_ClampsToTheModelsOptions — the options are the letters A–Z: a pool
// larger than that is shown its first 26, the rest keep their places below them,
// and the report says 26 were shown.
func TestDecision_ClampsToTheModelsOptions(t *testing.T) {
	f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){
		probabilities(map[string]float64{"Z": 0.9, "A": 0.1}),
	}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	d := newDecision(t, srv.URL, config.RerankerConfig{})
	texts := make([]string, 30)
	for i := range texts {
		texts[i] = fmt.Sprintf("candidate %d", i)
	}
	order, rep := d.Rank(context.Background(), "q", texts, 0)
	if got := len(f.reqs[0].Questions["best"].Criteria); got != decisionMaxOptions {
		t.Fatalf("model shown %d options, want %d", got, decisionMaxOptions)
	}
	if order[0] != 25 || order[1] != 0 {
		t.Errorf("order starts %v, want Z (index 25) then A (0)", order[:2])
	}
	if fmt.Sprint(order[26:]) != "[26 27 28 29]" {
		t.Errorf("tail = %v, want the unshown 26..29 in place", order[26:])
	}
	if !rep.Applied || rep.Candidates != decisionMaxOptions {
		t.Errorf("report = %+v, want applied over 26", rep)
	}
}

// TestDecision_ShrinksATooLargePrompt — the model never truncates its input and
// refuses a prompt over its window; the candidates are retried at half, then a
// quarter, of their characters, and a refusal at a quarter is prompt_too_large.
func TestDecision_ShrinksATooLargePrompt(t *testing.T) {
	tooLarge := status(http.StatusBadRequest, `{"error":"prompt 0 has 10474 tokens; expected 1–8194 (input is never truncated)"}`)
	long := []string{strings.Repeat("x", 1000), strings.Repeat("y", 1000)}

	t.Run("fits at half", func(t *testing.T) {
		f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){
			tooLarge, probabilities(map[string]float64{"A": 0.2, "B": 0.8}),
		}}
		srv := httptest.NewServer(f.handler(t))
		defer srv.Close()
		order, rep := newDecision(t, srv.URL, config.RerankerConfig{}).Rank(context.Background(), "q", long, 800)
		if !rep.Applied || fmt.Sprint(order) != "[1 0]" {
			t.Errorf("order %v report %+v, want applied [1 0] after one shrink", order, rep)
		}
		if f.calls() != 2 || len(f.reqs[1].Questions["best"].Criteria["A"]) != 400 {
			t.Errorf("calls = %d, second candidate length %d, want 2 calls with 400-char candidates",
				f.calls(), len(f.reqs[1].Questions["best"].Criteria["A"]))
		}
	})
	t.Run("never fits", func(t *testing.T) {
		f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){tooLarge}}
		srv := httptest.NewServer(f.handler(t))
		defer srv.Close()
		order, rep := newDecision(t, srv.URL, config.RerankerConfig{}).Rank(context.Background(), "q", long, 800)
		if rep.Applied || rep.Reason != memory.RerankPromptTooLarge || fmt.Sprint(order) != "[0 1]" {
			t.Errorf("order %v report %+v, want search's order with prompt_too_large", order, rep)
		}
		if f.calls() != 3 || len(f.reqs[2].Questions["best"].Criteria["A"]) != 200 {
			t.Errorf("calls = %d, want 3 (800, 400, 200 chars)", f.calls())
		}
	})
}

// TestDecision_FaultsKeepSearchOrder — every other fault keeps the pool's own order
// with its reason, and a pool too small to reorder makes no call.
func TestDecision_FaultsKeepSearchOrder(t *testing.T) {
	slow := func(w http.ResponseWriter, r systemOneRequest) {
		time.Sleep(300 * time.Millisecond)
		probabilities(map[string]float64{"B": 1})(w, r)
	}
	for _, c := range []struct {
		name   string
		reply  func(http.ResponseWriter, systemOneRequest)
		texts  []string
		reason string
		calls  int
	}{
		{"server error", status(http.StatusInternalServerError, "boom"), fiveTexts, memory.RerankCallFailed, 1},
		{"other 400", status(http.StatusBadRequest, `{"error":"model not found"}`), fiveTexts, memory.RerankCallFailed, 1},
		{"not json", status(http.StatusOK, "not json"), fiveTexts, memory.RerankCallFailed, 1},
		{"no probabilities", status(http.StatusOK, `{"answers":{}}`), fiveTexts, memory.RerankCallFailed, 1},
		{"timeout", slow, fiveTexts, memory.RerankTimeout, 1},
		{"one candidate", probabilities(nil), fiveTexts[:1], memory.RerankTooFewCandidates, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){c.reply}}
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()
			d := newDecision(t, srv.URL, config.RerankerConfig{TimeoutMs: 100})
			order, rep := d.Rank(context.Background(), "q", c.texts, 0)
			for i, o := range order {
				if o != i {
					t.Fatalf("order = %v, want the identity", order)
				}
			}
			if rep.Applied || rep.Reason != c.reason || f.calls() != c.calls {
				t.Errorf("report %+v after %d calls, want reason %s after %d", rep, f.calls(), c.reason, c.calls)
			}
		})
	}
}

// TestDecision_ARestrictedRunCannotSpendTheOperatorsKey — the key rule is the Ollama
// driver's: a keyed endpoint answers to a tenant's own key first, a run barred from
// the operator's key makes no call at all, and a keyless local endpoint has no
// operator key to protect.
func TestDecision_ARestrictedRunCannotSpendTheOperatorsKey(t *testing.T) {
	t.Setenv("OLLAMA_API_KEY", "test-operator-key")
	reply := probabilities(map[string]float64{"B": 0.9, "A": 0.1})
	restricted := providers.WithOperatorKeyAllowed(context.Background(), false)

	t.Run("keyed, allowed: operator key sent", func(t *testing.T) {
		f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){reply}}
		srv := httptest.NewServer(f.handler(t))
		defer srv.Close()
		_, rep := newDecision(t, srv.URL, config.RerankerConfig{Provider: "ollama"}).Rank(context.Background(), "q", fiveTexts, 0)
		if !rep.Applied || f.auth[0] != "Bearer test-operator-key" {
			t.Errorf("report %+v, auth %q, want applied with the operator's key", rep, f.auth[0])
		}
	})
	t.Run("keyed, restricted: no call", func(t *testing.T) {
		f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){reply}}
		srv := httptest.NewServer(f.handler(t))
		defer srv.Close()
		_, rep := newDecision(t, srv.URL, config.RerankerConfig{Provider: "ollama"}).Rank(restricted, "q", fiveTexts, 0)
		if rep.Applied || rep.Reason != memory.RerankCallFailed || f.calls() != 0 {
			t.Errorf("report %+v after %d calls, want call_failed and no request", rep, f.calls())
		}
	})
	t.Run("keyed, restricted, own key: tenant key sent", func(t *testing.T) {
		f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){reply}}
		srv := httptest.NewServer(f.handler(t))
		defer srv.Close()
		ctx := providers.WithCredentialResolver(restricted, func(_ context.Context, name string) (providers.CredentialResolution, bool) {
			if name == "OLLAMA_API_KEY" {
				return providers.CredentialResolution{Value: "test-tenant-key", Scope: "tenant"}, true
			}
			return providers.CredentialResolution{}, false
		})
		_, rep := newDecision(t, srv.URL, config.RerankerConfig{Provider: "ollama"}).Rank(ctx, "q", fiveTexts, 0)
		if !rep.Applied || f.auth[0] != "Bearer test-tenant-key" {
			t.Errorf("report %+v, auth %q, want applied with the tenant's own key", rep, f.auth[0])
		}
	})
	t.Run("keyless local, restricted: allowed", func(t *testing.T) {
		f := &fakeSystemOne{replies: []func(http.ResponseWriter, systemOneRequest){reply}}
		srv := httptest.NewServer(f.handler(t))
		defer srv.Close()
		_, rep := newDecision(t, srv.URL, config.RerankerConfig{}).Rank(restricted, "q", fiveTexts, 0)
		if !rep.Applied || f.auth[0] != "" {
			t.Errorf("report %+v, auth %q, want applied with no key", rep, f.auth[0])
		}
	})
}

// TestBuildRanker_BuildsTheDeclaredKind — the default kind is listwise; decision
// needs an Ollama provider and calls its endpoint, or the block's own base_url; no
// block is an untyped nil, so main never holds a Reranker that would be called.
func TestBuildRanker_BuildsTheDeclaredKind(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-operator-key")
	if r, err := BuildRanker(&config.Config{}); err != nil || r != nil {
		t.Fatalf("no block: %v, %v; want nil, nil", r, err)
	}
	r, err := BuildRanker(openAIConfig(config.RerankerConfig{}))
	if err != nil || r.Kind() != config.RerankerKindListwise {
		t.Fatalf("default kind: %v, %v; want a listwise reranker", r, err)
	}
	if _, err := BuildRanker(decisionConfig("http://ollama.test:11434", config.RerankerConfig{Provider: "openai"})); err == nil ||
		!strings.Contains(err.Error(), "Ollama provider") {
		t.Errorf("decision on openai: err = %v, want an Ollama-provider refusal", err)
	}
	d := newDecision(t, "http://ollama.test:11434/", config.RerankerConfig{})
	if d.baseURL != "http://ollama.test:11434" || d.Kind() != config.RerankerKindDecision {
		t.Errorf("decision: base %q kind %q, want the provider's endpoint and kind decision", d.baseURL, d.Kind())
	}
	d = newDecision(t, "http://ollama.test:11434", config.RerankerConfig{BaseURL: "http://gpu.test:11434"})
	if d.baseURL != "http://gpu.test:11434" || d.keyEnvName != rerankerKeyEnvName {
		t.Errorf("override: base %q key name %q, want the block's endpoint answering only to %s",
			d.baseURL, d.keyEnvName, rerankerKeyEnvName)
	}
	if _, err := Build(decisionConfig("http://ollama.test:11434", config.RerankerConfig{})); err == nil {
		t.Error("Build (listwise) accepted a decision block; it must refuse rather than build a chat driver for it")
	}
}
