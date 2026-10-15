package http

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// seedLiveRun files a running run of agent in the restricted fixture's tenant
// and user, live in the steer and cancel registries, with one pending question.
// parentRunID makes it a sub-run of that run.
func seedLiveRun(t *testing.T, srv *Server, st store.Store, agent, agentID, parentRunID string) (runID string, steered <-chan steer.Message) {
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
	q, dereg := srv.steerReg.Register(steer.Entry{RunID: run.ID, AgentID: agentID, SessionID: sess.ID, UserID: "dev"})
	t.Cleanup(dereg)
	registerLiveCancel(t, srv, run.ID, agentID, "dev")
	if _, err := st.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: "intr_" + agentID, RunID: run.ID, UserID: "dev", Question: "ok?", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return run.ID, q
}

// A token limited to certain agents controls only runs it could have started.
// A run of another agent in its tenant is the opaque 404 an unknown run gets on
// every control verb, and nothing reaches it; the same tenant's unrestricted
// token still controls it.
func TestRestrictedToken_CannotControlRunsOfOtherAgents(t *testing.T) {
	srv, ts, st := restrictedFixtureServer(t)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	runID, steered := seedLiveRun(t, srv, st, "other", "a_other", "")

	verbs := []struct{ name, method, path, body string }{
		{"steer", "POST", "/v1/runs/" + runID + "/input", `{"text":"hi"}`},
		{"review", "POST", "/v1/runs/" + runID + "/review", `{"decision":"approve"}`},
		{"retune", "POST", "/v1/runs/" + runID + "/retune", `{"max_tokens":100}`},
		{"cancel turn", "POST", "/v1/runs/" + runID + "/cancel", `{}`},
		{"compact", "POST", "/v1/runs/" + runID + "/compact", `{}`},
		{"resolve interrupt", "POST", "/v1/runs/" + runID + "/interrupts/intr_a_other/resolve", `{"answer":"yes"}`},
		{"cancel run", "POST", "/v1/agents/a_other/cancel", `{}`},
	}
	for _, v := range verbs {
		if code, body := bearerCall(t, ts, restrictedBearer, v.method, v.path, v.body); code != 404 {
			t.Errorf("restricted %s: status %d, want the opaque 404: %s", v.name, code, body)
		}
	}
	select {
	case m := <-steered:
		t.Fatalf("a refused call reached the run: %+v", m)
	default:
	}
	if row, err := st.InterruptGet(context.Background(), "intr_a_other"); err != nil || row.Status != store.InterruptStatusPending {
		t.Fatalf("the question after the refused answer: %+v (err %v), want it still pending", row, err)
	}

	// The unrestricted token of the same tenant and user is past the gate on
	// every verb: whatever it is told, it is not the not-found.
	for _, v := range verbs {
		if code, body := bearerCall(t, ts, freeBearer, v.method, v.path, v.body); code == 404 {
			t.Errorf("unrestricted %s: status 404, want it past the gate: %s", v.name, body)
		}
	}
}

// What the allowed agent spawns is part of the run the token started: its
// questions are the token's to answer, as its start was the token's to make.
func TestRestrictedToken_ControlsTheSpawnOfItsAllowedAgent(t *testing.T) {
	srv, ts, st := restrictedFixtureServer(t)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	rootID, _ := seedLiveRun(t, srv, st, "allowed", "a_root", "")
	childID, _ := seedLiveRun(t, srv, st, "other", "a_child", rootID)

	if code, body := bearerCall(t, ts, restrictedBearer, "POST",
		"/v1/runs/"+childID+"/interrupts/intr_a_child/resolve", `{"answer":"yes"}`); code != 200 {
		t.Errorf("answering the allowed run's child: status %d: %s", code, body)
	}
	if code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/runs/"+rootID+"/input", `{"text":"hi"}`); code != 200 {
		t.Errorf("steering the allowed run: status %d: %s", code, body)
	}
}

// The same refusal over MCP: review_run on a held run of another agent is a
// tool error and the verdict does not reach the run, while the tenant's
// unrestricted token delivers its verdict.
func TestMCPReviewRun_RestrictedTokenCannotReviewAnotherAgentsRun(t *testing.T) {
	srv, _, st := restrictedFixtureServer(t)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	runID, verdicts := seedLiveRun(t, srv, st, "other", "a_held_other", "")
	payload, _ := json.Marshal(providers.Event{Type: providers.EventAwaitingReview,
		AwaitingReview: &providers.AwaitingReviewEventInfo{Round: 1}})
	if err := st.AppendEvent(context.Background(), runID, string(providers.EventAwaitingReview), payload); err != nil {
		t.Fatal(err)
	}
	review := func(p auth.Principal) loommcp.CallToolResult {
		t.Helper()
		ctx, cancel := context.WithTimeout(auth.WithPrincipal(context.Background(), p), 5*time.Second)
		defer cancel()
		in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"review_run","arguments":{"agent_id":"a_held_other","decision":"approve"}}}` + "\n"
		var stdout bytes.Buffer
		if err := lcmcp.New(lcmcp.Config{Connector: srv, Logf: func(string, ...any) {}}).Serve(ctx, strings.NewReader(in), &stdout); err != nil {
			t.Fatalf("Serve: %v", err)
		}
		var resp loommcp.Response
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil || resp.Error != nil {
			t.Fatalf("response %q (err %v)", stdout.String(), err)
		}
		var res loommcp.CallToolResult
		if err := json.Unmarshal(resp.Result, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	member := []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}
	if res := review(auth.Principal{TenantID: "acme", Subject: "dev", Scopes: member,
		Restrictions: auth.Restrictions{Agents: []string{"allowed"}}}); !res.IsError {
		t.Errorf("restricted review_run = %+v, want a tool error", res)
	}
	select {
	case m := <-verdicts:
		t.Fatalf("a refused verdict reached the run: %+v", m)
	default:
	}
	if res := review(auth.Principal{TenantID: "acme", Subject: "dev", Scopes: member}); res.IsError {
		t.Errorf("unrestricted review_run = %+v, want the verdict delivered", res)
	}
	if m := <-verdicts; m.Kind != steer.KindApprove {
		t.Errorf("delivered %+v, want an approval", m)
	}
}
