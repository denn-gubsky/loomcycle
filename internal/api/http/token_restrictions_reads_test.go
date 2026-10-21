package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seededRun is one finished run the read tests file in the restricted
// fixture's tenant and user, in a session of its own.
type seededRun struct {
	agent, agentID, sessionID, runID string
}

// seedFinishedRun files a finished run of agent with a line of transcript, a
// prompt snapshot and a pending question, each naming the agent so a body
// that leaks it is easy to spot. parentRunID makes it a sub-run of that run.
func seedFinishedRun(t *testing.T, st store.Store, agent, agentID, parentRunID string) seededRun {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "acme", agent, "dev")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: agentID, UserID: "dev", TenantID: "acme", ParentRunID: parentRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := json.Marshal(providers.Event{Type: providers.EventText, Text: "said by " + agent})
	snap, _ := json.Marshal(providers.Event{Type: providers.EventPromptSnapshot, PromptSnapshot: &providers.PromptSnapshotInfo{
		System: []providers.ContentBlock{{Type: "text", Text: "system of " + agent}},
	}})
	for _, ev := range []struct {
		typ     string
		payload []byte
	}{{string(providers.EventPromptSnapshot), snap}, {string(providers.EventText), text}} {
		if err := st.AppendEvent(ctx, run.ID, ev.typ, ev.payload); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: "intr_" + agentID, RunID: run.ID, UserID: "dev", Question: "asked by " + agent, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	return seededRun{agent: agent, agentID: agentID, sessionID: sess.ID, runID: run.ID}
}

// The single reads of a session or a run: each returns the run's content or
// metadata, so a token limited to certain agents gets the opaque not-found
// for a run of another agent in its tenant — and its own agent's runs, and
// the tenant's unrestricted token, read as before.
func TestRestrictedToken_CannotReadSessionsOrRunsOfOtherAgents(t *testing.T) {
	_, ts, st := restrictedFixtureServer(t)
	other := seedFinishedRun(t, st, "other", "a_other", "")
	own := seedFinishedRun(t, st, "allowed", "a_own", "")
	// A session the allowed agent's run opened for a sub-agent is part of the
	// run the token started, whatever the sub-agent is called.
	spawned := seedFinishedRun(t, st, "other", "a_spawned", own.runID)
	// And the allowed agent run as a sub-agent of another agent's run is part
	// of THAT run, which the token could not have started.
	underOther := seedFinishedRun(t, st, "allowed", "a_under_other", other.runID)

	reads := func(r seededRun) map[string]string {
		return map[string]string{
			"transcript":     "/v1/sessions/" + r.sessionID + "/transcript",
			"run":            "/v1/runs/" + r.runID,
			"agent":          "/v1/agents/" + r.agentID,
			"prompt":         "/v1/runs/" + r.runID + "/prompt",
			"stream":         "/v1/runs/" + r.runID + "/stream",
			"walk listing":   "/v1/runs?walk_id=" + r.runID,
			"child listing":  "/v1/runs?parent_run_id=" + r.runID,
			"run interrupts": "/v1/runs/" + r.runID + "/interrupts",
		}
	}
	// The refusal must read exactly as a run that does not exist does, ids
	// aside: no 403, and nothing that names the run's agent.
	missing := seededRun{agentID: "a_missing", sessionID: "s_missing", runID: "r_missing"}
	anon := func(body string, r seededRun) string {
		return strings.NewReplacer(r.sessionID, "ID", r.runID, "ID", r.agentID, "ID").Replace(body)
	}
	for _, r := range []seededRun{other, underOther} {
		for name, path := range reads(r) {
			code, body := bearerCall(t, ts, restrictedBearer, "GET", path, "")
			_, missingBody := bearerCall(t, ts, restrictedBearer, "GET", reads(missing)[name], "")
			switch {
			case name == "run interrupts":
				// This listing answers an unreachable run as one with no
				// questions (its existing posture); it must not list them.
				if code != 200 || strings.Contains(body, "intr_") {
					t.Errorf("restricted %s of %s: %d %s, want an empty list", name, r.agentID, code, body)
				}
			case code != 404:
				t.Errorf("restricted %s of %s: status %d, want the opaque 404: %s", name, r.agentID, code, body)
			case anon(body, r) != anon(missingBody, missing):
				t.Errorf("restricted %s of %s: the 404 %q differs from a missing run's %q", name, r.agentID, body, missingBody)
			}
		}
		code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/sessions/"+r.sessionID+"/replay", `{"agent":"allowed"}`)
		if code != 404 {
			t.Errorf("restricted replay of %s's session: status %d, want the opaque 404: %s", r.agentID, code, body)
		}
	}
	for _, c := range []struct {
		bearer string
		r      seededRun
	}{
		{restrictedBearer, own}, {restrictedBearer, spawned},
		{freeBearer, other}, {freeBearer, underOther},
	} {
		for name, path := range reads(c.r) {
			if code, body := bearerCall(t, ts, c.bearer, "GET", path, ""); code != 200 {
				t.Errorf("%s %s of %s: status %d, want 200: %s", c.bearer, name, c.r.agentID, code, body)
			}
		}
	}
	// The tenant's unrestricted token can still replay another agent's session.
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/sessions/"+other.sessionID+"/replay", `{"agent":"allowed"}`); code == 404 {
		t.Errorf("unrestricted replay: 404, want it past the gate: %s", body)
	}
}

