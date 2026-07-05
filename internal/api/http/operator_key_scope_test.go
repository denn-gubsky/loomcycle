package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// keyedScriptedProvider is a scriptedProvider that ALSO advertises a key
// env-var (implements providers.KeyedProvider). keyableProvidersFor keeps a
// provider only when it needs no operator key OR the tenant can key it, so a
// keyed provider with no tenant credential is filtered out of a restricted
// run's viable set — the exact path RFC AX Layer-1 routing refuses on.
type keyedScriptedProvider struct {
	*scriptedProvider
	env string
}

func (k *keyedScriptedProvider) KeyEnvName() string { return k.env }

// operatorKeyTierServer builds a resolver-backed server whose one tier ("middle")
// resolves to the keyed provider, so the RFC AX credential-aware routing path
// (resolveAgentDef → keyableProvidersFor → resolver.Resolve) actually fires. The
// registry's Get returns prov for every id (stubResolver), matching the tier's
// "keyed" provider. gateOn sets the deployment gate; credKeyable (nil = tenant
// can key nothing) decides whether the keyed provider survives the filter.
func operatorKeyTierServer(t *testing.T, prov providers.Provider, gateOn bool, credKeyable func(ctx context.Context, tenantID, agentName, userID, name string) bool) (*Server, store.Store) {
	t.Helper()
	cfg := &config.Config{
		ProviderPriority: []string{"keyed"},
		Tiers: map[string][]config.TierCandidate{
			"middle": {{Provider: "keyed", Model: "km"}},
		},
		Agents: map[string]config.AgentDef{
			"tiered": {Tier: "middle", SystemPrompt: "you are tiered"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
		Pricing: config.PricingConfig{
			Currency: "USD",
			Models:   map[string]config.ModelPrice{"keyed/km": {Input: 3, Output: 15}},
		},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.OperatorKeyRestriction = gateOn

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "opkey.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	res := resolve.NewResolver([]string{"keyed"}, map[string][]resolve.Candidate{
		"middle": {{Provider: "keyed", Model: "km"}},
	})
	res.SetReachable("keyed", true, []string{"km"}, "")
	srv.SetResolver(res)
	if credKeyable != nil {
		srv.SetCredKeyable(credKeyable)
	}
	return srv, st
}

// completingKeyed returns a keyed provider that finishes a run in one iteration,
// stamping the given credential source on its Usage (as a real driver would after
// ResolveKeyOrOperator). model must match the tier candidate for pricing.
func completingKeyed(env, credSource, credScopeID string) *keyedScriptedProvider {
	return &keyedScriptedProvider{
		env: env,
		scriptedProvider: &scriptedProvider{scripts: [][]providers.Event{{
			{Type: providers.EventText, Text: "hi"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{
				InputTokens: 1000, OutputTokens: 500, Model: "km",
				CredentialSource: credSource, CredentialScopeID: credScopeID,
			}},
		}}},
	}
}

// restrictedPrincipal is a granular, non-admin, non-legacy token that OMITS
// providers:operator-key — the shape an operator mints to make a tenant pay its
// own way (RFC AX §1).
func restrictedPrincipal() auth.Principal {
	return auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}}
}

func postRun(srv *Server, p auth.Principal, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	rr := httptest.NewRecorder()
	srv.handleRuns(rr, req)
	return rr
}

const opKeyRunBody = `{"agent":"tiered","agent_id":"%s","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`

