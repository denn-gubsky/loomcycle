package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/limits"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

var gatewayRoutes = []string{"/v1/_llm/chat", "/v1/chat/completions", "/v1/embeddings"}

// The raw model endpoints reach a model with no agent in the way. Every
// principal that could use them still can, except an isolated user: a
// sandboxed token gets them only when the scope was granted by name.
func TestAuthMiddleware_GatewayIsClosedToAnIsolatedUser(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy-admin")
	member := []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}
	seedToken(t, st, "tok-isolated", "acme", "iso", []string{auth.ScopeUser}, time.Time{})
	seedToken(t, st, "tok-isolated-granted", "acme", "isog", []string{auth.ScopeUser, auth.ScopeLLMGateway}, time.Time{})
	seedToken(t, st, "tok-member", "acme", "mem", member, time.Time{})
	seedToken(t, st, "tok-tenant", "acme", "ops", []string{auth.ScopeTenant}, time.Time{})

	for _, path := range gatewayRoutes {
		if reached, code := runMW(t, s, "POST", path, "tok-isolated"); reached || code != http.StatusForbidden {
			t.Errorf("isolated user on %s: reached=%v code=%d, want 403", path, reached, code)
		}
		for _, tok := range []string{"tok-isolated-granted", "tok-member", "tok-tenant", "legacy-admin"} {
			if reached, code := runMW(t, s, "POST", path, tok); !reached {
				t.Errorf("%s on %s: refused with %d; every token but an isolated user's keeps the gateway", tok, path, code)
			}
		}
	}
	// The isolated user's own runs are not the gateway: a run reaches its
	// model through the resolver, and that route is unchanged.
	if reached, code := runMW(t, s, "POST", "/v1/runs", "tok-isolated"); !reached {
		t.Errorf("isolated user on POST /v1/runs: refused with %d", code)
	}
}

func TestHasScope_LLMGateway(t *testing.T) {
	for name, c := range map[string]struct {
		scopes []string
		want   bool
	}{
		"admin":            {[]string{auth.ScopeAdmin}, true},
		"tenant operator":  {[]string{auth.ScopeTenant}, true},
		"isolated user":    {[]string{auth.ScopeUser}, false},
		"member scopes":    {[]string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, false},
		"granted by name":  {[]string{auth.ScopeUser, auth.ScopeLLMGateway}, true},
		"no scopes at all": {nil, false},
	} {
		if got := auth.HasScope(c.scopes, auth.ScopeLLMGateway); got != c.want {
			t.Errorf("%s: HasScope = %v, want %v", name, got, c.want)
		}
	}
	if !auth.ValidScope(auth.ScopeLLMGateway) {
		t.Error("llm:gateway is not in the scope catalog, so no token could be granted it")
	}
	if scopes, _ := auth.GrantableUserScopes("isolated"); auth.HasScope(scopes, auth.ScopeLLMGateway) {
		t.Error("an isolated user's delegated token is granted the gateway by default")
	}
}

// gatewayBudgetServer is a server whose tenant "acme" has a hard token limit,
// with a member token of that tenant and one scripted provider reply that
// reports 10 input and 2 output tokens.
func gatewayBudgetServer(t *testing.T, hard int64, used int) (*httptest.Server, *Server) {
	t.Helper()
	prov := &scriptedProvider{defaultS: []providers.Event{
		{Type: providers.EventText, Text: "hi"},
		{Type: providers.EventUsage, Usage: &providers.Usage{InputTokens: 10, OutputTokens: 2}, StopReason: "end_turn"},
		{Type: providers.EventDone, StopReason: "end_turn"},
	}}
	cfg := makeBaseConfig()
	cfg.Env.AuthToken = "legacy-admin"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	srv, _ := makeServer(t, prov, cfg)
	seedToken(t, srv.store, "tok-alice", "acme", "alice",
		[]string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}, time.Time{})
	seedBudget(t, srv.store, hard, used)
	srv.limits = limits.New(srv.store)
	if err := srv.SeedLimits(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts, srv
}

// gatewayBodies is one valid chat request per raw chat route.
var gatewayBodies = map[string]string{
	"/v1/_llm/chat":        `{"messages":[{"role":"user","content":"hi"}],"max_tokens":50,"provider":"scripted","model":"scripted-model"}`,
	"/v1/chat/completions": `{"model":"scripted-model","messages":[{"role":"user","content":"hi"}],"loomcycle_provider":"scripted"}`,
}

// gatewayStreamBodies is the same two requests, streamed: each route has its
// own streaming path, which ends in its own usage write.
var gatewayStreamBodies = map[string]string{
	"/v1/_llm/chat":        `{"stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":50,"provider":"scripted","model":"scripted-model"}`,
	"/v1/chat/completions": `{"stream":true,"model":"scripted-model","messages":[{"role":"user","content":"hi"}],"loomcycle_provider":"scripted"}`,
}

