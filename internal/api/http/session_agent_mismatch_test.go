package http

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
)

func spawnSegs(text string) []loop.PromptSegment {
	return []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: text}}}}
}

// spawn_run (MCP), spawn_runs and runs:batch start through SpawnRun. Naming an
// agent with a session_id continues the session, which runs the session's own
// agent: naming another agent is refused, as on POST /v1/runs, rather than
// silently running the session's agent.
func TestSpawnRun_SessionIDWithAnotherAgentIsRefused(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{})
	sid := g.completedSession("plain")

	res, err := g.srv.SpawnRun(context.Background(), connector.SpawnRunRequest{
		Agent: "other", SessionID: sid, Segments: spawnSegs("WRONG AGENT turn"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || !strings.Contains(res.Error, `"plain"`) || !strings.Contains(res.Error, `"other"`) {
		t.Errorf("spawn with another agent: status %q error %q, want a refusal naming both agents", res.Status, res.Error)
	}
	if _, ran := g.prov.sawCall("WRONG AGENT turn"); ran {
		t.Error("the turn reached the model: the session's agent ran instead of being refused")
	}

	// Naming the session's own agent is a continuation.
	res, err = g.srv.SpawnRun(context.Background(), connector.SpawnRunRequest{
		Agent: "plain", SessionID: sid, Segments: spawnSegs("SAME AGENT turn"),
	})
	if err != nil || res.Status != "completed" {
		t.Fatalf("spawn with the session's own agent: %+v, %v", res, err)
	}
	if msgs, ok := g.prov.sawCall("SAME AGENT turn"); !ok || !strings.Contains(msgs, "first turn") {
		t.Errorf("the continuation did not carry the session's history: %q", msgs)
	}
}

// The refusal tells a token limited to some agents nothing about an agent
// outside its list: it names the session's agent only when the token may run
// it.
func TestSpawnRun_SessionAgentMismatchDoesNotNameAnAgentTheTokenMayNotRun(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{authed: true})
	r := g.postAs(goldenMemberBearer, "/v1/runs", goldenRunBody("other", "first turn", ""))
	_, _, sid := r.ids(t)
	r.drain(t)

	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "dev", TokenDefID: "def_golden_restricted",
		Scopes:       []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
		Restrictions: auth.Restrictions{Agents: []string{"plain"}},
	})
	res, err := g.srv.SpawnRun(ctx, connector.SpawnRunRequest{Agent: "plain", SessionID: sid, Segments: spawnSegs("LIMITED turn")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" || res.Error == "" || strings.Contains(res.Error, `"other"`) {
		t.Errorf("restricted token: status %q error %q, want a refusal that does not name agent \"other\"", res.Status, res.Error)
	}
	if _, ran := g.prov.sawCall("LIMITED turn"); ran {
		t.Error("the restricted token's turn reached the model")
	}
}

// Continuing a session through POST /v1/sessions/{id}/messages with a token
// limited to some agents, when the session's agent is not one of them, is
// refused without naming that agent: the caller named none, and the refusal
// must not tell it one outside its list.
func TestHandleMessages_AContinuationRefusalDoesNotNameAnAgentTheTokenMayNotRun(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{authed: true})
	r := g.postAs(goldenMemberBearer, "/v1/runs", goldenRunBody("other", "first turn", ""))
	_, _, sid := r.ids(t)
	r.drain(t)

	limited := g.postAs(goldenRestrictedBearer, "/v1/sessions/"+sid+"/messages", goldenMsgBody("LIMITED turn", ""))
	limited.drain(t)
	if limited.status != http.StatusForbidden || !strings.Contains(limited.body, "agent_not_allowed") {
		t.Errorf("restricted token: %d %q, want 403 agent_not_allowed", limited.status, limited.body)
	}
	if strings.Contains(limited.body, "other") {
		t.Errorf("restricted token learned the session's agent: %q", limited.body)
	}
	if _, ran := g.prov.sawCall("LIMITED turn"); ran {
		t.Error("the restricted token's turn reached the model")
	}
}

// A token limited to some agents that continues a session whose agent is not
// on its list gets one answer, whatever agent the request names — none, the
// session's own, one on the list, one that is not. Two different answers let
// the caller enumerate names until the answer changes, and so learn the
// session's agent.
func TestContinuation_ARestrictedTokenGetsOneAnswerWhateverAgentItNames(t *testing.T) {
	g := newGoldenServer(t, goldenOpts{authed: true})
	r := g.postAs(goldenMemberBearer, "/v1/runs", goldenRunBody("other", "first turn", ""))
	_, _, sid := r.ids(t)
	r.drain(t)

	type answer struct {
		status int
		body   string
	}
	answers := map[string]answer{}
	msg := g.postAs(goldenRestrictedBearer, "/v1/sessions/"+sid+"/messages", goldenMsgBody("PROBE turn", ""))
	msg.drain(t)
	answers["messages, no agent"] = answer{msg.status, msg.body}
	for _, name := range []string{"other", "plain", "zzz-not-an-agent"} {
		resp := g.postAs(goldenRestrictedBearer, "/v1/runs", goldenRunBody(name, "PROBE turn", `"session_id":"`+sid+`"`))
		resp.drain(t)
		answers["runs, agent "+name] = answer{resp.status, resp.body}
	}
	want := answers["messages, no agent"]
	if want.status != http.StatusForbidden || strings.Contains(want.body, "other") {
		t.Errorf("messages: %d %q, want a 403 that does not name the session's agent", want.status, want.body)
	}
	for k, a := range answers {
		if a != want {
			t.Errorf("%s: %d %q; want the same answer as every other probe: %d %q", k, a.status, a.body, want.status, want.body)
		}
	}

	// The MCP spawn tools (and gRPC Run) reach the same RunOnce.
	restricted := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "dev", TokenDefID: "def_golden_restricted",
		Scopes:       []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
		Restrictions: auth.Restrictions{Agents: []string{"plain"}},
	})
	errs := map[string]string{}
	for _, name := range []string{"", "other", "plain", "zzz-not-an-agent"} {
		res, err := g.srv.SpawnRun(restricted, connector.SpawnRunRequest{Agent: name, SessionID: sid, Segments: spawnSegs("PROBE turn")})
		if err != nil {
			t.Fatal(err)
		}
		errs[name] = res.Status + " " + res.Error
	}
	for name, e := range errs {
		if e != errs[""] {
			t.Errorf("spawn naming %q: %q; want the same as naming none: %q", name, e, errs[""])
		}
	}
	if _, ran := g.prov.sawCall("PROBE turn"); ran {
		t.Error("a probe reached the model")
	}
}
