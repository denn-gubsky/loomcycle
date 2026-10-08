package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// hookedTeam runs one agent state and has a webhook of its own, `github`,
// publishing into its own tenant-scoped `events` channel; its `inbox` is
// user-scoped.
const hookedTeam = `{"entry":"review",
  "local":{"channels":{"events":{"scope":"tenant"},"inbox":{"scope":"user"}},
    "webhooks":{"github":{"channel":"./events","auth":{"signing_secret_env":"LOOMCYCLE_GH_SECRET"}}}},
  "states":[{"state":"review","handler":{"kind":"agent","agent":"reviewer"}},
    {"state":"done","handler":{"kind":"terminal"}}],
  "transitions":[{"from":"review","to":"done","on":"success"}]}`

const hookedTeamWebhook = `{"channel":"./events","auth":{"signing_secret_env":"LOOMCYCLE_GH_SECRET"}}`

// withHook is hookedTeam with its one webhook's body replaced.
func withHook(body string) string {
	return strings.Replace(hookedTeam, hookedTeamWebhook, body, 1)
}

// webhookAuthor is ctx granted what the operator planes grant: any webhook
// definition.
func webhookAuthor(ctx context.Context) context.Context {
	return tools.WithWebhookDefPolicy(ctx, tools.WebhookDefPolicyValue{Scopes: []string{"any"}, SelfName: "operator"})
}

// An author who could create a webhook definition may declare a team's own
// webhook; an agent inside a run, which holds no WebhookDef policy, may not —
// at create, or by forking a team that already has one.
func TestTeamDefCreate_LocalWebhookTakesTheAuthorityToCreateAWebhookDef(t *testing.T) {
	tool, inRun, done := teamDefFixture(t)
	defer done()
	wantRefused(t, teamOp(t, tool, inRun, "create", "hooked", hookedTeam), "an in-run author",
		`local.webhooks["github"]`, "authority to create a webhook definition", "default-deny")

	operator := webhookAuthor(inRun)
	if res := teamOp(t, tool, operator, "create", "hooked", hookedTeam); res.IsError {
		t.Fatalf("an operator may declare a team's own webhook: %s", res.Text)
	}
	wantRefused(t, teamOp(t, tool, inRun, "fork", "hooked", `{"colors":null}`), "an in-run fork carrying the webhook",
		`local.webhooks["github"]`, "authority to create a webhook definition")
	narrow := tools.WithWebhookDefPolicy(inRun, tools.WebhookDefPolicyValue{Scopes: []string{"named:other"}})
	wantRefused(t, teamOp(t, tool, narrow, "fork", "hooked", `{"colors":null}`), "a scope naming another webhook",
		"hooked/github")
	if res := teamOp(t, tool, tools.WithWebhookDefPolicy(inRun, tools.WebhookDefPolicyValue{Scopes: []string{"named:hooked/github"}}),
		"fork", "hooked", `{"colors":null}`); res.IsError {
		t.Fatalf("a scope naming the webhook as <team>/<name> covers it: %s", res.Text)
	}
	// Dropping the webhooks needs no such authority.
	if res := teamOp(t, tool, inRun, "fork", "hooked", `{"local":{"webhooks":{}}}`); res.IsError {
		t.Fatalf("a fork that declares no webhook needs no webhook authority: %s", res.Text)
	}
}

