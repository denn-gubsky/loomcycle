package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A person reads only their own conversations. A tenant member — isolated or
// not — sees the runs and sessions it started and nothing else in its tenant:
// not a colleague's, and not a tenant-level run no user started (an operator's,
// a schedule's, a webhook's). A tenant operator sees every run of its tenant,
// an admin every run.

// tenantMemberCtx is a non-isolated member: the scopes a tenant-mode user's
// token is minted with.
func tenantMemberCtx(t *testing.T, tenant, subject string) context.Context {
	t.Helper()
	scopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	p := auth.Principal{TenantID: tenant, Subject: subject, Scopes: scopes}
	if auth.IsIsolated(p, true) || auth.HasScope(p.Scopes, auth.ScopeTenant) {
		t.Fatal("a tenant-mode member must be neither isolated nor a tenant operator for this test to mean anything")
	}
	return auth.WithPrincipal(context.Background(), p)
}

// ownRunsFixture seeds four finished runs, each with a prompt, an answer, a
// transcript event and a pending question: alice's and bob's in acme, a
// tenant-level run in acme that no user started, and carol's in globex.
type ownRunsFixture struct {
	srv  *Server
	runs map[string]store.Run // by row name
}

var ownRunRows = []struct{ name, tenant, user string }{
	{"alice", "acme", "alice"},
	{"bob", "acme", "bob"},
	{"tenant-level", "acme", ""},
	{"globex", "globex", "carol"},
}