// The listings a restricted token may call leave out the runs its allowlist
// does not reach instead of refusing the whole list, and a page of them is
// still filled with the runs it does reach: a user with many runs of other
// agents does not hide the token's own runs behind them.
func TestRestrictedToken_ListingsOmitRunsOfOtherAgents(t *testing.T) {
	srv, ts, st := restrictedFixtureServer(t)
	own := seedFinishedRun(t, st, "allowed", "a_own", "")
	spawned := seedFinishedRun(t, st, "other", "a_spawned", own.runID)
	// More runs of another agent, all newer, than one page of the listing.
	for i := 0; i < userAgentsListMax+20; i++ {
		seedFinishedRun(t, st, "other", fmt.Sprintf("a_other_%d", i), "")
	}

	listed := func(bearer, path string) map[string]bool {
		t.Helper()
		code, body := bearerCall(t, ts, bearer, "GET", path, "")
		if code != 200 {
			t.Fatalf("%s %s: status %d: %s", bearer, path, code, body)
		}
		var resp struct {
			Agents     []agentResponse      `json:"agents"`
			Interrupts []store.InterruptRow `json:"interrupts"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out := map[string]bool{}
		for _, a := range resp.Agents {
			out[a.RunID] = true
		}
		for _, i := range resp.Interrupts {
			out[i.RunID] = true
		}
		return out
	}
	want := map[string]bool{own.runID: true, spawned.runID: true}
	for _, path := range []string{"/v1/users/dev/agents?status=all", "/v1/users/dev/interrupts"} {
		if got := listed(restrictedBearer, path); len(got) != len(want) || !got[own.runID] || !got[spawned.runID] {
			t.Errorf("restricted %s lists %d runs (own: %v, spawned: %v), want exactly those two", path, len(got), got[own.runID], got[spawned.runID])
		}
	}
	// The awaited_state listing filters the same way: two runs waiting on a
	// channel, one of each agent.
	waiting := map[string]string{}
	for _, agent := range []string{"allowed", "other"} {
		sess, err := st.CreateSession(context.Background(), "acme", agent, "dev")
		if err != nil {
			t.Fatal(err)
		}
		run, err := st.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: "a_wait_" + agent, UserID: "dev", TenantID: "acme"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AppendEvent(context.Background(), run.ID, "tool_call",
			[]byte(`{"type":"tool_call","tool_use":{"id":"tu_1","name":"Channel","input":{"op":"subscribe","channel":"findings"}}}`)); err != nil {
			t.Fatal(err)
		}
		waiting[agent] = run.ID
	}
	if got := listed(restrictedBearer, "/v1/users/dev/agents?awaited_state=channel"); len(got) != 1 || !got[waiting["allowed"]] {
		t.Errorf("restricted awaited_state listing = %v, want only the allowed agent's waiting run", got)
	}
	if got := listed(freeBearer, "/v1/users/dev/agents?awaited_state=channel"); len(got) != 2 {
		t.Errorf("unrestricted awaited_state listing = %v, want both waiting runs", got)
	}

	// The unrestricted token's listing is the one it always got: the newest page.
	if got := listed(freeBearer, "/v1/users/dev/agents?status=all"); len(got) != userAgentsListMax {
		t.Errorf("unrestricted listing has %d runs, want a full page of %d", len(got), userAgentsListMax)
	}

	// MCP list_runs and get_run are the connector's, and answer alike.
	restricted := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
		Restrictions: auth.Restrictions{Agents: []string{"allowed"}},
	})
	runs, err := srv.ListRuns(restricted, connector.ListRunsFilter{UserID: "dev", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	want[waiting["allowed"]] = true
	if len(runs) != 3 {
		t.Errorf("restricted ListRuns = %d runs, want its own three", len(runs))
	}
	for _, r := range runs {
		if !want[r.RunID] {
			t.Errorf("restricted ListRuns includes %s (%s)", r.RunID, r.AgentID)
			break
		}
	}
}

// The connector's single reads — MCP get_run, the gRPC StreamRun, and the
// replay gRPC and HTTP share — apply the allowlist as the HTTP reads do.
func TestConnectorReads_RestrictedTokenCannotReadAnotherAgentsRun(t *testing.T) {
	srv, _, st := restrictedFixtureServer(t)
	other := seedFinishedRun(t, st, "other", "a_other", "")
	own := seedFinishedRun(t, st, "allowed", "a_own", "")
	restricted := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
		Restrictions: auth.Restrictions{Agents: []string{"allowed"}},
	})
	free := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
	})
	noop := func(providers.Event) error { return nil }

	if _, err := srv.GetRun(restricted, other.agentID); err == nil {
		t.Error("restricted GetRun by agent id read another agent's run")
	}
	if _, err := srv.GetRunByRunID(restricted, other.runID); err == nil {
		t.Error("restricted GetRunByRunID read another agent's run")
	}
	if err := srv.StreamRunEvents(restricted, other.runID, 0, noop); err == nil {
		t.Error("restricted StreamRunEvents replayed another agent's run")
	}
	if _, err := srv.ListWalkRuns(restricted, other.runID, 0, ""); err == nil {
		t.Error("restricted ListWalkRuns listed another agent's walk")
	}
	if _, err := srv.ListChildRuns(restricted, other.runID, 0, ""); err == nil {
		t.Error("restricted ListChildRuns listed another agent's run's children")
	}
	var re *replayErr
	if _, err := srv.ReplaySession(restricted, connector.ReplaySessionRequest{SourceSessionID: other.sessionID, Agent: "allowed"}); err == nil {
		t.Error("restricted ReplaySession copied another agent's session")
	} else if !errors.As(err, &re) || re.status != 404 {
		t.Errorf("restricted ReplaySession = %v, want the opaque 404", err)
	}

	for name, ctx := range map[string]context.Context{"own run": restricted, "unrestricted": free} {
		r := own
		if name == "unrestricted" {
			r = other
		}
		if _, err := srv.GetRun(ctx, r.agentID); err != nil {
			t.Errorf("%s GetRun: %v", name, err)
		}
		if _, err := srv.GetRunByRunID(ctx, r.runID); err != nil {
			t.Errorf("%s GetRunByRunID: %v", name, err)
		}
		if err := srv.StreamRunEvents(ctx, r.runID, 0, noop); err != nil {
			t.Errorf("%s StreamRunEvents: %v", name, err)
		}
	}
}

// The run-state stream passes a restricted token only the transitions of runs
// its allowlist reaches; every other subscriber sees them all.
func TestStreamUserRunStates_RestrictedTokenSeesOnlyReachableRuns(t *testing.T) {
	srv, _, st := restrictedFixtureServer(t)
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)
	other := seedFinishedRun(t, st, "other", "a_other", "")
	own := seedFinishedRun(t, st, "allowed", "a_own", "")
	spawned := seedFinishedRun(t, st, "other", "a_spawned", own.runID)

	stream := func(p auth.Principal) <-chan connector.RunStateEvent {
		ctx, cancel := context.WithCancel(auth.WithPrincipal(t.Context(), p))
		t.Cleanup(cancel)
		events := make(chan connector.RunStateEvent, 16)
		go func() {
			_ = srv.StreamUserRunStates(ctx, connector.StreamUserRunStatesRequest{UserID: "dev", TenantID: "acme", TenantScoped: true},
				func(evt connector.RunStateEvent) error {
					events <- evt
					return nil
				})
		}()
		return events
	}
	member := []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}
	restricted := stream(auth.Principal{TenantID: "acme", Subject: "dev", Scopes: member,
		Restrictions: auth.Restrictions{Agents: []string{"allowed"}}})
	free := stream(auth.Principal{TenantID: "acme", Subject: "dev", Scopes: member})
	for bus.ActiveSubscriberCount() < 2 {
		time.Sleep(2 * time.Millisecond)
	}

	for _, r := range []seededRun{other, own, spawned} {
		parent := ""
		if r == spawned {
			parent = own.runID
		}
		srv.publishRunState(runStateMeta{RunID: r.runID, AgentID: r.agentID, Agent: r.agent, UserID: "dev", TenantID: "acme", ParentRunID: parent}, "running", "", "")
	}
	collect := func(ch <-chan connector.RunStateEvent, n int) map[string]bool {
		got := map[string]bool{}
		for i := 0; i < n; i++ {
			select {
			case evt := <-ch:
				got[evt.RunID] = true
			case <-time.After(3 * time.Second):
				t.Fatalf("stream carried %d of %d events", i, n)
			}
		}
		select {
		case evt := <-ch:
			got[evt.RunID] = true
		case <-time.After(150 * time.Millisecond):
		}
		return got
	}
	if got := collect(restricted, 2); len(got) != 2 || !got[own.runID] || !got[spawned.runID] {
		t.Errorf("restricted stream carried %v, want only its own run and the run it spawned", got)
	}
	if got := collect(free, 3); len(got) != 3 {
		t.Errorf("unrestricted stream carried %v, want all three runs", got)
	}
}

// A listing that leaves out the runs a restricted token may not see must not
// say it did: `truncated` there would tell the token that runs it cannot see
// exist. A restricted listing with nothing visible reads exactly as the
// listing of a user with no runs at all, however many hidden rows it read.
func TestRestrictedToken_ListingDoesNotRevealHiddenRunsThroughTruncated(t *testing.T) {
	srv, ts, st := restrictedFixtureServer(t)
	srv.awaitedScanCap = 5
	for i := 0; i < 7; i++ {
		seedFinishedRun(t, st, "other", fmt.Sprintf("a_hidden_%d", i), "")
		// And running ones, waiting on a channel, for the awaited_state scan.
		sess, err := st.CreateSession(context.Background(), "acme", "other", "dev")
		if err != nil {
			t.Fatal(err)
		}
		run, err := st.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: fmt.Sprintf("a_waiting_%d", i), UserID: "dev", TenantID: "acme"})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AppendEvent(context.Background(), run.ID, "tool_call",
			[]byte(`{"type":"tool_call","tool_use":{"id":"tu_1","name":"Channel","input":{"op":"subscribe","channel":"findings"}}}`)); err != nil {
			t.Fatal(err)
		}
	}
	_, empty := bearerCall(t, ts, restrictedBearer, "GET", "/v1/users/nobody/agents?status=all", "")
	for _, path := range []string{"/v1/users/dev/agents?status=all", "/v1/users/dev/agents?awaited_state=channel"} {
		if code, body := bearerCall(t, ts, restrictedBearer, "GET", path, ""); code != 200 || body != empty {
			t.Errorf("restricted %s = %d %s, want exactly a user with no runs: %s", path, code, body, empty)
		}
	}
}

// Guard: an isolated member's listing of another user reads exactly as the
// listing of a user with no runs, `truncated` included, on both the plain and
// the awaited_state listing — however many runs that user has.
func TestListUserAgents_IsolatedCrossUserReadsAsAUserWithNoRuns(t *testing.T) {
	srv, mux := runReadServer(t)
	srv.awaitedScanCap = 5
	for i := 0; i < 2*userAgentsListMax; i++ {
		addRun(t, srv.store, "acme", "bob", fmt.Sprintf("a_bob_%d", i), "")
	}
	// Also a scoped principal with no tenant: the store reads tenant "" as
	// every tenant, so its scan reads acme's rows and must not say so.
	for name, ctx := range map[string]context.Context{
		"isolated member": principalCtx("acme", "alice", auth.ScopeUser, auth.ScopeRunsRead),
		"no-tenant":       principalCtx("", "op", auth.ScopeRunsRead),
	} {
		list := func(path string) string {
			t.Helper()
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s = %d %s, want a listing", name, path, rec.Code, rec.Body)
			}
			return rec.Body.String()
		}
		for _, q := range []string{"?status=all", "?awaited_state=channel"} {
			if got, empty := list("/v1/users/bob/agents"+q), list("/v1/users/nobody/agents"+q); got != empty {
				t.Errorf("%s listing of another user%s = %s, want exactly a user with no runs: %s", name, q, got, empty)
			}
		}
	}
}