// TestOperatorKeyScope_AdmissionRefusesWhenNoKeyableProvider is the RFC AX §5
// admission regression: gate on + a granular token lacking providers:operator-key
// + no tenant credential for the tier's keyed provider ⇒ the resolver's viable
// set is empty ⇒ 403 with the typed code operator_key_restricted. Fails on the
// stage-2 code, where resolveErrorToStatus mapped ErrOperatorKeyRestricted to the
// 400 default and handleRuns wrote plain text (no typed code).
func TestOperatorKeyScope_AdmissionRefusesWhenNoKeyableProvider(t *testing.T) {
	prov := completingKeyed("KEYED_API_KEY", "", "")
	srv, _ := operatorKeyTierServer(t, prov, true, nil) // nil credKeyable ⇒ nothing keyable

	rr := postRun(srv, restrictedPrincipal(),
		`{"agent":"tiered","agent_id":"a_refused","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "operator_key_restricted") {
		t.Fatalf("body missing typed code operator_key_restricted: %s", rr.Body.String())
	}
	if prov.scriptedProvider.calls.Load() != 0 {
		t.Errorf("provider was called %d times; a refused run must never touch the operator key", prov.scriptedProvider.calls.Load())
	}
}

// TestOperatorKeyScope_RoutesToTenantKeyableProvider: a restricted run whose
// tenant CAN key the provider (credKeyable true) routes to it and succeeds — the
// credential-aware-routing happy path (RFC AX §3). The RFC AV attribution is
// unchanged: the per-run + ledger credential_source stays "tenant".
func TestOperatorKeyScope_RoutesToTenantKeyableProvider(t *testing.T) {
	prov := completingKeyed("KEYED_API_KEY", "tenant", "acme")
	keyable := func(_ context.Context, _, _, _, name string) bool { return name == "KEYED_API_KEY" }
	srv, st := operatorKeyTierServer(t, prov, true, keyable)

	rr := postRun(srv, restrictedPrincipal(),
		`{"agent":"tiered","agent_id":"a_routed","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	run, err := st.GetRunByAgentID(context.Background(), "a_routed")
	if err != nil {
		t.Fatalf("GetRunByAgentID: %v", err)
	}
	if !run.OperatorKeyRestricted {
		t.Errorf("run.OperatorKeyRestricted = false, want true (a restricted run that routed to its own key)")
	}
	// RFC AV attribution survives restriction: the tenant key paid.
	rows, err := st.TokenUsageForRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CredentialSource != "tenant" {
		t.Fatalf("ledger rows = %+v, want 1 row with source tenant", rows)
	}
	if run.CredentialSource != "tenant" {
		t.Errorf("run summary source = %q, want tenant", run.CredentialSource)
	}
}

// TestOperatorKeyScope_GateOffRunsNormally: with the gate OFF, a token lacking
// providers:operator-key runs exactly as before — no restriction, no keyable
// filter, the run resolves and completes even though the tenant has no credential.
// The byte-identical-when-off invariant (RFC AX §1).
func TestOperatorKeyScope_GateOffRunsNormally(t *testing.T) {
	prov := completingKeyed("KEYED_API_KEY", "", "")
	srv, st := operatorKeyTierServer(t, prov, false, nil) // gate off

	rr := postRun(srv, restrictedPrincipal(),
		`{"agent":"tiered","agent_id":"a_gateoff","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	run, err := st.GetRunByAgentID(context.Background(), "a_gateoff")
	if err != nil {
		t.Fatalf("GetRunByAgentID: %v", err)
	}
	if run.OperatorKeyRestricted {
		t.Errorf("run.OperatorKeyRestricted = true with the gate OFF; must fail open")
	}
}

// TestOperatorKeyScope_ScopedPrincipalNotRestricted: with the gate ON, a token
// that HOLDS providers:operator-key runs normally — proving the scope is the grant
// that lifts the restriction.
func TestOperatorKeyScope_ScopedPrincipalNotRestricted(t *testing.T) {
	prov := completingKeyed("KEYED_API_KEY", "", "")
	srv, _ := operatorKeyTierServer(t, prov, true, nil) // gate on, nothing keyable

	scoped := auth.Principal{TenantID: "acme", Subject: "alice",
		Scopes: []string{auth.ScopeRunsCreate, auth.ScopeProvidersOperatorKey}}
	rr := postRun(srv, scoped,
		`{"agent":"tiered","agent_id":"a_scoped","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (holder of providers:operator-key is unrestricted); body: %s",
			rr.Code, rr.Body.String())
	}
}

// TestOperatorKeyScope_SubAgentInheritsRestriction is the anti-escape regression
// (RFC AX §2): a restricted parent's sub-agent inherits the restriction on its own
// run row — a child can't launder an unrestricted run out of a restricted parent.
// Uses pin-path agents (no resolver) so the assertion isolates the RunIdentity
// threading through runSubAgent, independent of routing.
func TestOperatorKeyScope_SubAgentInheritsRestriction(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "parent"},
			"child":  {Model: "stub-model", SystemPrompt: "child"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.OperatorKeyRestriction = true

	prov := &scriptedProvider{scripts: [][]providers.Event{
		{ // parent iter 1: spawn child
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{
				ID: "tu1", Name: "Agent", Input: []byte(`{"name":"child","prompt":"hi"}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{}},
		},
		{ // child
			{Type: providers.EventText, Text: "child ran"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
		{ // parent iter 2: wrap up
			{Type: providers.EventText, Text: "parent done"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
	}}

	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "opkey_sub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)

	rr := postRun(srv, restrictedPrincipal(),
		`{"agent":"parent","agent_id":"a_parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	ctx := context.Background()
	parent, err := st.GetRunByAgentID(ctx, "a_parent")
	if err != nil {
		t.Fatalf("GetRunByAgentID(parent): %v", err)
	}
	if !parent.OperatorKeyRestricted {
		t.Fatalf("parent.OperatorKeyRestricted = false, want true")
	}
	children, err := st.ListRunsByParentAgentID(ctx, "a_parent")
	if err != nil {
		t.Fatalf("ListRunsByParentAgentID: %v", err)
	}
	if len(children) == 0 {
		t.Fatalf("no sub-agent run found for parent a_parent")
	}
	for _, c := range children {
		if !c.OperatorKeyRestricted {
			t.Errorf("sub-agent run %q OperatorKeyRestricted = false; a child must inherit the parent's restriction", c.AgentID)
		}
	}
}

// TestOperatorKeyScope_ResumeReRestrictsFromRow is the RFC AX §2 resume
// regression: the operator-key restriction is restored from the runs column, not
// re-derived from a principal/gate (the background resume sweep has neither). A
// paused run stamped OperatorKeyRestricted=true whose tenant can key nothing is
// flagged unresumable with the restriction error — proving resume honors the row.
// The deployment gate is OFF here, so the ONLY source of the restriction is the
// persisted bit: if resume ignored it, resolveAgentDef would route and the run
// would re-dispatch (n=1) instead.
func TestOperatorKeyScope_ResumeReRestrictsFromRow(t *testing.T) {
	prov := completingKeyed("KEYED_API_KEY", "", "")
	srv, st := operatorKeyTierServer(t, prov, false, nil) // gate OFF, nothing keyable
	ctx := context.Background()

	sess, err := st.CreateSession(ctx, "", "tiered", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_paused", UserID: "alice", Model: "km", OperatorKeyRestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	if !run.OperatorKeyRestricted {
		t.Fatalf("setup: run did not persist OperatorKeyRestricted=true")
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "resume me"}}},
	})
	if err := st.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}

	n, warnings := srv.ResumePausedRuns(ctx)
	if n != 0 {
		t.Fatalf("re-dispatched %d, want 0 (a restricted run with no keyable provider is unresumable)", n)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "restricted") {
		t.Fatalf("warnings = %v, want 1 mentioning the operator-key restriction", warnings)
	}
	got, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunFailed {
		t.Errorf("resumed run status = %q, want failed (flagged unresumable)", got.Status)
	}
}

// TestOperatorKeyScope_GatewayRefusesRestrictedPrincipal is the fail-closed
// closure of the LLM-gateway hole (RFC AX): /v1/chat/completions calls provider
// .Call directly and does not wire BYO-key, so a restricted principal must be
// refused 403 there rather than spend the operator's key. Fails on the stage-2
// code, where the gateway had no restriction check and the run returned 200.
func TestOperatorKeyScope_GatewayRefusesRestrictedPrincipal(t *testing.T) {
	prov := &scriptedProvider{scripts: [][]providers.Event{{
		{Type: providers.EventText, Text: "hi"},
		{Type: providers.EventUsage, Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}, StopReason: "end_turn"},
		{Type: providers.EventDone, StopReason: "end_turn"},
	}}}
	srv, _ := makeServer(t, prov, makeBaseConfig())
	srv.cfg.Env.OperatorKeyRestriction = true

	body := `{"model":"km","messages":[{"role":"user","content":"hi"}],"loomcycle_provider":"scripted"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), restrictedPrincipal()))
	rr := httptest.NewRecorder()
	srv.handleOpenAICompatChat(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("gateway status = %d, want 403; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "operator_key_restricted") {
		t.Fatalf("gateway body missing operator_key_restricted: %s", rr.Body.String())
	}
	if prov.calls.Load() != 0 {
		t.Errorf("provider called %d times; a refused gateway request must never reach provider.Call", prov.calls.Load())
	}
}

// TestOperatorKeyScope_GatewayAllowsScopedPrincipal: a principal holding
// providers:operator-key (or any unrestricted principal) uses the gateway
// normally — proving the closure refuses ONLY the restricted principal.
func TestOperatorKeyScope_GatewayAllowsScopedPrincipal(t *testing.T) {
	prov := &scriptedProvider{scripts: [][]providers.Event{{
		{Type: providers.EventText, Text: "hi"},
		{Type: providers.EventUsage, Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}, StopReason: "end_turn"},
		{Type: providers.EventDone, StopReason: "end_turn"},
	}}}
	srv, _ := makeServer(t, prov, makeBaseConfig())
	srv.cfg.Env.OperatorKeyRestriction = true

	scoped := auth.Principal{TenantID: "acme", Subject: "alice",
		Scopes: []string{auth.ScopeRunsCreate, auth.ScopeProvidersOperatorKey}}
	body := `{"model":"km","messages":[{"role":"user","content":"hi"}],"loomcycle_provider":"scripted"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req = req.WithContext(auth.WithPrincipal(req.Context(), scoped))
	rr := httptest.NewRecorder()
	srv.handleOpenAICompatChat(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200 for a scoped principal; body: %s", rr.Code, rr.Body.String())
	}
}
