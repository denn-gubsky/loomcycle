package http

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	restrictedBearer = "tok-restricted"
	freeBearer       = "tok-free"
	lockedBearer     = "tok-locked"      // overrides: none
	effortOnlyBearer = "tok-effort-only" // overrides: [effort]
	quietBearer      = "tok-quiet"       // retention: none
)

// restrictedFixture is a full server with two agents and two member tokens of
// one tenant: one limited to the agent "allowed", one not limited. scripts are
// the provider's replies in call order; every later call answers "done".
func restrictedFixture(t *testing.T, scripts ...[]providers.Event) (*httptest.Server, store.Store) {
	t.Helper()
	_, ts, st := restrictedFixtureServer(t, scripts...)
	return ts, st
}

// restrictedFixtureServer is restrictedFixture with the Server itself, for a
// test that calls one of its methods directly.
func restrictedFixtureServer(t *testing.T, scripts ...[]providers.Event) (*Server, *httptest.Server, store.Store) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Env.AuthToken = "legacy-admin-secret"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	cfg.Agents = map[string]config.AgentDef{
		"allowed": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "allowed"},
		"other":   {Model: "stub-model", Tools: []string{}, SystemPrompt: "other"},
		// fixed runs on its definition only; launcher is a parent that spawns it.
		"fixed":    {Model: "stub-model", Tools: []string{}, SystemPrompt: "fixed", AllowOverrides: new(bool)},
		"launcher": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "launcher"},
	}
	done := []providers.Event{
		{Type: providers.EventText, Text: "done"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "restricted.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	member := []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}
	for _, tok := range []struct{ id, bearer, restrictions string }{
		{"def_restricted", restrictedBearer, `{"agents":["allowed"]}`},
		{"def_free", freeBearer, ""},
		{"def_locked", lockedBearer, `{"overrides":"none"}`},
		{"def_effort", effortOnlyBearer, `{"overrides":["effort"]}`},
		{"def_quiet", quietBearer, `{"retention":"none"}`},
	} {
		if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
			DefID: tok.id, Name: "u-" + tok.id, TenantID: "acme", Subject: "dev",
			TokenHash: auth.HashToken(testTokenPepper, tok.bearer), AllowedScopes: member,
			RestrictionsJSON: tok.restrictions,
		}); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(cfg, &stubResolver{p: &scriptedProvider{scripts: scripts, defaultS: done}}, []tools.Tool{}, concurrency.New(8, 8, 5*time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return srv, ts, st
}

// bearerCall sends one request with a bearer and returns the status and body.
func bearerCall(t *testing.T, ts *httptest.Server, bearer, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func runBody(agent string) string {
	return `{"agent":"` + agent + `","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
}

// A token limited to one agent starts that agent and is refused every other,
// with the same answer whether or not the other agent exists.
func TestRestrictedToken_StartsItsAgentAndIsRefusedTheRest(t *testing.T) {
	ts, _ := restrictedFixture(t)
	if code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/runs", runBody("allowed")); code != 200 {
		t.Fatalf("the allowed agent: status %d: %s", code, body)
	}
	for _, agent := range []string{"other", "no-such-agent"} {
		code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/runs", runBody(agent))
		if code != 403 || !strings.Contains(body, "agent_not_allowed") {
			t.Errorf("agent %q: status %d, want 403 agent_not_allowed: %s", agent, code, body)
		}
	}
	// The same tenant's unrestricted token is not affected.
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runBody("other")); code != 200 {
		t.Errorf("an unrestricted token starting %q: status %d: %s", "other", code, body)
	}
}

// The allowlist governs what the token STARTS. An agent the allowed one spawns
// follows that agent's own definition, which is how a client-held token for an
// orchestrating agent reaches the agent that does the work.
func TestRestrictedToken_TheAllowedAgentsOwnSpawnIsNotGated(t *testing.T) {
	spawn := []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(`{"name":"other","prompt":"work"}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	ts, st := restrictedFixture(t, spawn)
	body := `{"agent":"allowed","agent_id":"a_parent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	if code, out := bearerCall(t, ts, restrictedBearer, "POST", "/v1/runs", body); code != 200 {
		t.Fatalf("status %d: %s", code, out)
	}
	parent, err := st.GetRunByAgentID(context.Background(), "a_parent")
	if err != nil {
		t.Fatal(err)
	}
	kids, err := st.ListRunsByParentRunID(context.Background(), parent.ID)
	if err != nil || len(kids) != 1 || kids[0].Agent != "other" || kids[0].Status != store.RunCompleted {
		t.Fatalf("children of the allowed run = %+v (err %v), want one completed run of %q", kids, err, "other")
	}
}

// Every other way to start a top-level run is held to the same list.
func TestRestrictedToken_EveryOtherIntakeIsHeldToTheList(t *testing.T) {
	ts, st := restrictedFixture(t)
	ctx := context.Background()

	// A session of the other agent, owned by the same user: continuing it is
	// a new run of that agent.
	sess, err := st.CreateSession(ctx, "acme", "other", "dev")
	if err != nil {
		t.Fatal(err)
	}
	msg := `{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"more"}]}]}`
	if code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/sessions/"+sess.ID+"/messages", msg); code != 403 || !strings.Contains(body, "agent_not_allowed") {
		t.Errorf("continuing a session of another agent: status %d, want 403 agent_not_allowed: %s", code, body)
	}
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/sessions/"+sess.ID+"/messages", msg); code != 200 {
		t.Errorf("the unrestricted token continuing it: status %d: %s", code, body)
	}

	// A draft.
	draft := `{"agent":"other","start":false,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	if code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/runs", draft); code != 403 || !strings.Contains(body, "agent_not_allowed") {
		t.Errorf("a draft of another agent: status %d, want 403 agent_not_allowed: %s", code, body)
	}

	// A batch: each child is admitted by itself.
	batch := `{"spawns":[` + runBody("allowed") + `,` + runBody("other") + `]}`
	code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/runs:batch", batch)
	if code != 200 {
		t.Fatalf("batch: status %d: %s", code, body)
	}
	var res struct {
		Results []struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil || len(res.Results) != 2 {
		t.Fatalf("batch answer: %s (err %v)", body, err)
	}
	if res.Results[0].Error != "" {
		t.Errorf("the allowed child failed: %+v", res.Results[0])
	}
	if !strings.Contains(res.Results[1].Error, "not allowed") {
		t.Errorf("the other child = %+v, want it refused", res.Results[1])
	}

	// A session replay seeds a run of the target agent.
	src, _ := st.CreateSession(ctx, "acme", "allowed", "dev")
	replay := `{"agent":"other"}`
	if code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/sessions/"+src.ID+"/replay", replay); code != 403 {
		t.Errorf("replaying into another agent: status %d, want 403: %s", code, body)
	}

	// No session of the other agent was opened by the restricted token: the
	// only one is the session seeded above.
	sessions, _, err := st.ListSessions(ctx, store.SessionFilter{TenantID: "acme", AgentName: "other"}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != sess.ID {
		t.Errorf("sessions of %q = %+v, want only the seeded one", "other", sessions)
	}
}

