package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func putCredential(t *testing.T, e *credential.Engine, tenant, scope, scopeID, name string, allowed *[]string) {
	t.Helper()
	if allowed != nil {
		var err error
		if allowed, err = credential.NormalizeAllowedURLs(allowed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.PutInline(context.Background(), credential.Identity{TenantID: tenant, Scope: scope, ScopeID: scopeID, Name: name}, "value-"+name, nil, allowed, nil); err != nil {
		t.Fatal(err)
	}
}

func bearerOverlay(extra string) string {
	return `{"enabled":true,"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_S"}` + extra + `}`
}

// A webhook's user_bearer is a tenant-level credential the author holds, kept
// as the reference; one restricted by allowed_urls too, since it is resolved
// for each server. An agent's or a user's credential of the name does not
// count: the run resolves the bearer at tenant level only.
func TestWebhookDefTool_UserBearerIsACredentialTheAuthorHolds(t *testing.T) {
	tool, ctx, _ := storedSecretFixture(t)
	e := tool.Credentials
	putCredential(t, e, "acme", "tenant", "", "tok", nil)
	putCredential(t, e, "acme", "agent", "a", "agent-tok", nil)
	putCredential(t, e, "acme", "tenant", "", "pinned", &[]string{"https://forge.example.com/mcp"})
	putCredential(t, e, "other", "tenant", "", "foreign", nil)

	for name, ref := range map[string]string{"t": "$cred:tok", "pin": "$cred:pinned"} {
		if text, isErr := webhookCreate(t, tool, ctx, name, bearerOverlay(`,"user_bearer":"`+ref+`"`)); isErr {
			t.Fatalf("%s: %s", ref, text)
		}
		if got := acmeWebhook(t, tool, name).UserBearer; got != ref {
			t.Errorf("%s: stored user_bearer %q", ref, got)
		}
	}
	for _, tc := range []struct{ name, overlay, want string }{
		{"a literal", bearerOverlay(`,"user_bearer":"ghp_literalTokenValue123"`), "takes only a stored credential"},
		{"a missing credential", bearerOverlay(`,"user_bearer":"$cred:gone"`), `names no tenant-level credential in tenant "acme"`},
		{"the agent's", bearerOverlay(`,"user_bearer":"$cred:agent-tok"`), "names no tenant-level credential"},
		{"another tenant's", bearerOverlay(`,"user_bearer":"$cred:foreign"`), "names no tenant-level credential"},
		{"on a channel", `{"enabled":true,"delivery":"channel","channel":"c","auth":{"signing_secret_env":"LOOMCYCLE_S"},"user_bearer":"$cred:tok"}`, "forbids user_bearer"},
		{"on a team", `{"enabled":true,"delivery":"team","team":"t","auth":{"signing_secret_env":"LOOMCYCLE_S"},"user_bearer":"$cred:tok"}`, "forbids credentials"},
	} {
		if text, isErr := webhookCreate(t, tool, ctx, "w", tc.overlay); !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("%s: %s, want a refusal mentioning %q", tc.name, text, tc.want)
		}
	}

	noTenant := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	noTenant = auth.WithPrincipal(noTenant, auth.Principal{Subject: "dev", Scopes: []string{auth.ScopeTenant}})
	if text, isErr := webhookCreate(t, tool, noTenant, "w", bearerOverlay(`,"user_bearer":"$cred:tok"`)); !isErr || !strings.Contains(text, "no tenant") {
		t.Errorf("an author with no tenant: %s", text)
	}
}

// A fork keeps the parent's bearer, a fork naming one replaces it, and the
// replacement is checked like a new one.
func TestWebhookDefTool_AForkKeepsOrReplacesTheBearer(t *testing.T) {
	tool, ctx, _ := storedSecretFixture(t)
	putCredential(t, tool.Credentials, "acme", "tenant", "", "one", nil)
	putCredential(t, tool.Credentials, "acme", "tenant", "", "two", nil)
	if text, isErr := webhookCreate(t, tool, ctx, "w", bearerOverlay(`,"user_bearer":"$cred:one"`)); isErr {
		t.Fatal(text)
	}
	fork := func(overlay string) (string, bool) {
		res, err := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"w","overlay":`+overlay+`}`))
		if err != nil {
			t.Fatal(err)
		}
		return res.Text, res.IsError
	}
	if text, isErr := fork(`{"rate_limit":{"requests_per_minute":10}}`); isErr {
		t.Fatal(text)
	}
	if got := acmeWebhook(t, tool, "w").UserBearer; got != "$cred:one" {
		t.Errorf("a fork without user_bearer: %q, want the parent's", got)
	}
	if text, isErr := fork(`{"user_bearer":"$cred:two"}`); isErr {
		t.Fatal(text)
	}
	if got := acmeWebhook(t, tool, "w").UserBearer; got != "$cred:two" {
		t.Errorf("a fork naming another: %q", got)
	}
	if text, isErr := fork(`{"user_bearer":"$cred:gone"}`); !isErr || !strings.Contains(text, "names no tenant-level credential") {
		t.Errorf("a fork naming a missing credential: %s", text)
	}
}

// A fork of a static webhook keeps its user_bearer, and is judged on it like
// any bearer it writes: the author must hold the credential.
func TestWebhookDefTool_AForkOfAStaticDefinitionKeepsItsBearer(t *testing.T) {
	tool, ctx, _ := storedSecretFixture(t)
	static := tool.Cfg.Webhooks["gh-push"]
	static.UserBearer = "$cred:static-tok"
	tool.Cfg.Webhooks["gh-push"] = static
	fork := func() (string, bool) {
		res, err := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"gh-push","overlay":{"rate_limit":{"requests_per_minute":10}}}`))
		if err != nil {
			t.Fatal(err)
		}
		return res.Text, res.IsError
	}
	if text, isErr := fork(); !isErr || !strings.Contains(text, "names no tenant-level credential") {
		t.Fatalf("a fork carrying a bearer its author does not hold: %s", text)
	}
	putCredential(t, tool.Credentials, "acme", "tenant", "", "static-tok", nil)
	if text, isErr := fork(); isErr {
		t.Fatal(text)
	}
	if got := acmeWebhook(t, tool, "gh-push").UserBearer; got != "$cred:static-tok" {
		t.Errorf("the fork's user_bearer %q, want the static definition's", got)
	}
}