func newOwnRunsFixture(t *testing.T) *ownRunsFixture {
	t.Helper()
	srv, cleanup := channelFanFixture(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	f := &ownRunsFixture{srv: srv, runs: map[string]store.Run{}}
	for _, row := range ownRunRows {
		run := seedTenantRun(t, srv.store, row.tenant, row.user, "a_"+strings.ReplaceAll(row.name, "-", "_"))
		srv.makeRecordingEmit(ctx, run.ID, tools.RunIdentityValue{}, run.SessionID, runStateMeta{}, func(providers.Event) {})(
			snapshotEvent("system", "secret plan of "+row.name))
		if err := srv.store.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn",
			store.Usage{Result: []byte(`{"final_text":"answer for ` + row.name + `"}`)}, ""); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
		if _, err := srv.store.InterruptCreate(ctx, store.InterruptRow{
			InterruptID: store.MintInterruptID(time.Now()), RunID: run.ID, UserID: row.user,
			Question: "question of " + row.name, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("InterruptCreate: %v", err)
		}
		f.runs[row.name] = run
	}
	return f
}

// ownRunCallers names each caller and the rows it must see.
func ownRunCallers(t *testing.T) []struct {
	name string
	ctx  context.Context
	sees map[string]bool
} {
	return []struct {
		name string
		ctx  context.Context
		sees map[string]bool
	}{
		{"member", tenantMemberCtx(t, "acme", "bob"), map[string]bool{"bob": true}},
		{"isolated member", tenantPrincipalCtx("acme", "bob", auth.ScopeUser), map[string]bool{"bob": true}},
		{"tenant operator", tenantOperatorCtx("acme"), map[string]bool{"alice": true, "bob": true, "tenant-level": true}},
		{"another tenant's operator", tenantOperatorCtx("globex"), map[string]bool{"globex": true}},
		{"admin", adminCtx(""), map[string]bool{"alice": true, "bob": true, "tenant-level": true, "globex": true}},
	}
}

func TestRunReads_AMemberReadsOnlyItsOwnRuns(t *testing.T) {
	f := newOwnRunsFixture(t)
	srv := f.srv
	serve := func(ctx context.Context, h http.HandlerFunc, path string, values map[string]string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		for k, v := range values {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code, rec.Body.String()
	}
	totalOf := func(body string) int {
		var b struct {
			Total int `json:"total"`
		}
		_ = json.Unmarshal([]byte(body), &b)
		return b.Total
	}
	for _, c := range ownRunCallers(t) {
		for _, row := range ownRunRows {
			run, want := f.runs[row.name], c.sees[row.name]
			check := func(what string, got bool, detail string) {
				t.Helper()
				if got != want {
					t.Errorf("%s reading %s's run through %s: visible=%v, want %v (%s)", c.name, row.name, what, got, want, detail)
				}
			}

			code, body := serve(c.ctx, srv.handleGetAgent, "/v1/agents/"+run.AgentID, map[string]string{"agent_id": run.AgentID})
			check("GET /v1/agents/{agent_id}", code == http.StatusOK && strings.Contains(body, "answer for "+row.name), body)
			code, body = serve(c.ctx, srv.handleGetRun, "/v1/runs/"+run.ID, map[string]string{"run_id": run.ID})
			check("GET /v1/runs/{run_id}", code == http.StatusOK && strings.Contains(body, "answer for "+row.name), body)
			code, body = getPrompt(t, srv, c.ctx, run.ID)
			check("GET /v1/runs/{run_id}/prompt", code == http.StatusOK && strings.Contains(body, "secret plan of "+row.name), body)
			code, body = serve(c.ctx, srv.handleRunStream, "/v1/runs/"+run.ID+"/stream", map[string]string{"run_id": run.ID})
			check("GET /v1/runs/{run_id}/stream", code == http.StatusOK, body)
			code, body = serve(c.ctx, srv.handleTranscript, "/v1/sessions/"+run.SessionID+"/transcript", map[string]string{"id": run.SessionID})
			check("GET /v1/sessions/{id}/transcript", code == http.StatusOK, body)
			code, body = serve(c.ctx, srv.handleListRunInterrupts, "/v1/runs/"+run.ID+"/interrupts?status=all", map[string]string{"run_id": run.ID})
			check("GET /v1/runs/{run_id}/interrupts", code == http.StatusOK && totalOf(body) == 1, body)

			// The connector backs gRPC and the MCP tools.
			got, err := srv.GetRun(c.ctx, run.AgentID)
			check("connector GetRun", err == nil && strings.Contains(string(got.Result), "answer for "+row.name), errString(err))
			err = srv.StreamRunEvents(c.ctx, run.ID, 0, func(providers.Event) error { return nil })
			check("connector StreamRunEvents", err == nil, errString(err))
			if err != nil && !errors.Is(err, connector.ErrRunNotInFlight) {
				t.Errorf("%s streaming %s's run: %v, want the opaque not-in-flight", c.name, row.name, err)
			}
		}
	}
}

func TestRunLists_AMemberListsOnlyItsOwnRuns(t *testing.T) {
	f := newOwnRunsFixture(t)
	srv := f.srv
	listAgents := func(ctx context.Context, user string) []string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/users/"+user+"/agents", nil).WithContext(ctx)
		req.SetPathValue("user_id", user)
		rec := httptest.NewRecorder()
		srv.handleListUserAgents(rec, req)
		var body struct {
			Agents []agentResponse `json:"agents"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/users/%s/agents = %d %s", user, rec.Code, rec.Body.String())
		}
		var ids []string
		for _, a := range body.Agents {
			ids = append(ids, a.AgentID)
		}
		return ids
	}
	userInterrupts := func(ctx context.Context, user string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/users/"+user+"/interrupts?status=all", nil).WithContext(ctx)
		req.SetPathValue("user_id", user)
		rec := httptest.NewRecorder()
		srv.handleListUserInterrupts(rec, req)
		var body struct {
			Total int `json:"total"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Total
	}
	streamCode := func(ctx context.Context, user string) int {
		t.Helper()
		// Only a refused stream is driven: an admitted one holds the request
		// open, and this server has no run-state bus, so it answers 503 at once.
		req := httptest.NewRequest(http.MethodGet, "/v1/users/"+user+"/agents/stream", nil).WithContext(ctx)
		req.SetPathValue("user_id", user)
		rec := httptest.NewRecorder()
		srv.handleStreamUserAgents(rec, req)
		return rec.Code
	}
	connectorList := func(ctx context.Context, user string) int {
		t.Helper()
		rows, err := srv.ListRuns(ctx, connector.ListRunsFilter{UserID: user})
		if err != nil {
			t.Fatalf("connector ListRuns(%s): %v", user, err)
		}
		return len(rows)
	}

	for _, member := range []struct {
		name string
		ctx  context.Context
	}{
		{"member", tenantMemberCtx(t, "acme", "bob")},
		{"isolated member", tenantPrincipalCtx("acme", "bob", auth.ScopeUser)},
	} {
		if got := listAgents(member.ctx, "alice"); len(got) != 0 {
			t.Errorf("%s listing alice's runs = %v, want none", member.name, got)
		}
		if got := listAgents(member.ctx, "bob"); len(got) != 1 || got[0] != f.runs["bob"].AgentID {
			t.Errorf("%s listing its own runs = %v, want [%s]", member.name, got, f.runs["bob"].AgentID)
		}
		if n := userInterrupts(member.ctx, "alice"); n != 0 {
			t.Errorf("%s listing alice's questions = %d, want none", member.name, n)
		}
		if n := userInterrupts(member.ctx, "bob"); n != 1 {
			t.Errorf("%s listing its own questions = %d, want 1", member.name, n)
		}
		if code := streamCode(member.ctx, "alice"); code != http.StatusNotFound {
			t.Errorf("%s streaming alice's run states = %d, want the opaque 404", member.name, code)
		}
		if n := connectorList(member.ctx, "alice"); n != 0 {
			t.Errorf("%s listing alice's runs through the connector (MCP list_runs) = %d rows, want none", member.name, n)
		}
		if n := connectorList(member.ctx, "bob"); n != 1 {
			t.Errorf("%s listing its own runs through the connector = %d rows, want 1", member.name, n)
		}
	}

	op := tenantOperatorCtx("acme")
	if got := listAgents(op, "alice"); len(got) != 1 {
		t.Errorf("tenant operator listing alice's runs = %v, want her run", got)
	}
	if n := userInterrupts(op, "alice"); n != 1 {
		t.Errorf("tenant operator listing alice's questions = %d, want 1", n)
	}
	if code := streamCode(op, "alice"); code == http.StatusNotFound {
		t.Error("tenant operator streaming alice's run states got the member's 404")
	}
	if n := connectorList(op, "alice"); n != 1 {
		t.Errorf("tenant operator listing alice's runs through the connector = %d rows, want 1", n)
	}
	if got := listAgents(tenantOperatorCtx("globex"), "alice"); len(got) != 0 {
		t.Errorf("another tenant's operator listing acme's alice = %v, want none", got)
	}
}

// The audit view is every event of the caller's tenant: the transcripts of
// every run. A member's view holds only its own sessions' events.
func TestListEvents_AMemberSeesOnlyItsOwnSessionsEvents(t *testing.T) {
	f := newOwnRunsFixture(t)
	srv := f.srv
	sessions := func(ctx context.Context) map[string]bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/_events?limit=500", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		srv.handleListEvents(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/_events = %d %s", rec.Code, rec.Body.String())
		}
		var body listEventsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, ev := range body.Events {
			seen[ev.SessionID] = true
		}
		return seen
	}
	for _, c := range ownRunCallers(t) {
		if c.name == "isolated member" {
			continue // the route gate refuses an isolated member before the handler
		}
		got := sessions(c.ctx)
		for _, row := range ownRunRows {
			if want := c.sees[row.name]; got[f.runs[row.name].SessionID] != want {
				t.Errorf("%s: events of %s's session visible=%v, want %v", c.name, row.name, !want, want)
			}
		}
	}
}

