package http

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// namedProviders resolves each provider id to its own provider, so a test can
// give one provider a key name and leave another keyless.
type namedProviders map[string]providers.Provider

func (n namedProviders) Get(id string) (providers.Provider, error) { return n[id], nil }

// serviceModelsServer is a routing server with a decision block over three
// providers: "ollama" (probed reachable, "nimble" pulled as nimble:latest,
// "clef" not pulled), "down" (probed unreachable) and "unprobed" (never
// probed). "ollama" and "unprobed" take no key; "down" takes DOWN_API_KEY.
func serviceModelsServer(t *testing.T, gateOn bool, credKeyable func(ctx context.Context, tenantID, agentName, userID, name string) bool) *Server {
	t.Helper()
	cfg := &config.Config{
		ProviderPriority: []string{"ollama"},
		Tiers:            map[string][]config.TierCandidate{"middle": {{Provider: "ollama", Model: "chat-model"}}},
		Concurrency:      config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.OperatorKeyRestriction = gateOn

	drv, err := decision.New("ollama", decision.Options{ProviderID: "ollama", BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := decision.NewService("decide", []decision.ModelSpec{
		{Name: "decide", Provider: "ollama", Model: "nimble", Driver: drv},
		{Name: "deep", Provider: "ollama", Model: "clef", Driver: drv},
		{Name: "far", Provider: "down", Model: "nimble", Driver: drv},
		{Name: "elsewhere", Provider: "unprobed", Model: "nimble", Driver: drv},
	})
	if err != nil {
		t.Fatal(err)
	}
	keyless := &scriptedProvider{}
	pr := namedProviders{
		"ollama":   keyless,
		"unprobed": keyless,
		"down":     &keyedScriptedProvider{scriptedProvider: &scriptedProvider{}, env: "DOWN_API_KEY"},
	}
	srv := New(cfg, pr, []tools.Tool{&builtin.Decision{Service: svc}}, concurrency.New(1, 1, time.Second), nil)
	res := resolve.NewResolver([]string{"ollama"}, map[string][]resolve.Candidate{
		"middle": {{Provider: "ollama", Model: "chat-model"}},
	})
	res.SetReachable("ollama", true, []string{"chat-model", "nimble:latest"}, "")
	res.SetReachable("down", false, nil, "probe failed")
	srv.SetResolver(res)
	if credKeyable != nil {
		srv.SetCredKeyable(credKeyable)
	}
	return srv
}

func decisionModel(t *testing.T, resp routingResponse, name string) routingDecisionModel {
	t.Helper()
	if resp.Decision == nil {
		t.Fatal("decision block missing")
	}
	for _, m := range resp.Decision.Models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("decision model %q missing; got %+v", name, resp.Decision.Models)
	return routingDecisionModel{}
}

func boolIs(p *bool, want bool) bool { return p != nil && *p == want }

// TestRouting_DecisionModelsCarryProviderStateAndPulledModel: each decision
// model reports its provider's probe and whether the provider lists the model.
// A bare name matches Ollama's ":latest" listing; a model the provider does not
// list is reachable but unavailable; an unreachable provider says nothing about
// the list; a provider never probed carries no state at all.
func TestRouting_DecisionModelsCarryProviderStateAndPulledModel(t *testing.T) {
	resp := routingFor(t, serviceModelsServer(t, false, nil), []string{auth.ScopeAdmin})

	if resp.Decision.Default != "decide" {
		t.Errorf("default = %q, want decide", resp.Decision.Default)
	}
	pulled := decisionModel(t, resp, "decide")
	if !pulled.Default || pulled.Provider != "ollama" || pulled.Model != "nimble" {
		t.Errorf("decide = %+v, want the default on ollama/nimble", pulled)
	}
	if !boolIs(pulled.Reachable, true) || !boolIs(pulled.Listed, true) || !boolIs(pulled.Available, true) {
		t.Errorf("decide (nimble pulled as nimble:latest) should be reachable, listed, available; got %+v", pulled)
	}

	missing := decisionModel(t, resp, "deep")
	if missing.Default {
		t.Error("deep marked default")
	}
	if !boolIs(missing.Reachable, true) || !boolIs(missing.Listed, false) || !boolIs(missing.Available, false) {
		t.Errorf("deep (clef not pulled) should be reachable, not listed, unavailable; got %+v", missing)
	}

	down := decisionModel(t, resp, "far")
	if !boolIs(down.Reachable, false) || down.Listed != nil || !boolIs(down.Available, false) {
		t.Errorf("far (provider down) should be unreachable, listed unknown, unavailable; got %+v", down)
	}

	unprobed := decisionModel(t, resp, "elsewhere")
	if unprobed.Reachable != nil || unprobed.Listed != nil || unprobed.Available != nil {
		t.Errorf("elsewhere (provider never probed) should carry no state; got %+v", unprobed)
	}
}

// TestRouting_RestrictedTenantSeesOnlyDecisionModelsItCanKey: under the
// operator-key gate a decision call that would spend the operator's key is
// refused, so the tenant view lists only models whose provider takes no key or
// for which the tenant holds its own credential. An admin sees all of them.
func TestRouting_RestrictedTenantSeesOnlyDecisionModelsItCanKey(t *testing.T) {
	tenant := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}}

	resp := routingForPrincipal(t, serviceModelsServer(t, true, nil), tenant)
	for _, m := range resp.Decision.Models {
		if m.Provider == "down" {
			t.Errorf("tenant without a DOWN_API_KEY credential sees %+v", m)
		}
	}
	if len(resp.Decision.Models) != 3 {
		t.Errorf("tenant sees %d decision models, want the 3 keyless ones", len(resp.Decision.Models))
	}

	keyable := func(_ context.Context, _, _, _, name string) bool { return name == "DOWN_API_KEY" }
	resp = routingForPrincipal(t, serviceModelsServer(t, true, keyable), tenant)
	decisionModel(t, resp, "far")

	admin := routingForPrincipal(t, serviceModelsServer(t, true, nil), auth.Principal{Scopes: []string{auth.ScopeAdmin}})
	if len(admin.Decision.Models) != 4 {
		t.Errorf("admin sees %d decision models, want all 4", len(admin.Decision.Models))
	}
}

// TestRouting_EmbedderReportsObservedHealthAndAlias: the embedder block names
// the alias memory.embedder points at and reports what the embedder's calls did
// since boot: untried before any call, failing after a failed one.
func TestRouting_EmbedderReportsObservedHealthAndAlias(t *testing.T) {
	srv := routingTestServer(t)
	srv.cfg().Memory.Embedder.Model = "local-embedding"
	srv.cfg().Models = map[string]config.ModelRef{
		"local-embedding": {Provider: "ollama-local", Model: "nomic-embed-text", Kind: config.ModelKindEmbedder},
	}
	fe := &fakeEmbedder{provider: "ollama-local", model: "nomic-embed-text", dim: 768}
	obs := providers.ObserveEmbedder(fe)
	srv.SetEmbedder(obs)

	resp := routingFor(t, srv, []string{auth.ScopeAdmin})
	e := resp.Embedder
	if e == nil {
		t.Fatal("embedder block missing")
	}
	if e.Provider != "ollama-local" || e.Model != "nomic-embed-text" || e.Alias != "local-embedding" || e.Dimension != 768 {
		t.Errorf("embedder = %+v, want ollama-local/nomic-embed-text via local-embedding, 768 wide", e)
	}
	if e.State != providers.EmbedderUntried {
		t.Errorf("state before any call = %q, want untried", e.State)
	}

	fe.failNext = true
	if _, err := obs.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("injected failure did not fail")
	}
	e = routingFor(t, srv, []string{auth.ScopeAdmin}).Embedder
	if e.State != providers.EmbedderFailing || e.Failures != 1 {
		t.Errorf("after a failed call: state %q failures %d, want failing 1", e.State, e.Failures)
	}
}

// TestRouting_ServiceModelBlocksOmittedWhenNotConfigured: a deployment with no
// embedder and no decision block gets neither key, so the UI shows no section.
func TestRouting_ServiceModelBlocksOmittedWhenNotConfigured(t *testing.T) {
	resp := routingFor(t, routingTestServer(t), []string{auth.ScopeAdmin})
	if resp.Embedder != nil {
		t.Errorf("embedder = %+v, want omitted", resp.Embedder)
	}
	if resp.Decision != nil {
		t.Errorf("decision = %+v, want omitted", resp.Decision)
	}
}
