package http

import (
	"context"
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