// A token with an agent allowlist is held to the run plane. Each refused route
// here is another way to reach an agent or a model with no allowlist in the
// way; the same routes answer an unrestricted token of the same tenant as they
// always did.
func TestRestrictedToken_IsHeldToTheRunPlane(t *testing.T) {
	ts, _ := restrictedFixture(t)
	refused := []struct{ method, path string }{
		{"POST", "/v1/_agentdef"}, {"POST", "/v1/_skilldef"}, {"POST", "/v1/_teamdef"},
		{"POST", "/v1/_scheduledef"}, {"POST", "/v1/_webhookdef"}, {"POST", "/v1/_mcpserverdef"},
		{"POST", "/v1/_credentialdef"},
		{"POST", "/v1/_schedules/def_x/run-now"},
		{"POST", "/v1/_decide"},
		{"POST", "/v1/_llm/chat"}, {"POST", "/v1/chat/completions"}, {"POST", "/v1/embeddings"},
		{"POST", "/v1/_memory/search"}, {"POST", "/v1/_document"}, {"POST", "/v1/_path"},
		{"POST", "/v1/users/dev/channels/c/publish"}, {"GET", "/v1/agents/allowed/channels"},
		{"GET", "/v1/_library/agents"}, {"GET", "/v1/_usage"},
	}
	for _, r := range refused {
		code, body := bearerCall(t, ts, restrictedBearer, r.method, r.path, `{}`)
		if code != 403 || !strings.Contains(body, "restricted_token") {
			t.Errorf("%s %s: status %d, want 403 restricted_token: %s", r.method, r.path, code, body)
		}
		if code, body := bearerCall(t, ts, freeBearer, r.method, r.path, `{}`); strings.Contains(body, "restricted_token") {
			t.Errorf("%s %s refused an UNRESTRICTED token as restricted (status %d): %s", r.method, r.path, code, body)
		}
	}
	reached := []struct{ method, path string }{
		{"GET", "/v1/runs?walk_id=x"}, {"GET", "/v1/runs/r_none"}, {"GET", "/v1/_runnable-agents"},
		{"GET", "/v1/_me"}, {"GET", "/v1/users/dev/agents"}, {"GET", "/v1/agents/a_none"},
		{"POST", "/v1/agents/a_none/cancel"},
	}
	for _, r := range reached {
		if code, body := bearerCall(t, ts, restrictedBearer, r.method, r.path, ``); strings.Contains(body, "restricted_token") {
			t.Errorf("%s %s is on the run plane but was refused (status %d): %s", r.method, r.path, code, body)
		}
	}
}