// Acting on a run follows reading it: a member cancels, compacts and answers
// the questions of its own runs only, and another user's or a tenant-level run
// answers as an unknown id does. A tenant operator acts on every run of its
// tenant.
func TestRunActions_AMemberActsOnlyOnItsOwnRuns(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		s, st := tokenAuthServer(t, "")
		aliceLive := registerLiveCancel(t, s, seedRunInTenant(t, st, "acme", "alice", "a_alice"), "a_alice", "alice")
		tenantLive := registerLiveCancel(t, s, seedRunInTenant(t, st, "acme", "", "a_tenant"), "a_tenant", "")
		bobLive := registerLiveCancel(t, s, seedRunInTenant(t, st, "acme", "bob", "a_bob"), "a_bob", "bob")
		bob := tenantMemberCtx(t, "acme", "bob")

		cancelAs := func(ctx context.Context, agentID string) (int, string) {
			r := httptest.NewRequest(http.MethodPost, "/v1/agents/"+agentID+"/cancel", nil)
			r.SetPathValue("agent_id", agentID)
			rr := httptest.NewRecorder()
			s.handleCancelAgent(rr, r.WithContext(ctx))
			return rr.Code, rr.Body.String()
		}
		ghostCode, ghostBody := cancelAs(bob, "a_ghost")
		for _, id := range []string{"a_alice", "a_tenant"} {
			code, body := cancelAs(bob, id)
			if want := strings.ReplaceAll(ghostBody, "a_ghost", id); code != ghostCode || body != want {
				t.Errorf("member cancelling %s over HTTP = %d %q, want the unknown-id reply %d %q", id, code, body, ghostCode, want)
			}
			if _, err := s.CancelRun(bob, id, ""); err == nil {
				t.Errorf("member cancelling %s through the connector (gRPC CancelAgent, MCP cancel_run) succeeded", id)
			}
		}
		if aliceLive() || tenantLive() {
			t.Fatal("a member cancelled a run it did not start")
		}
		if code, body := cancelAs(bob, "a_bob"); code != http.StatusOK || !bobLive() {
			t.Errorf("member cancelling its own run = %d %s, want 200 and cancelled", code, body)
		}
		if code, body := cancelAs(tenantOperatorCtx("acme"), "a_tenant"); code != http.StatusOK || !tenantLive() {
			t.Errorf("tenant operator cancelling a tenant-level run = %d %s, want 200 and cancelled", code, body)
		}
	})

	t.Run("compact", func(t *testing.T) {
		srv, prov := compactFixture(t)
		aliceSess, aliceRun := seedTenantConversation(t, srv, "acme", "alice", "a_alice")
		bobSess, bobRun := seedTenantConversation(t, srv, "acme", "bob", "a_bob")
		bob := tenantMemberCtx(t, "acme", "bob")
		compactAs := func(ctx context.Context, runID string) (int, string) {
			r := httptest.NewRequest(http.MethodPost, "/v1/runs/"+runID+"/compact", strings.NewReader(`{}`))
			r.SetPathValue("run_id", runID)
			rr := httptest.NewRecorder()
			srv.handleCompactRun(rr, r.WithContext(ctx))
			return rr.Code, rr.Body.String()
		}
		ghostCode, ghostBody := compactAs(bob, "run_ghost")
		if code, body := compactAs(bob, aliceRun); code != ghostCode || body != ghostBody {
			t.Errorf("member compacting alice's run = %d %q, want the unknown-id reply %d %q", code, body, ghostCode, ghostBody)
		}
		if _, err := srv.CompactRun(bob, aliceRun); err == nil {
			t.Error("member compacting alice's run through the connector (gRPC CompactRun, MCP compact_run) succeeded")
		}
		if prov.calls.Load() != 0 || compactionMarkers(t, srv, aliceSess) != 0 {
			t.Fatal("a member compacted another user's run")
		}
		if code, body := compactAs(bob, bobRun); code != http.StatusOK || compactionMarkers(t, srv, bobSess) != 1 {
			t.Errorf("member compacting its own run = %d %s, want 200 and a marker", code, body)
		}
	})

	t.Run("answer a question", func(t *testing.T) {
		f := newOwnRunsFixture(t)
		srv := f.srv
		bob := tenantMemberCtx(t, "acme", "bob")
		for _, name := range []string{"alice", "tenant-level"} {
			rows, err := srv.store.InterruptListByRun(context.Background(), f.runs[name].ID, "")
			if err != nil || len(rows) != 1 {
				t.Fatalf("InterruptListByRun(%s) = %v, %v", name, rows, err)
			}
			_, err = srv.InterruptionResolve(bob, connector.InterruptionResolveRequest{
				RunID: f.runs[name].ID, InterruptID: rows[0].InterruptID, Answer: "yes",
			})
			var nf *store.ErrNotFound
			if !errors.As(err, &nf) {
				t.Errorf("member answering %s's question (MCP interruption_resolve) = %v, want the opaque not-found", name, err)
			}
		}
	})
}

