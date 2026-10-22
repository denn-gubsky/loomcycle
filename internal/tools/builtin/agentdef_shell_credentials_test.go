package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// lookupShellCreds is the shell_credentials a stored agent resolves to: nil
// for absent, else the list ([] kept as []).
func lookupShellCreds(t *testing.T, tool *AgentDef, name string) *config.CredentialNames {
	t.Helper()
	def, ok := lookup.Agent(context.Background(), tool.Store, tool.Cfg, "", name)
	if !ok {
		t.Fatalf("%s not found", name)
	}
	return def.ShellCredentials
}

func wantShellCreds(t *testing.T, got *config.CredentialNames, want []string, what string) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s: shell_credentials = %v, want absent", what, *got)
	case want != nil && got == nil:
		t.Errorf("%s: shell_credentials absent, want %v", what, want)
	case want != nil && !reflect.DeepEqual(append([]string{}, (*got)...), want):
		t.Errorf("%s: shell_credentials = %v, want %v", what, *got, want)
	}
}

// RFC EW: shell_credentials authored over the substrate survives persist and
// resolve, `[]` included (it must not come back as "absent", which would hand
// the agent every credential the operator lists), and a fork that does not
// send it keeps its parent's.
func TestAgentDefTool_ShellCredentialsRoundTrip(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	ctx = operatorPlane(ctx)

	for _, tc := range []struct {
		name, field string
		want        []string
	}{
		{"coder-absent", ``, nil},
		{"coder-none", `,"shell_credentials":[]`, []string{}},
		{"coder-one", `,"shell_credentials":["GITHUB_TOKEN"]`, []string{"GITHUB_TOKEN"}},
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"`+tc.name+`","promote":true,"overlay":{
			"system_prompt":"code","tools":["Bash"]`+tc.field+`}}`))
		if res.IsError {
			t.Fatalf("create %s: %s", tc.name, res.Text)
		}
		wantShellCreds(t, lookupShellCreds(t, tool, tc.name), tc.want, tc.name)
	}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"coder-none","promote":true,"overlay":{"system_prompt":"code more"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	wantShellCreds(t, lookupShellCreds(t, tool, "coder-none"), []string{}, "after a fork that did not send it")

	// Changing only the field mints a new version rather than deduplicating
	// against the stored one (it is not part of the content hash).
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"coder-none","promote":true,"overlay":{"shell_credentials":["GITHUB_TOKEN"]}}`))
	if res.IsError {
		t.Fatalf("fork changing only shell_credentials: %s", res.Text)
	}
	wantShellCreds(t, lookupShellCreds(t, tool, "coder-none"), []string{"GITHUB_TOKEN"}, "after a fork that changed only it")
}

// A fork of a YAML agent keeps the YAML agent's narrowing. Without it the
// fork came back holding every credential the operator lists: wider than the
// agent it was forked from.
func TestAgentDefTool_ForkOfAStaticAgentKeepsItsShellCredentials(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	none := config.CredentialNames{}
	tool.Cfg.Agents["reviewer"] = config.AgentDef{Model: "m", SystemPrompt: "review", Tools: []string{"Bash"}, ShellCredentials: &none}

	res, _ := tool.Execute(operatorPlane(ctx), json.RawMessage(`{"op":"fork","name":"reviewer","promote":true,"overlay":{"system_prompt":"review harder"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	// Read the fork's own stored row: lookup would answer from the static
	// agent itself, which shadows a shared-tenant fork of the same name.
	row, err := tool.Store.AgentDefGetActive(context.Background(), tools.RunIdentity(ctx).TenantID, "reviewer")
	if err != nil {
		t.Fatalf("AgentDefGetActive: %v", err)
	}
	var stored lookup.SubstrateAgentDef
	if err := json.Unmarshal(row.Definition, &stored); err != nil {
		t.Fatal(err)
	}
	wantShellCreds(t, stored.ShellCredentials, []string{}, "the stored fork of a static agent with []")
}

// The write path refuses a malformed name, even for the operator.
func TestAgentDefTool_RefusesAShellCredentialThatIsNotAName(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	res, _ := tool.Execute(operatorPlane(ctx), json.RawMessage(`{"op":"create","name":"coder","overlay":{"shell_credentials":["git:GH_TOKEN"]}}`))
	wantRefused(t, res, "a cmd:NAME entry", "shell_credentials[0]")
}

// The ceiling (RFC EW): an in-run author whose own shell is narrowed may store
// only a subset of its names, may not leave the field unset (every
// credential), and may keep what a forked version already allowed. One whose
// shell narrows nothing may store anything.
func TestAgentDefCreate_ShellCredentialsCeiling(t *testing.T) {
	tool, ctx := ceilingFixture(t) // in-run author, shell narrowing nothing

	if res := agentDefOp(t, tool, ctx, "create", "sdlc/any", `{"history_scope":["user"]}`); res.IsError {
		t.Fatalf("an unnarrowed author's unset field was judged: %s", res.Text)
	}
	if res := agentDefOp(t, tool, ctx, "create", "sdlc/some", `{"history_scope":["user"],"shell_credentials":["A","B"]}`); res.IsError {
		t.Fatalf("an unnarrowed author's list was judged: %s", res.Text)
	}

	author := tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.ShellCreds = tools.ShellCredsPolicyValue{Set: true, Names: []string{"A"}}
	})
	wantRefused(t, agentDefOp(t, tool, author, "create", "sdlc/x1", `{"history_scope":["user"]}`),
		"unset under a narrowed author", "shell_credentials:", "left unset", "every credential the operator lists")
	wantRefused(t, agentDefOp(t, tool, author, "create", "sdlc/x2", `{"history_scope":["user"],"shell_credentials":["A","B"]}`),
		"a name the author lacks", "shell_credentials:", `"B"`)
	for name, list := range map[string]string{"sdlc/keeps-a": `["A"]`, "sdlc/keeps-none": `[]`} {
		if res := agentDefOp(t, tool, author, "create", name, `{"history_scope":["user"],"shell_credentials":`+list+`}`); res.IsError {
			t.Fatalf("shell_credentials %s within the author's [A] refused: %s", list, res.Text)
		}
	}

	// An operator mints an agent allowed [B]; the author holding [A] may fork it
	// keeping [B] (and adding its own A), but not adding C.
	op := operatorPlane(context.Background())
	op = tools.UpdateRunPolicy(op, func(rp *tools.RunPolicy) { rp.AgentDef = tools.AgentDefPolicyValue{Scopes: []string{"any"}} })
	if res, _ := tool.Execute(op, json.RawMessage(`{"op":"create","name":"sdlc/releaser","promote":true,
		"overlay":{"history_scope":["user"],"shell_credentials":["B"]}}`)); res.IsError {
		t.Fatalf("operator create: %s", res.Text)
	}
	if res := agentDefOp(t, tool, author, "fork", "sdlc/releaser", `{"shell_credentials":["A","B"]}`); res.IsError {
		t.Fatalf("fork keeping the base's name: %s", res.Text)
	}
	wantRefused(t, agentDefOp(t, tool, author, "fork", "sdlc/releaser", `{"shell_credentials":["B","C"]}`),
		"fork widening", "shell_credentials:", `"C"`)

	// A fork of a version that narrowed nothing may keep that.
	if res := agentDefOp(t, tool, author, "fork", "sdlc/any", `{"history_scope":["user"]}`); res.IsError {
		t.Fatalf("fork of an unnarrowed version keeping it: %s", res.Text)
	}
}