func TestTeamDefCreate_ValidatesALocalWebhookAsAWebhookDef(t *testing.T) {
	tool, base, done := teamDefFixture(t)
	defer done()
	ctx := webhookAuthor(base)
	for _, body := range []string{
		hookedTeamWebhook,
		`{"channel":"./events","auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_GH_SECRET","algorithm":"sha256","header":"X-Hub-Signature-256","delivery_id_header":"X-GitHub-Delivery"}}`,
		`{"channel":"./events","auth":{"kind":"bearer","bearer_token_env":"LOOMCYCLE_GL_TOKEN","header":"X-Gitlab-Token"}}`,
		`{"channel":"./events","auth":{"kind":"none"}}`,
		`{"channel":"./events","auth":{"signing_secret_env":"LOOMCYCLE_GH_SECRET"},"payload_mapping":{"user_id":"$.sender.login"}}`,
		`{"channel":"./inbox","auth":{"signing_secret_env":"LOOMCYCLE_GH_SECRET"},"payload_mapping":{"user_id":"$.users[0].id"}}`,
	} {
		if res := teamOp(t, tool, ctx, "create", "hooked", withHook(body)); res.IsError {
			t.Errorf("%s must be accepted: %s", body, res.Text)
		}
	}
	for body, want := range map[string]string{
		`{"channel":"./events"}`: "auth.kind=hmac requires auth.signing_secret_env",
		`{"channel":"./events","auth":{"signing_secret_env":"gh secret"}}`:                              "not a valid env-var name",
		`{"channel":"./events","auth":{"kind":"bearer"}}`:                                               "auth.kind=bearer requires auth.bearer_token_env",
		`{"channel":"./events","auth":{"kind":"none","bearer_token_env":"X"}}`:                          "auth.kind=none forbids",
		`{"channel":"./events","auth":{"kind":"mtls"}}`:                                                 `unknown auth.kind "mtls"`,
		`{"channel":"./events","auth":{"signing_secret_env":"S","algorithm":"sha1"}}`:                   "only sha256",
		`{"channel":"./events","auth":{"signing_secret_env":"S","secret":"x"}}`:                         `unknown field "secret"`,
		`{"channel":"./ghost","auth":{"signing_secret_env":"S"}}`:                                       `"./ghost" names a channel the team does not declare`,
		`{"channel":"events","auth":{"signing_secret_env":"S"}}`:                                        "must name one of the team's own channels",
		`{"channel":"./inbox","auth":{"signing_secret_env":"S"}}`:                                       "scope=user",
		`{"channel":"./events","auth":{"signing_secret_env":"S"},"payload_mapping":{"goal":"$.x"}}`:     `target "goal": only user_id applies`,
		`{"channel":"./events","auth":{"signing_secret_env":"S"},"payload_mapping":{"user_id":"$..x"}}`: "not a supported JSONPath",
		`{"channel":"./events","auth":{"signing_secret_env":"S"},"payload_mapping":{"user_id":""}}`:     "not a supported JSONPath",
		`{"channel":"./events","auth":{"signing_secret_env":"S"},"payload_mapping":"$.x"}`:              "not a webhook definition",
	} {
		wantRefused(t, teamOp(t, tool, ctx, "create", "hooked", withHook(body)), body, `local.webhooks["github"]`, want)
	}
}

// Every field a WebhookDef takes and a team's own webhook does not is refused
// by name, never dropped.
func TestTeamDefCreate_RefusesEachWebhookDefFieldALocalWebhookDoesNotTake(t *testing.T) {
	tool, base, done := teamDefFixture(t)
	defer done()
	ctx := webhookAuthor(base)
	for field, value := range map[string]string{
		"agent": `"reviewer"`, "team": `"other"`, "vars": `{}`, "delivery": `"spawn"`,
		"sync_response": `{"enabled":true,"timeout_ms":100}`, "on_complete": `[]`,
		"user_credentials": `{"K":"v"}`, "user_credentials_from_env": `{"K":"V"}`,
		"user_tier": `"free"`, "tenant_id": `"globex"`, "metadata": `{}`, "enabled": `true`,
		"rate_limit": `{}`, "body_size_limit_bytes": `10`, "credentials": `{}`,
	} {
		body := `{"channel":"./events","auth":{"signing_secret_env":"S"},"` + field + `":` + value + `}`
		wantRefused(t, teamOp(t, tool, ctx, "create", "hooked", withHook(body)), field,
			`local.webhooks["github"]`, `"`+field+`" is not a field of a team's own webhook`)
	}
}

// A fork that sends local.webhooks replaces them and leaves the team's other
// kinds alone; one that does not send them keeps the parent's.
func TestTeamDefFork_ReplacesOnlyTheLocalWebhooks(t *testing.T) {
	tool, base, done := teamDefFixture(t)
	defer done()
	ctx := webhookAuthor(base)
	createTeam(t, tool, ctx, "hooked", hookedTeam)
	webhooksOf := func(text string) map[string]any {
		var out struct {
			Definition struct {
				Local struct {
					Channels map[string]any `json:"channels"`
					Webhooks map[string]any `json:"webhooks"`
				} `json:"local"`
			} `json:"definition"`
		}
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("decode %s: %v", text, err)
		}
		if len(out.Definition.Local.Channels) != 2 {
			t.Errorf("the team's channels changed: %v", out.Definition.Local.Channels)
		}
		return out.Definition.Local.Webhooks
	}
	res := teamOp(t, tool, ctx, "fork", "hooked", `{"colors":null}`)
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := webhooksOf(res.Text); len(got) != 1 || got["github"] == nil {
		t.Errorf("a fork without local.webhooks must keep the parent's, got %v", got)
	}
	res = teamOp(t, tool, ctx, "fork", "hooked", `{"local":{"webhooks":{"gitlab":{"channel":"./events","auth":{"kind":"bearer","bearer_token_env":"LOOMCYCLE_GL"}}}}}`)
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if got := webhooksOf(res.Text); len(got) != 1 || got["gitlab"] == nil {
		t.Errorf("a fork that sends local.webhooks must replace them, got %v", got)
	}
}