// scheduleBearerFixture is the ScheduleDef fixture with a credential engine
// and an author in tenant acme.
func scheduleBearerFixture(t *testing.T) (*ScheduleDef, context.Context) {
	t.Helper()
	tool, base, cleanup := scheduleDefFixture(t)
	t.Cleanup(cleanup)
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	tool.Credentials = credential.NewEngine(tool.Store, sealer)
	ctx := tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme"})
	ctx = auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeTenant}})
	return tool, ctx
}

func scheduleCreate(t *testing.T, tool *ScheduleDef, ctx context.Context, name, overlay string) (string, bool) {
	t.Helper()
	res, err := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"`+name+`","overlay":`+overlay+`}`))
	if err != nil {
		t.Fatal(err)
	}
	return res.Text, res.IsError
}

func activeScheduleDef(t *testing.T, tool *ScheduleDef, name string) mergedScheduleDef {
	t.Helper()
	row, err := tool.Store.ScheduleDefGetActive(context.Background(), "acme", name)
	if err != nil {
		t.Fatal(err)
	}
	var d mergedScheduleDef
	if err := json.Unmarshal(row.Definition, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// A schedule's user_bearer follows the same rules. Its user_id's personal
// credential does not count: the author need not be that user, and the run
// resolves the bearer at tenant level only.
func TestScheduleDefTool_UserBearerIsACredentialTheAuthorHolds(t *testing.T) {
	tool, ctx := scheduleBearerFixture(t)
	e := tool.Credentials
	putCredential(t, e, "acme", "tenant", "", "tok", nil)
	putCredential(t, e, "acme", "user", "alice", "alice-tok", nil)
	base := `"agent":"digest","schedule":"0 6 * * *","user_id":"alice"`

	for name, ref := range map[string]string{"t": "$cred:tok"} {
		if text, isErr := scheduleCreate(t, tool, ctx, name, `{`+base+`,"user_bearer":"`+ref+`"}`); isErr {
			t.Fatalf("%s: %s", ref, text)
		}
		if got := activeScheduleDef(t, tool, name).UserBearer; got != ref {
			t.Errorf("%s: stored %q", ref, got)
		}
	}
	for _, tc := range []struct{ name, overlay, want string }{
		{"a literal", `{` + base + `,"user_bearer":"Bearer abc"}`, "takes only a stored credential"},
		{"a missing credential", `{` + base + `,"user_bearer":"$cred:gone"}`, "names no tenant-level credential"},
		{"its user_id's own", `{` + base + `,"user_bearer":"$cred:alice-tok"}`, "names no tenant-level credential"},
		{"on a channel", `{"delivery":"channel","channel":"c","schedule":"0 6 * * *","user_bearer":"$cred:tok"}`, "forbids credentials"},
		{"on a team", `{"delivery":"team","team":"t","schedule":"0 6 * * *","user_bearer":"$cred:tok"}`, "forbids credentials"},
	} {
		if text, isErr := scheduleCreate(t, tool, ctx, "s", tc.overlay); !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("%s: %s, want a refusal mentioning %q", tc.name, text, tc.want)
		}
	}
}