// memberRunRoutes classifies every HTTP route that addresses a run, a session
// or a user's runs, by how a member's request for ANOTHER user's is answered:
// "hidden" — a refusal (an opaque 404, or the 4xx the route gives a run it
// cannot address); "empty" — 200 with none of that user's rows. The census
// below derives the route list from the mux registrations, so a route added
// without a row here fails it.
var memberRunRoutes = map[string]string{
	"DELETE /v1/runs/{run_id}":                                 "hidden",
	"DELETE /v1/runs/{run_id}/tree":                            "hidden",
	"DELETE /v1/sessions/{id}":                                 "hidden",
	"GET /v1/_events":                                          "empty",
	"GET /v1/_metrics/runs/{run_id}":                           "hidden", // operator-admin route
	"GET /v1/_resident":                                        "empty",
	"GET /v1/agents/{agent_id}":                                "hidden",
	"GET /v1/runs":                                             "hidden", // walk_id / parent_run_id of another user's run
	"GET /v1/runs/{run_id}":                                    "hidden",
	"GET /v1/runs/{run_id}/breakpoints":                        "hidden",
	"GET /v1/runs/{run_id}/config":                             "hidden",
	"GET /v1/runs/{run_id}/effective-config":                   "hidden",
	"GET /v1/runs/{run_id}/inbox/{path...}":                    "hidden",
	"GET /v1/runs/{run_id}/interrupts":                         "empty",
	"GET /v1/runs/{run_id}/prompt":                             "hidden",
	"GET /v1/runs/{run_id}/stream":                             "hidden",
	"GET /v1/sessions/{id}/transcript":                         "hidden",
	"GET /v1/users/{user_id}/agents":                           "empty",
	"GET /v1/users/{user_id}/agents/stream":                    "hidden",
	"GET /v1/users/{user_id}/interrupts":                       "empty",
	"PATCH /v1/runs/{run_id}":                                  "hidden",
	"POST /v1/_resident/{run_id}/cancel":                       "hidden",
	"POST /v1/_resident/{run_id}/close":                        "hidden",
	"POST /v1/agents/{agent_id}/cancel":                        "hidden",
	"POST /v1/runs/{run_id}/cancel":                            "hidden",
	"POST /v1/runs/{run_id}/compact":                           "hidden",
	"POST /v1/runs/{run_id}/file":                              "hidden",
	"POST /v1/runs/{run_id}/input":                             "hidden",
	"POST /v1/runs/{run_id}/interrupts/{interrupt_id}/resolve": "hidden",
	"POST /v1/runs/{run_id}/retune":                            "hidden",
	"POST /v1/runs/{run_id}/review":                            "hidden",
	"POST /v1/runs/{run_id}/start":                             "hidden",
	"POST /v1/sessions/{id}/messages":                          "hidden",
	"POST /v1/sessions/{id}/replay":                            "hidden",
	"PUT /v1/runs/{run_id}/breakpoints":                        "hidden",
}