func TestRunPlaneRoute(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/v1/runs", true}, {"POST", "/v1/runs:batch", true}, {"GET", "/v1/runs/r1/stream", true},
		{"DELETE", "/v1/runs/r1", true}, {"POST", "/v1/sessions/s1/messages", true},
		{"DELETE", "/v1/runs/r1/tree", false}, {"DELETE", "/v1/sessions/s1", false},
		{"GET", "/v1/runs/r1/tree", true},
		{"GET", "/v1/agents/a1", true}, {"POST", "/v1/agents/a1/cancel", true},
		{"GET", "/v1/agents/name/channels", false}, {"POST", "/v1/agents/a1/cancel/x", false},
		{"GET", "/v1/users/u/agents", true}, {"GET", "/v1/users/u/agents/stream", true},
		{"GET", "/v1/users/u/interrupts", true}, {"GET", "/v1/users/u/channels/c/peek", false},
		{"POST", "/v1/users/u/agents", false},
		{"GET", "/v1/_runnable-agents", true}, {"POST", "/v1/_runnable-agents", false},
		{"POST", "/v1/_mcp", true},
		{"POST", "/v1/_agentdef", false}, {"POST", "/v1/_decide", false},
		{"POST", "/v1/chat/completions", false}, {"POST", "/v1/runsx", false},
		{"GET", "/v1/client-tools", false},
	} {
		if got := runPlaneRoute(c.method, c.path); got != c.want {
			t.Errorf("runPlaneRoute(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// The catalogue is "what you may run": a limited token is shown its agents.
func TestRestrictedToken_RunnableAgentsListsOnlyItsOwn(t *testing.T) {
	ts, _ := restrictedFixture(t)
	names := func(bearer string) []string {
		code, body := bearerCall(t, ts, bearer, "GET", "/v1/_runnable-agents", "")
		if code != 200 {
			t.Fatalf("status %d: %s", code, body)
		}
		var res runnableAgentsResponse
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, a := range res.Agents {
			out = append(out, a.Name)
		}
		sort.Strings(out)
		return out
	}
	if got := names(restrictedBearer); len(got) != 1 || got[0] != "allowed" {
		t.Errorf("restricted token sees %v, want [allowed]", got)
	}
	if got := names(freeBearer); len(got) != 4 {
		t.Errorf("unrestricted token sees %v, want every agent", got)
	}
}

// A restrictions record this build cannot read refuses the token. Reading it
// as "no restrictions" would hand a narrowed credential all of its scopes.
func TestResolvePrincipal_UnreadableRestrictionsRefuseTheToken(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	ctx := context.Background()
	for i, rec := range []string{`{"agents":"allowed"}`, `{"agents":["a"],"from_the_future":true}`, `not json`} {
		bearer := "tok-bad-" + string(rune('a'+i))
		if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
			DefID: "def_bad_" + string(rune('a'+i)), Name: "bad", TenantID: "acme", Subject: "dev",
			TokenHash: auth.HashToken(testTokenPepper, bearer), AllowedScopes: []string{auth.ScopeRunsCreate},
			RestrictionsJSON: rec,
		}); err != nil {
			t.Fatal(err)
		}
		if p, ok := s.resolvePrincipal(ctx, bearer); ok {
			t.Errorf("record %s: the token authenticated as %+v", rec, p)
		}
	}
	seedToken(t, st, "tok-plain", "acme", "plain", []string{auth.ScopeRunsCreate}, time.Time{})
	if p, ok := s.resolvePrincipal(ctx, "tok-plain"); !ok || !p.Restrictions.IsZero() {
		t.Errorf("a token with no record: ok=%v restrictions=%+v, want it unrestricted", ok, p.Restrictions)
	}
}

// runRowCreators names every function in this package that creates a run row,
// and how each answers for the token's agent allowlist. A function that
// creates a run row and is not listed fails the test: someone has to decide
// whether it is an intake a credential can reach.
var runRowCreators = map[string]string{
	// Top-level intakes a credential reaches: each must admit the principal.
	"handleRuns":              "admit",
	"handleMessages":          "admit",
	"RunOnce":                 "admit",
	"ReplaySession":           "admit",
	"createConfiguredRunCore": "admit",
	// A walk's run: admitted by admitTeamRun before it is opened.
	"openTeamWalkRun": "exempt: admitTeamRun refuses a restricted credential's direct walk before this is reached",
	// Not intakes.
	"openOrCreateSessionAndRun":       "exempt: the shared helper the intakes call",
	"openOrCreateSessionAndQueuedRun": "exempt: the shared helper the intakes call for a run that waits for its slot",
	"prepareSubRunValues":             "exempt: a sub-run follows its parent agent's definition, not the token's list",
	"Open":                            "exempt: a channel hook's run (channel_hooks.go), opened under the hook's own identity with no credential",
	"CreateRun":                       "exempt: the no-trace store filter (no_trace.go) passing a creation through; every caller above reaches the store by it",
	"CreateQueuedRun":                 "exempt: the no-trace store filter (no_trace.go) passing a queued creation through; every caller above reaches the store by it",
}

func TestRunRowCreators_AllAdmitThePrincipal(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			creates, admits := false, false
			// Any reference counts, not only a call: the session helpers hand
			// s.store.CreateRun / CreateQueuedRun on as a value.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "CreateRun", "CreateConfiguredRun", "CreateQueuedRun",
					"openOrCreateSessionAndRun", "openOrCreateSessionAndQueuedRun":
					creates = true
				case "admitPrincipalAgent":
					admits = true
				}
				return true
			})
			if !creates {
				continue
			}
			found[fn.Name.Name] = true
			how, listed := runRowCreators[fn.Name.Name]
			switch {
			case !listed:
				t.Errorf("%s (%s) creates a run row and is not in runRowCreators: decide whether a credential can reach it, and gate it with admitPrincipalAgent if so", fn.Name.Name, name)
			case how == "admit" && !admits:
				t.Errorf("%s (%s) creates a top-level run row without calling admitPrincipalAgent: a token limited to certain agents could start any agent through it", fn.Name.Name, name)
			}
		}
	}
	for fn := range runRowCreators {
		if !found[fn] {
			t.Errorf("runRowCreators lists %s, which no longer creates a run row: remove it, or the list will hide the next real one", fn)
		}
	}
}
