package http

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A stored definition carrying both a provider/model pin and a tier predates
// AgentDef create/fork refusing that pair. It must route by its PIN: the live
// incident was an agent forked to provider:ollama-local + a model that kept
// tier:middle, and the tier's cascade sent the run to a paid provider.

// pinTierServer wires a resolver whose middle tier is served by a reachable
// paid provider — so a resolution that consults the tier gets a real, wrong
// answer instead of an error — plus a sqlite store and a provider registry
// that tells the two providers apart.
func pinTierServer(t *testing.T) *Server {
	t.Helper()
	r := resolve.NewResolver([]string{"anthropic", "ollama-local"}, map[string][]resolve.Candidate{
		"middle": {{Provider: "anthropic", Model: "claude-sonnet-4-6"}},
	})
	r.SetReachable("anthropic", true, []string{"claude-sonnet-4-6"}, "")
	r.SetReachable("ollama-local", true, []string{"qwen3:32b"}, "")
	s := minimalServerWithResolver(t, r)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "pin-tier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s.store = st
	s.providers = &routingResolver{known: map[string]providers.Provider{
		"anthropic":    &listModelsProvider{id: "anthropic"},
		"ollama-local": &listModelsProvider{id: "ollama-local"},
	}}
	return s
}

// storeActiveDef writes an agent_defs row directly — bypassing AgentDef
// create/fork, as a row from before the refusal would have been — and makes
// it the name's active version.
func storeActiveDef(t *testing.T, s *Server, name, defID, definition string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.store.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: defID, Name: name, Definition: json.RawMessage(definition),
	}); err != nil {
		t.Fatalf("AgentDefCreate: %v", err)
	}
	if err := s.store.AgentDefSetActive(ctx, "", name, defID, ""); err != nil {
		t.Fatalf("AgentDefSetActive: %v", err)
	}
}

func TestResolveAgent_StoredRowWithPinAndTierResolvesToPin(t *testing.T) {
	s := pinTierServer(t)
	storeActiveDef(t, s, "walker", "def_both",
		`{"provider":"ollama-local","model":"qwen3:32b","tier":"middle","system_prompt":"walk"}`)

	logs, restore := captureLog(t)
	defer restore()

	prov, model, _, err := s.resolveAgent(context.Background(), "", "", "walker", "", false, nil)
	if err != nil {
		t.Fatalf("resolveAgent: %v", err)
	}
	if prov != "ollama-local" || model != "qwen3:32b" {
		t.Fatalf("resolved (%q, %q), want the pin (ollama-local, qwen3:32b) — the tier must not win", prov, model)
	}
	out := logs()
	if !strings.Contains(out, `"walker"`) || !strings.Contains(out, "def_both") || !strings.Contains(out, "resolving to the pin") {
		t.Errorf("warning should name the agent and its def id; log = %q", out)
	}

	// Once per definition, not once per run.
	if _, _, _, err := s.resolveAgent(context.Background(), "", "", "walker", "", false, nil); err != nil {
		t.Fatalf("second resolveAgent: %v", err)
	}
	if n := strings.Count(logs(), "resolving to the pin"); n != 1 {
		t.Errorf("warning logged %d times for one definition, want 1", n)
	}
}

// The crossing the incident went through: a tier agent forked to a pin by the
// AgentDef tool, then run by name. The run must land on the pin.
func TestResolveAgent_TierAgentForkedToPinRunsOnThePin(t *testing.T) {
	s := pinTierServer(t)
	tool := &builtin.AgentDef{Store: s.store, Cfg: s.cfg()}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_test"})
	ctx = tools.WithAgentDefPolicy(ctx, tools.AgentDefPolicyValue{Scopes: []string{"any"}})
	for _, call := range []string{
		`{"op":"create","name":"walker","overlay":{"tier":"middle","system_prompt":"walk"}}`,
		`{"op":"fork","name":"walker","promote":true,"overlay":{"provider":"ollama-local","model":"qwen3:32b"}}`,
	} {
		res, err := tool.Execute(ctx, json.RawMessage(call))
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %s", call, err, res.Text)
		}
	}

	prov, model, _, err := s.resolveAgent(context.Background(), "", "", "walker", "", false, nil)
	if err != nil {
		t.Fatalf("resolveAgent: %v", err)
	}
	if prov != "ollama-local" || model != "qwen3:32b" {
		t.Fatalf("forked agent ran on (%q, %q), want its pin (ollama-local, qwen3:32b)", prov, model)
	}
}

// A pinned run whose provider fails re-resolves to its pin, never to the
// tier's cascade: the fallback closure walks resolveAgent, so it must agree
// with the initial resolution.
func TestFallbackForRun_PinAndTierRowNeverFallsBackToTierProvider(t *testing.T) {
	s := pinTierServer(t)
	s.cfg().UserTiers = map[string]config.UserTier{"default": {FallbackOnError: true}}
	storeActiveDef(t, s, "walker", "def_both",
		`{"provider":"ollama-local","model":"qwen3:32b","tier":"middle"}`)
	_, restore := captureLog(t)
	defer restore()

	policy, reResolve := s.fallbackForRun("", "", "walker", "", false, nil, nil)
	if !policy.Enabled || reResolve == nil {
		t.Fatal("fallback policy not armed — fixture does not exercise the fallback path")
	}
	prov, model, _, err := reResolve(context.Background(), "ollama-local", "qwen3:32b", errors.New("503 from ollama"))
	if err != nil {
		t.Fatalf("reResolve: %v", err)
	}
	if prov.ID() != "ollama-local" || model != "qwen3:32b" {
		t.Fatalf("fallback moved the pinned run to (%q, %q), want it held on (ollama-local, qwen3:32b)", prov.ID(), model)
	}
}

// A def_id-pinned sub-run lays the stored row over the static base. A row
// that only names a tier must clear the base's WHOLE pin: a provider left
// behind reads as a pin and, with the pin winning, would silently swap the
// tier the row chose for the base's provider and the default model.
func TestApplyAgentDefOverlay_TierRowClearsStaticProviderToo(t *testing.T) {
	base := config.AgentDef{Provider: "anthropic", Model: "claude-haiku-4-5"}
	got := applyAgentDefOverlay(base, json.RawMessage(`{"tier":"low"}`))
	if got.Provider != "" || got.Model != "" || got.Tier != "low" {
		t.Errorf("tier row over pinned base = (provider %q, model %q, tier %q), want (\"\", \"\", low)",
			got.Provider, got.Model, got.Tier)
	}
	if !routesByTier(got) {
		t.Error("the merged sub-run definition does not route by its tier")
	}
}

// The mirror: a row that pins only a provider over a tier-routed base drops
// the base's tier, so the sub-run is pinned as the row says.
func TestApplyAgentDefOverlay_ProviderRowClearsStaticTier(t *testing.T) {
	base := config.AgentDef{Tier: "middle"}
	got := applyAgentDefOverlay(base, json.RawMessage(`{"provider":"ollama-local"}`))
	if got.Tier != "" || got.Provider != "ollama-local" {
		t.Errorf("provider row over tier base = (provider %q, tier %q), want (ollama-local, \"\")", got.Provider, got.Tier)
	}
}
