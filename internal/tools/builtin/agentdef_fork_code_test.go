package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A fork that keeps a version's grants is safe only while what runs under them
// is the code those grants were given to. These tests pin that a fork changing
// a code-js agent's code is held to its author's own grants, as a create is,
// and that a fork leaving the code alone still keeps the lineage's.

const (
	forkOrigCode = "function run() { return {}; }"
	forkNewCode  = "function run() { return {replaced: true}; }"
)

// forkCodeFixture is ceilingFixture plus a code-js provider and a static code
// agent shipping tenant reach on both memory planes and the whole Document
// tool — the shape of a bundled KB agent. The in-run author holds AgentDef
// rights over every name but only Read and agent-scope memory and SQL.
func forkCodeFixture(t *testing.T) (*AgentDef, context.Context) {
	t.Helper()
	tool, ctx := ceilingFixture(t)
	tool.Cfg.Providers = map[string]config.ProviderConfig{config.CodeJSProvider: {}}
	tool.Cfg.Agents["sdlc/taxo"] = config.AgentDef{
		Provider:     config.CodeJSProvider,
		Model:        "sdlc/taxo",
		Code:         forkOrigCode,
		Tools:        []string{"Read", "Document"},
		MemoryScopes: []string{"tenant"},
		SqlScopes:    []string{"tenant"},
	}
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.AgentDef = tools.AgentDefPolicyValue{Scopes: []string{"any"}, SelfName: "sdlc/meta"}
	})
	return tool, ctx
}

