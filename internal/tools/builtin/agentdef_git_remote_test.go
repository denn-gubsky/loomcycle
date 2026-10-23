package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func lookupGitRemote(t *testing.T, tool *AgentDef, name string) string {
	t.Helper()
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", name)
	if !ok {
		t.Fatalf("%s not found", name)
	}
	return strings.Join(def.GitRemote, ",")
}

// RFC FB: git_remote authored over the substrate survives persist and
// resolve, a fork that does not send it keeps its parent's, and a fork that
// changes only it mints a new version (it is not part of the content hash, so
// the create dedup has to compare it).
func TestAgentDefTool_GitRemoteRoundTrip(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = operatorPlane(ctx)

	for _, tc := range []struct{ name, field, want string }{
		{"git-absent", ``, ""},
		{"git-fetch", `,"git_remote":["fetch"]`, "fetch"},
		{"git-push", `,"git_remote":["fetch","push"]`, "fetch,push"},
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"`+tc.name+`","promote":true,"overlay":{
			"system_prompt":"code","tools":["Git"]`+tc.field+`}}`))
		if res.IsError {
			t.Fatalf("create %s: %s", tc.name, res.Text)
		}
		if got := lookupGitRemote(t, tool, tc.name); got != tc.want {
			t.Errorf("%s: git_remote = %q, want %q", tc.name, got, tc.want)
		}
	}

	// A create whose content matches the active version but whose git_remote
	// differs must mint a new version, not hand back the old one.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"git-fetch","promote":true,"overlay":{
		"system_prompt":"code","tools":["Git"],"git_remote":["fetch","push"]}}`))
	if res.IsError {
		t.Fatalf("re-create changing only git_remote: %s", res.Text)
	}
	if got := lookupGitRemote(t, tool, "git-fetch"); got != "fetch,push" {
		t.Errorf("a create changing only git_remote deduplicated: git_remote = %q, want fetch,push", got)
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"git-push","promote":true,"overlay":{"system_prompt":"code more"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := lookupGitRemote(t, tool, "git-push"); got != "fetch,push" {
		t.Errorf("after a fork that did not send it: git_remote = %q, want fetch,push", got)
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"git-push","promote":true,"overlay":{"git_remote":["fetch"]}}`))
	if res.IsError {
		t.Fatalf("fork changing only git_remote: %s", res.Text)
	}
	if got := lookupGitRemote(t, tool, "git-push"); got != "fetch" {
		t.Errorf("after a fork that changed only it: git_remote = %q, want fetch", got)
	}
}

// A fork of a YAML agent keeps the YAML agent's git_remote. Read from the
// fork's own stored row: lookup would answer from the static agent itself,
// which shadows a shared-tenant fork of the same name.
func TestAgentDefTool_ForkOfAStaticAgentKeepsItsGitRemote(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	tool.Cfg.Agents["releaser"] = config.AgentDef{Model: "m", SystemPrompt: "release", Tools: []string{"Git"}, GitRemote: []string{"fetch", "push"}}

	res, _ := tool.Execute(operatorPlane(ctx), json.RawMessage(`{"op":"fork","name":"releaser","promote":true,"overlay":{"system_prompt":"release carefully"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	row, err := tool.Store.AgentDefGetActive(context.Background(), tools.RunIdentity(ctx).TenantID, "releaser")
	if err != nil {
		t.Fatalf("AgentDefGetActive: %v", err)
	}
	var stored lookup.SubstrateAgentDef
	if err := json.Unmarshal(row.Definition, &stored); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stored.GitRemote, ","); got != "fetch,push" {
		t.Errorf("the stored fork of a static agent has git_remote %q, want fetch,push", got)
	}
}

// The write path refuses an invalid list, even for the operator.
func TestAgentDefTool_RefusesAnInvalidGitRemote(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(operatorPlane(ctx), json.RawMessage(`{"op":"create","name":"coder","overlay":{"git_remote":["push"]}}`))
	wantRefused(t, res, "push without fetch", "git_remote", "push needs fetch")
	res, _ = tool.Execute(operatorPlane(ctx), json.RawMessage(`{"op":"create","name":"coder","overlay":{"git_remote":["clone"]}}`))
	wantRefused(t, res, "an unknown operation", "git_remote[0]", `"clone"`)
}

// The ceiling (RFC FB): an in-run author may store only the Git network
// operations its own run may perform, plus what a forked version already
// had. Unset and [] grant nothing and are within every ceiling.
func TestAgentDefCreate_GitRemoteCeiling(t *testing.T) {
	tool, ctx := ceilingFixture(t) // in-run author with no Git network

	for name, list := range map[string]string{"sdlc/unset": ``, "sdlc/empty": `,"git_remote":[]`} {
		if res := agentDefOp(t, tool, ctx, "create", name, `{"history_scope":["user"]`+list+`}`); res.IsError {
			t.Fatalf("%s: a grant of nothing was refused: %s", name, res.Text)
		}
	}
	wantRefused(t, agentDefOp(t, tool, ctx, "create", "sdlc/f0", `{"history_scope":["user"],"git_remote":["fetch"]}`),
		"fetch by an author that cannot fetch", "git_remote:", `"fetch"`)

	fetcher := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.GitRemote = tools.GitRemotePolicyValue{Fetch: true}
	})
	if res := agentDefOp(t, tool, fetcher, "create", "sdlc/f1", `{"history_scope":["user"],"git_remote":["fetch"]}`); res.IsError {
		t.Fatalf("fetch within the author's own grant refused: %s", res.Text)
	}
	wantRefused(t, agentDefOp(t, tool, fetcher, "create", "sdlc/p1", `{"history_scope":["user"],"git_remote":["fetch","push"]}`),
		"push by an author that cannot push", "git_remote:", `"push"`)

	pusher := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.GitRemote = tools.GitRemotePolicyValue{Fetch: true, Push: true}
	})
	if res := agentDefOp(t, tool, pusher, "create", "sdlc/p2", `{"history_scope":["user"],"git_remote":["fetch","push"]}`); res.IsError {
		t.Fatalf("push within the author's own grant refused: %s", res.Text)
	}

	// An operator mints an agent that may push; a fetch-only author may fork
	// it keeping push (the forked version's), but cannot fork a fetch-only
	// version into a pusher.
	op := operatorPlane(context.Background())
	op = tools.UpdateRunPolicy(op, func(rp *tools.RunPolicy) { rp.AgentDef = tools.AgentDefPolicyValue{Scopes: []string{"any"}} })
	if res, _ := tool.Execute(op, json.RawMessage(`{"op":"create","name":"sdlc/releaser","promote":true,
		"overlay":{"history_scope":["user"],"git_remote":["fetch","push"]}}`)); res.IsError {
		t.Fatalf("operator create: %s", res.Text)
	}
	if res := agentDefOp(t, tool, fetcher, "fork", "sdlc/releaser", `{"system_prompt":"again"}`); res.IsError {
		t.Fatalf("fork keeping the base's push: %s", res.Text)
	}
	wantRefused(t, agentDefOp(t, tool, fetcher, "fork", "sdlc/f1", `{"git_remote":["fetch","push"]}`),
		"fork widening to push", "git_remote:", `"push"`)
}
