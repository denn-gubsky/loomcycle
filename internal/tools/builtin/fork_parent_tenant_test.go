package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A fork's parent_def_id is a caller-chosen global handle. Every def kind with
// a fork op must refuse another tenant's private def exactly as it refuses an
// id that does not exist, so a fork can neither confirm the id exists nor
// reveal the name it was created under.

type forkKind struct {
	kind string
	// fixture returns the tool and a ctx carrying the policy its ops need.
	fixture func(t *testing.T) (tools.Tool, context.Context, func())
	// create is a valid create overlay; fork a valid fork overlay.
	create, fork string
}

func forkKinds() []forkKind {
	return []forkKind{
		{"AgentDef", func(t *testing.T) (tools.Tool, context.Context, func()) { return agentDefFixture(t) },
			`{"system_prompt":"p"}`, `{"system_prompt":"forked"}`},
		{"SkillDef", func(t *testing.T) (tools.Tool, context.Context, func()) { return skillDefFixture(t) },
			`{"body":"b"}`, `{"body":"forked"}`},
		{"TeamDef", func(t *testing.T) (tools.Tool, context.Context, func()) { return teamDefFixture(t) },
			validTeamGraph, `{"colors":{"states":{"review":"#eeeeee"}}}`},
		{"ScheduleDef", func(t *testing.T) (tools.Tool, context.Context, func()) { return scheduleDefFixture(t) },
			`{"agent":"worker","schedule":"0 9 * * 1","user_id":"alice"}`, `{"user_id":"bob"}`},
		{"WebhookDef", func(t *testing.T) (tools.Tool, context.Context, func()) { return webhookDefFixture(t) },
			`{"delivery":"spawn","agent":"intake","auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}}`, `{"agent":"intake2"}`},
		{"MCPServerDef", func(t *testing.T) (tools.Tool, context.Context, func()) { return mcpServerDefFixture(t) },
			`{"transport":"http","url":"https://n8n.example.com/mcp"}`, `{"url":"https://internal.example/mcp"}`},
		{"DocumentSourceDef", func(t *testing.T) (tools.Tool, context.Context, func()) { return documentSourceDefFixture(t) },
			`{"config":{"base_url":"https://a.example.com"}}`, `{"config":{"api_version":"v2"}}`},
		{"MemoryBackendDef", func(t *testing.T) (tools.Tool, context.Context, func()) { return memoryBackendDefFixture(t) },
			`{"kind":"inprocess"}`, `{"config":{"api_version":"v2"}}`},
		{"A2AAgentDef", func(t *testing.T) (tools.Tool, context.Context, func()) { return a2aAgentDefFixture(t) },
			`{"agent_card_url":"https://peer.example/card.json"}`, `{"agent_card_url":"https://peer2.example/card.json"}`},
		{"A2AServerCardDef", func(t *testing.T) (tools.Tool, context.Context, func()) { return a2aServerCardDefFixture(t) },
			`{"name":"card","exposed_agents":[{"agent_name":"writer"}]}`, `{"description":"forked"}`},
	}
}

func asTenant(base context.Context, tenant string) context.Context {
	return tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a_test", TenantID: tenant})
}

func asAdmin(base context.Context) context.Context {
	ctx := asTenant(base, "ops")
	return auth.WithPrincipal(ctx, auth.Principal{TenantID: "ops", Subject: "op", Scopes: []string{auth.ScopeAdmin}})
}

func execDef(t *testing.T, tool tools.Tool, ctx context.Context, input string) tools.Result {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute %s: %v", input, err)
	}
	return res
}

func createDefFor(t *testing.T, tool tools.Tool, ctx context.Context, name, overlay string) string {
	t.Helper()
	res := execDef(t, tool, ctx, `{"op":"create","name":"`+name+`","overlay":`+overlay+`}`)
	if res.IsError {
		t.Fatalf("create %q: %s", name, res.Text)
	}
	return decodeResult(t, res.Text)["def_id"].(string)
}

func forkInput(name, parentDefID, overlay string) string {
	return `{"op":"fork","name":"` + name + `","parent_def_id":"` + parentDefID + `","overlay":` + overlay + `}`
}

// maskID hides the one id a refusal may echo back, so two refusals compare on
// everything else: text, error flag and structured category.
func maskID(res tools.Result, id string) tools.Result {
	res.Text = strings.ReplaceAll(res.Text, id, "<id>")
	return res
}

func TestDefFork_AnotherTenantsParentIsRefusedAsNotFound(t *testing.T) {
	for _, k := range forkKinds() {
		t.Run(k.kind, func(t *testing.T) {
			tool, base, done := k.fixture(t)
			defer done()
			secretID := createDefFor(t, tool, asTenant(base, "globex"), "globex-secret", k.create)

			acme := asTenant(base, "acme")
			const missingID = "def_no_such_parent"

			// Under another name, where the name check would quote globex's
			// def name, and under that same name, where the tenant check
			// refused in words of its own.
			for _, name := range []string{"acme-probe", "globex-secret"} {
				missing := execDef(t, tool, acme, forkInput(name, missingID, k.fork))
				// It must reach the parent lookup, or the comparison below
				// holds between two refusals that never looked at the parent.
				if !missing.IsError || !strings.Contains(missing.Text, "parent_def_id") || !strings.Contains(missing.Text, "not found") {
					t.Fatalf("fork of a nonexistent parent_def_id as %q = %q, want the not-found refusal", name, missing.Text)
				}
				got := execDef(t, tool, acme, forkInput(name, secretID, k.fork))
				if !got.IsError {
					t.Fatalf("acme forked globex's def %s as %q: %s", secretID, name, got.Text)
				}
				if masked, want := maskID(got, secretID), maskID(missing, missingID); !reflect.DeepEqual(masked, want) {
					t.Errorf("fork of globex's def as %q = %+v, want the missing-parent refusal %+v", name, masked, want)
				}
				if name == "acme-probe" && (strings.Contains(got.Text, "globex") || strings.Contains(got.Text, "secret")) {
					t.Errorf("refusal %q reveals another tenant's def", got.Text)
				}
			}
		})
	}
}

func TestDefFork_OwnSharedAndAdminParentsStillFork(t *testing.T) {
	for _, k := range forkKinds() {
		t.Run(k.kind, func(t *testing.T) {
			tool, base, done := k.fixture(t)
			defer done()
			ownID := createDefFor(t, tool, asTenant(base, "acme"), "acme-own", k.create)
			sharedID := createDefFor(t, tool, asTenant(base, ""), "shared-base", k.create)
			globexID := createDefFor(t, tool, asTenant(base, "globex"), "globex-own", k.create)

			for _, tc := range []struct {
				who      string
				ctx      context.Context
				name, id string
			}{
				{"acme its own", asTenant(base, "acme"), "acme-own", ownID},
				{"acme the shared base", asTenant(base, "acme"), "shared-base", sharedID},
				{"an admin another tenant's", asAdmin(base), "globex-own", globexID},
			} {
				if res := execDef(t, tool, tc.ctx, forkInput(tc.name, tc.id, k.fork)); res.IsError {
					t.Errorf("%s forking %s: %s", k.kind, tc.who, res.Text)
				}
			}

			// A visible parent under the wrong name is still refused by name.
			res := execDef(t, tool, asTenant(base, "acme"), forkInput("other-name", ownID, k.fork))
			if !res.IsError || !strings.Contains(res.Text, "has name") {
				t.Errorf("fork of acme's own def under another name = %q, want the name-mismatch refusal", res.Text)
			}
		})
	}
}