// A gateway call spends the same tokens a run does. A caller at its hard
// ceiling was refused new runs and could go on calling the model here.
func TestGateway_RefusesACallerAtItsHardTokenLimit(t *testing.T) {
	ts, _ := gatewayBudgetServer(t, 1000, 1000)
	for path, req := range gatewayBodies {
		code, body := bearerCall(t, ts, "tok-alice", "POST", path, req)
		if code != http.StatusTooManyRequests || !strings.Contains(body, "token_limit_exceeded") {
			t.Errorf("%s at the ceiling: status %d, want 429 token_limit_exceeded: %s", path, code, body)
		}
	}
}

func TestEmbeddings_RefusesACallerAtItsHardTokenLimit(t *testing.T) {
	ts, srv := gatewayBudgetServer(t, 1000, 1000)
	srv.SetEmbedder(&fakeEmbedder{provider: "openai", model: "text-embedding-3-small", dim: 4})
	code, body := bearerCall(t, ts, "tok-alice", "POST", "/v1/embeddings", `{"model":"text-embedding-3-small","input":"hi"}`)
	if code != http.StatusTooManyRequests || !strings.Contains(body, "token_limit_exceeded") {
		t.Errorf("embeddings at the ceiling: status %d, want 429 token_limit_exceeded: %s", code, body)
	}
	under, srv2 := gatewayBudgetServer(t, 1000, 10)
	srv2.SetEmbedder(&fakeEmbedder{provider: "openai", model: "text-embedding-3-small", dim: 4})
	if code, body := bearerCall(t, under, "tok-alice", "POST", "/v1/embeddings", `{"model":"text-embedding-3-small","input":"hi"}`); code != http.StatusOK {
		t.Errorf("embeddings under the ceiling: status %d: %s", code, body)
	}
}

// A gateway call is written to the usage ledger and moves the caller's
// budget. It used to be a log line and nothing else, so it appeared in no
// usage report and the cheapest way to spend unaccounted tokens was to skip
// the agent.
func TestGateway_CallsAreRecordedAndCountAgainstTheBudget(t *testing.T) {
	ts, srv := gatewayBudgetServer(t, 1020, 1000)
	ctx := context.Background()
	total := func() int64 {
		rows, err := srv.store.UsageReport(ctx, store.UsageQuery{TenantID: "acme"})
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		for _, r := range rows {
			n += r.InputTokens + r.OutputTokens
		}
		return n
	}
	before := total()

	// Under the ceiling (1000 of 1020): the two calls are served, 12 tokens
	// each, one per wire format.
	for path, req := range gatewayBodies {
		if code, body := bearerCall(t, ts, "tok-alice", "POST", path, req); code != http.StatusOK {
			t.Fatalf("%s under the ceiling: status %d: %s", path, code, body)
		}
	}
	if got := total() - before; got != 24 {
		t.Errorf("the usage report grew by %d tokens after two gateway calls of 12, want 24", got)
	}
}

// The streaming paths end in their own usage write, one per wire format.
func TestGateway_StreamedCallsAreRecordedToo(t *testing.T) {
	ts, srv := gatewayBudgetServer(t, 100000, 0)
	rows := func() int64 {
		agg, err := srv.store.UsageReport(context.Background(), store.UsageQuery{TenantID: "acme"})
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		for _, r := range agg {
			n += r.InputTokens + r.OutputTokens
		}
		return n
	}
	before := rows()
	for path, req := range gatewayStreamBodies {
		if code, body := bearerCall(t, ts, "tok-alice", "POST", path, req); code != http.StatusOK {
			t.Fatalf("%s streamed: status %d: %s", path, code, body)
		}
	}
	if got := rows() - before; got != 24 {
		t.Errorf("the usage report grew by %d tokens after two streamed calls of 12, want 24", got)
	}
}

// The calls themselves move the budget: once they cross the ceiling, the next
// one is refused.
func TestGateway_ACallThatCrossesTheCeilingRefusesTheNext(t *testing.T) {
	ts, _ := gatewayBudgetServer(t, 1020, 1000)
	for path, req := range gatewayBodies {
		if code, body := bearerCall(t, ts, "tok-alice", "POST", path, req); code != http.StatusOK {
			t.Fatalf("%s under the ceiling: status %d: %s", path, code, body)
		}
	}
	// 1024 of 1020 now: the calls themselves moved the budget, so the next
	// one is refused.
	if code, body := bearerCall(t, ts, "tok-alice", "POST", "/v1/_llm/chat", gatewayBodies["/v1/_llm/chat"]); code != http.StatusTooManyRequests {
		t.Errorf("a third call, past the ceiling the first two crossed: status %d, want 429: %s", code, body)
	}
}