func forkPromote(t *testing.T, tool *AgentDef, ctx context.Context, name, overlay string) tools.Result {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"`+name+`","promote":true,"overlay":`+overlay+`}`))
	if err != nil {
		t.Fatalf("fork %q: %v", name, err)
	}
	return res
}

// activeDef is the definition the name resolves to in the author's tenant, or
// ok=false when nothing is active there.
func activeDef(t *testing.T, tool *AgentDef, ctx context.Context, name string) (mergedDef, bool) {
	t.Helper()
	row, err := tool.Store.AgentDefGetActive(ctx, tools.RunIdentity(ctx).TenantID, name)
	if err != nil {
		return mergedDef{}, false
	}
	var d mergedDef
	if err := json.Unmarshal(row.Definition, &d); err != nil {
		t.Fatalf("decode active definition: %v", err)
	}
	return d, true
}

func TestAgentDefFork_ChangedCodeDoesNotInheritBaseGrants(t *testing.T) {
	code := `"code_body":` + mustJSON(forkNewCode)
	refusals := []struct {
		what, overlay, mentions string
	}{
		{"keeping everything", `{` + code + `}`, "Tools cannot widen"},
		{"keeping the Document tool", `{` + code + `,"memory_scopes":["agent"],"sql_scopes":["agent"]}`, "Tools cannot widen"},
		{"keeping tenant memory", `{` + code + `,"tools":["Read"],"sql_scopes":["agent"]}`, "memory_scopes:"},
		{"keeping tenant SQL", `{` + code + `,"tools":["Read"],"memory_scopes":["agent"]}`, "sql_scopes:"},
	}
	for _, r := range refusals {
		t.Run(r.what, func(t *testing.T) {
			tool, ctx := forkCodeFixture(t)
			wantRefused(t, forkPromote(t, tool, ctx, "sdlc/taxo", r.overlay), "changed-code fork "+r.what, r.mentions)
			if d, ok := activeDef(t, tool, ctx, "sdlc/taxo"); ok && d.Code == forkNewCode {
				t.Fatalf("a refused fork's code became active: %+v", d)
			}
		})
	}

	// Within the author's own grants the new code may be stored and promoted:
	// the rule is the author's ceiling, not a ban on changing code.
	tool, ctx := forkCodeFixture(t)
	if res := forkPromote(t, tool, ctx, "sdlc/taxo", `{`+code+`,"tools":["Read"],"memory_scopes":["agent"],"sql_scopes":["agent"]}`); res.IsError {
		t.Fatalf("changed-code fork within the author's grants refused: %s", res.Text)
	}
	d, ok := activeDef(t, tool, ctx, "sdlc/taxo")
	if !ok || d.Code != forkNewCode {
		t.Fatalf("narrowed fork not active: ok=%v def=%+v", ok, d)
	}
	if strings.Join(d.Tools, ",") != "Read" || strings.Join(d.MemoryScopes, ",") != "agent" || strings.Join(d.SqlScopes, ",") != "agent" {
		t.Fatalf("active changed-code fork holds more than its author: tools=%v memory=%v sql=%v", d.Tools, d.MemoryScopes, d.SqlScopes)
	}
}

// A code-js agent turned into a model agent runs the author's prompt, not the
// code its grants were given to: also a change of what runs.
func TestAgentDefFork_LeavingCodeJSDoesNotInheritBaseGrants(t *testing.T) {
	tool, ctx := forkCodeFixture(t)
	wantRefused(t, forkPromote(t, tool, ctx, "sdlc/taxo", `{"provider":"anthropic","code_body":"","system_prompt":"do as told","tools":["Read"],"sql_scopes":["agent"]}`),
		"code-js to model fork", "memory_scopes:")
}

func TestAgentDefFork_SameCodeKeepsBaseGrants(t *testing.T) {
	for _, overlay := range []string{
		`{"sampling":{"temperature":0.2}}`,
		`{"code_body":` + mustJSON(forkOrigCode) + `,"max_iterations":5}`,
	} {
		tool, ctx := forkCodeFixture(t)
		if res := forkPromote(t, tool, ctx, "sdlc/taxo", overlay); res.IsError {
			t.Fatalf("same-code fork %s refused: %s", overlay, res.Text)
		}
		d, ok := activeDef(t, tool, ctx, "sdlc/taxo")
		if !ok {
			t.Fatalf("same-code fork %s not active", overlay)
		}
		if d.Code != forkOrigCode || strings.Join(d.MemoryScopes, ",") != "tenant" || strings.Join(d.SqlScopes, ",") != "tenant" ||
			strings.Join(d.Tools, ",") != "Read,Document" {
			t.Fatalf("same-code fork %s lost the lineage's grants: %+v", overlay, d)
		}
	}
}

// Every capability ceiling that lets a fork keep the forked version's grants
// stops doing so once the fork changes the code: each wider grant an operator
// gave the code agent is refused to an author that does not hold it, and kept
// by a fork that leaves the code alone.
func TestAgentDefFork_ChangedCodeIsHeldToTheAuthorOnEveryCapability(t *testing.T) {
	cases := append(ceilingCases(),
		ceilingCase{field: "statement_methods", policy: func(ctx context.Context) context.Context { return ctx },
			wider: `{"statement_methods":["rule"]}`},
		ceilingCase{field: "git_remote", policy: func(ctx context.Context) context.Context { return ctx },
			wider: `{"git_remote":["fetch"]}`},
	)
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			tool, base := ceilingFixture(t)
			tool.Cfg.Providers = map[string]config.ProviderConfig{config.CodeJSProvider: {}}

			// The operator authors the code agent with the wide grant, in the
			// shared tier the author's fork falls back to.
			var overlay map[string]any
			if err := json.Unmarshal([]byte(c.wider), &overlay); err != nil {
				t.Fatalf("wider overlay: %v", err)
			}
			overlay["provider"] = config.CodeJSProvider
			overlay["model"] = "sdlc/case"
			overlay["code_body"] = forkOrigCode
			opOverlay, _ := json.Marshal(overlay)
			op := operatorPlane(context.Background())
			op = tools.UpdateRunPolicy(op, func(rp *tools.RunPolicy) { rp.AgentDef = tools.AgentDefPolicyValue{Scopes: []string{"any"}} })
			if res := agentDefOp(t, tool, op, "create", "sdlc/case", string(opOverlay)); res.IsError {
				t.Fatalf("operator create: %s", res.Text)
			}

			ctx := c.policy(base)
			refusedAs := c.refusedAs
			if refusedAs == "" {
				refusedAs = c.field + ":"
			}
			wantRefused(t, agentDefOp(t, tool, ctx, "fork", "sdlc/case", `{"code_body":`+mustJSON(forkNewCode)+`}`),
				"changed-code fork keeping "+c.field, refusedAs)
			if res := agentDefOp(t, tool, ctx, "fork", "sdlc/case", `{"sampling":{"temperature":0.2}}`); res.IsError {
				t.Fatalf("same-code fork keeping %s refused: %s", c.field, res.Text)
			}
		})
	}
}