func TestValidateTeamDefBody_RefusesALocalWebhookThatCouldNotWork(t *testing.T) {
	if err := ValidateTeamDefBody(json.RawMessage(hookedTeam)); err != nil {
		t.Fatalf("a good body must restore: %v", err)
	}
	for _, body := range []string{
		`{"channel":"./events"}`,
		`{"channel":"./ghost","auth":{"signing_secret_env":"S"}}`,
		`{"channel":"./inbox","auth":{"signing_secret_env":"S"}}`,
		`{"channel":"./events","auth":{"signing_secret_env":"S"},"agent":"reviewer"}`,
	} {
		err := ValidateTeamDefBody(json.RawMessage(withHook(body)))
		if err == nil || !strings.Contains(err.Error(), `local.webhooks["github"]`) {
			t.Errorf("%s: a restored body must be refused, got %v", body, err)
		}
	}
}

// A team that declares webhooks is refused on a server that cannot arm them
// (its walk would wait on an endpoint that never answers), and when run in
// another tenant than the team's.
func TestTeamDefRun_RefusesATeamWhoseWebhooksCannotRun(t *testing.T) {
	tool, base, done := teamDefFixture(t)
	defer done()
	owner := webhookAuthor(tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme"}))
	created := createTeam(t, tool, owner, "hooked", hookedTeam)
	tool.Spawn = textSpawn(func(context.Context, string, teamrun.Prompt, string) (string, error) { return "ok", nil })

	res, _ := tool.Execute(owner, json.RawMessage(`{"op":"run","name":"hooked","input":"x"}`))
	wantRefused(t, res, "no arming wired", "declares webhooks of its own", "cannot run them")

	log := &lifecycleLog{}
	log.wire(tool, nil)
	admin := auth.WithPrincipal(tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a_root", TenantID: "globex"}),
		auth.Principal{TenantID: "globex", Subject: "root", Scopes: []string{auth.ScopeAdmin}})
	res, _ = tool.Execute(admin, json.RawMessage(`{"op":"run","def_id":"`+created["def_id"].(string)+`","input":"x"}`))
	wantRefused(t, res, "another tenant", "declares webhooks of its own and belongs to another tenant")
	if got := log.String(); got != "" {
		t.Errorf("a walk refused for its tenant must cost no run: %s", got)
	}

	// Wired and in its own tenant, the walk arms its triggers and runs.
	res, _ = tool.Execute(owner, json.RawMessage(`{"op":"run","name":"hooked","input":"x"}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	if got := log.String(); got != "open,arm:,disarm,finish" {
		t.Errorf("lifecycle = %s, want the walk armed and disarmed", got)
	}
}

// The model learns a team's own webhooks from the tool's schema: it must parse
// and describe them under local, and the description must name the route.
func TestTeamDefInputSchema_DescribesLocalWebhooks(t *testing.T) {
	var schema struct {
		Properties struct {
			Overlay struct {
				Properties struct {
					Local struct {
						Properties map[string]struct {
							AdditionalProperties struct {
								Properties map[string]any `json:"properties"`
								Required   []string       `json:"required"`
							} `json:"additionalProperties"`
						} `json:"properties"`
					} `json:"local"`
				} `json:"properties"`
			} `json:"overlay"`
		} `json:"properties"`
	}
	if err := json.Unmarshal((&TeamDef{}).InputSchema(), &schema); err != nil {
		t.Fatalf("the TeamDef input schema does not parse: %v", err)
	}
	hooks, ok := schema.Properties.Overlay.Properties.Local.Properties["webhooks"]
	if !ok {
		t.Fatal("local.webhooks is not in the TeamDef input schema")
	}
	for _, field := range []string{"auth", "channel", "payload_mapping"} {
		if _, ok := hooks.AdditionalProperties.Properties[field]; !ok {
			t.Errorf("local.webhooks does not describe %q", field)
		}
	}
	desc := (&TeamDef{}).Description()
	if !strings.Contains(desc, "webhooks of its own") || !strings.Contains(desc, "/v1/_teams/<tenant>/<team>/webhooks/<name>") {
		t.Error("the TeamDef description does not describe a team's own webhooks and their route")
	}
}