var muxRouteRe = regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+ /v1/[^"]*)"`)

// runAddressingRoute reports whether a mux pattern addresses a run, a session
// or a user's runs: the routes a member's request for another user's
// conversation can reach.
func runAddressingRoute(pattern string) bool {
	_, path, _ := strings.Cut(pattern, " ")
	switch {
	case strings.Contains(path, "{run_id}"),
		strings.HasPrefix(path, "/v1/agents/{agent_id}"),
		strings.HasPrefix(path, "/v1/sessions/"),
		strings.HasPrefix(path, "/v1/users/{user_id}/agents"),
		strings.HasPrefix(path, "/v1/users/{user_id}/interrupts"),
		path == "/v1/runs" && strings.HasPrefix(pattern, "GET "),
		path == "/v1/_events", path == "/v1/_resident":
		return true
	}
	return false
}

// Every route that addresses a run or a session, found from the mux
// registrations, keeps another user's conversation from a member: driven
// through the real mux and auth middleware with a member's token at bob's run,
// none answers with its content, none acts on it, and a listing holds none of
// it.
func TestMemberRunRouteCensus_NoRouteHandsAMemberAnotherUsersRun(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, m := range muxRouteRe.FindAllSubmatch(src, -1) {
		if p := string(m[1]); runAddressingRoute(p) {
			found[p] = true
		}
	}
	for p := range found {
		if _, ok := memberRunRoutes[p]; !ok {
			t.Errorf("%s addresses a run or a session but is not in memberRunRoutes: classify it, and confine a member to its own runs there", p)
		}
	}
	for p := range memberRunRoutes {
		if !found[p] {
			t.Errorf("memberRunRoutes lists %s, which the mux no longer registers", p)
		}
	}

	srv, mux := treeDeleteServer(t)
	st := srv.store
	bobs := seedTreeRun(t, st, "acme", "bob", "lead", "", true)
	interrupt := store.InterruptRow{InterruptID: store.MintInterruptID(time.Now()), RunID: bobs.ID, UserID: "bob",
		Question: "question for bob " + treeCanary, CreatedAt: time.Now()}
	if _, err := st.InterruptCreate(context.Background(), interrupt); err != nil {
		t.Fatal(err)
	}
	expand := strings.NewReplacer(
		"{run_id}", bobs.ID, "{agent_id}", bobs.AgentID, "{id}", bobs.SessionID,
		"{user_id}", "bob", "{interrupt_id}", interrupt.InterruptID, "{path...}", "notes.txt")
	for pattern, class := range memberRunRoutes {
		if !found[pattern] {
			continue
		}
		method, path, _ := strings.Cut(pattern, " ")
		path = expand.Replace(path)
		if path == "/v1/runs" {
			path += "?parent_run_id=" + bobs.ID
		}
		var body io.Reader
		if method != http.MethodGet && method != http.MethodDelete {
			body = strings.NewReader(`{"answer":"yes","verdict":"approve","message":"hi","prompt":"hi"}`)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req := httptest.NewRequest(method, path, body).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+tokMember)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		cancel()
		got := rec.Body.String()
		if strings.Contains(got, treeCanary) {
			t.Errorf("%s: a member read bob's conversation: %d %s", pattern, rec.Code, got)
		}
		switch class {
		case "hidden":
			if rec.Code/100 == 2 {
				t.Errorf("%s: a member was answered %d for bob's run: %s", pattern, rec.Code, got)
			}
		case "empty":
			if rec.Code != http.StatusOK || strings.Contains(got, bobs.ID) || strings.Contains(got, bobs.SessionID) {
				t.Errorf("%s: a member's listing named bob's run: %d %s", pattern, rec.Code, got)
			}
		}
	}
	if !runExists(t, st, bobs.ID) || !sessionExists(t, st, bobs.SessionID) {
		t.Fatal("a member's request removed bob's run")
	}
	if holding, _ := eventsHolding(t, st, treeCanary); holding != 2 {
		t.Errorf("%d of bob's events hold the canary, want both untouched", holding)
	}
	rows, err := st.InterruptListByRun(context.Background(), bobs.ID, "")
	if err != nil || len(rows) != 1 || rows[0].Status != store.InterruptStatusPending {
		t.Errorf("bob's question after a member's requests = %+v, %v; want it pending", rows, err)
	}
}
