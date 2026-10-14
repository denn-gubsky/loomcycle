package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/credential/credentialtest"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// storedSecretFixture is the WebhookDef fixture with a credential engine over
// its store, a context for an author in tenant acme, and put to store a
// tenant-level credential restricted to no URL.
func storedSecretFixture(t *testing.T) (*WebhookDef, context.Context, func(tenant, name string)) {
	t.Helper()
	tool, base, cleanup := webhookDefFixture(t)
	t.Cleanup(cleanup)
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	tool.Credentials = credential.NewEngine(tool.Store, sealer)
	put := func(tenant, name string) {
		t.Helper()
		if _, err := tool.Credentials.PutInline(context.Background(), credential.Identity{TenantID: tenant, Scope: "tenant", Name: name}, "value-"+name, nil, &[]string{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	ctx := tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme"})
	ctx = auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeTenant}})
	return tool, ctx, put
}

func secretOverlay(auth string) string {
	return `{"enabled":true,"delivery":"spawn","agent":"a","auth":` + auth + `}`
}

func acmeWebhook(t *testing.T, tool *WebhookDef, name string) config.Webhook {
	t.Helper()
	w, ok := lookup.Webhook(context.Background(), tool.Store, tool.Cfg, "acme", name)
	if !ok {
		t.Fatalf("webhook acme/%s does not resolve", name)
	}
	return w
}

// A stored secret must be a tenant-level credential of the author's own
// tenant — the tenant the receiver reads it from — so a definition is saved
// only when it can verify a delivery, and never reads another tenant's.
func TestWebhookDefTool_AStoredSecretIsTheAuthorsTenantsCredential(t *testing.T) {
	tool, ctx, put := storedSecretFixture(t)
	put("acme", "hook")
	put("other", "foreign")

	if text, isErr := webhookCreate(t, tool, ctx, "own", secretOverlay(`{"secret":"$cred:hook"}`)); isErr {
		t.Fatalf("the author's own credential: %s", text)
	}
	if w := acmeWebhook(t, tool, "own"); w.Auth.Secret != "$cred:hook" || w.Auth.SigningSecretEnv != "" {
		t.Errorf("stored auth = %+v, want the reference and no env name", w.Auth)
	}
	for _, tc := range []struct{ name, auth, want string }{
		{"another tenant's", `{"secret":"$cred:foreign"}`, `names no tenant-level credential in tenant "acme"`},
		{"a missing previous", `{"secret":"$cred:hook","previous_secret":"$cred:gone"}`, `auth.previous_secret "$cred:gone" names no tenant-level credential`},
	} {
		if text, isErr := webhookCreate(t, tool, ctx, "w", secretOverlay(tc.auth)); !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("%s: %s, want a refusal mentioning %s", tc.name, text, tc.want)
		}
	}

	// An author with no tenant would read the shared tenant's credentials.
	noTenant := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test"})
	noTenant = auth.WithPrincipal(noTenant, auth.Principal{Subject: "dev", Scopes: []string{auth.ScopeTenant}})
	if text, isErr := webhookCreate(t, tool, noTenant, "w", secretOverlay(`{"secret":"$cred:hook"}`)); !isErr || !strings.Contains(text, "no tenant") {
		t.Errorf("an author with no tenant: %s, want it refused", text)
	}
	// An admin is not checked: the credential may be created afterwards.
	admin := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeAdmin}})
	if text, isErr := webhookCreate(t, tool, admin, "later", secretOverlay(`{"secret":"$cred:not-yet"}`)); isErr {
		t.Errorf("an admin naming a credential not yet created: %s", text)
	}
}

// A stored secret that has expired could verify nothing, so a definition
// naming it is refused, and the refusal says it expired rather than that it
// is missing.
func TestWebhookDefTool_AnExpiredStoredSecretIsRefusedAsExpired(t *testing.T) {
	tool, ctx, put := storedSecretFixture(t)
	put("acme", "hook")
	put("acme", "stale")
	credentialtest.Expire(t, tool.Store, credential.Identity{TenantID: "acme", Scope: "tenant", Name: "stale"}, time.Now().Add(-time.Minute))
	for _, auth := range []string{`{"secret":"$cred:stale"}`, `{"secret":"$cred:hook","previous_secret":"$cred:stale"}`} {
		text, isErr := webhookCreate(t, tool, ctx, "w", secretOverlay(auth))
		if !isErr || !strings.Contains(text, `"$cred:stale"`) || !strings.Contains(text, "expired") {
			t.Errorf("%s: %s, want it refused as expired", auth, text)
		}
		if strings.Contains(text, "value-stale") {
			t.Fatalf("the refusal carries the value: %s", text)
		}
	}
}

// Only a reference is taken: a literal would be kept in the definition. A
// refusal never repeats the literal, which is a secret.
func TestWebhookDefTool_AStoredSecretIsAReferenceNeverALiteral(t *testing.T) {
	tool, ctx, put := storedSecretFixture(t)
	put("acme", "hook")
	for _, tc := range []struct{ name, auth, want string }{
		{"a literal", `{"secret":"literal-signing-value"}`, `auth.secret takes only a stored credential`},
		{"a literal previous", `{"secret":"$cred:hook","previous_secret":"literal-signing-value"}`, `auth.previous_secret takes only a stored credential`},
		{"both forms", `{"secret":"$cred:hook","signing_secret_env":"LOOMCYCLE_S"}`, `set one of the two forms`},
		{"previous alone", `{"previous_secret":"$cred:hook"}`, `set auth.secret with it`},
		{"previous beside an env name", `{"signing_secret_env":"LOOMCYCLE_S","previous_secret":"$cred:hook"}`, `set one of the two forms`},
		{"none", `{"kind":"none","secret":"$cred:hook"}`, `auth.kind=none forbids`},
	} {
		text, isErr := webhookCreate(t, tool, ctx, "w", secretOverlay(tc.auth))
		if !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("%s: %s, want a refusal mentioning %s", tc.name, text, tc.want)
		}
		if strings.Contains(text, "literal-signing-value") {
			t.Errorf("%s: the refusal repeats the literal: %s", tc.name, text)
		}
	}
}

// A fork moves between the two forms by naming the other one, and a new
// secret drops the previous one unless the fork names it too.
func TestWebhookDefTool_AForkMovesBetweenTheFormsOfASecret(t *testing.T) {
	tool, ctx, put := storedSecretFixture(t)
	put("acme", "hook")
	put("acme", "hook-old")
	put("acme", "hook-new")
	if text, isErr := webhookCreate(t, tool, ctx, "w", secretOverlay(`{"signing_secret_env":"LOOMCYCLE_S"}`)); isErr {
		t.Fatalf("create: %s", text)
	}
	fork := func(auth string) {
		t.Helper()
		res, err := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"w","overlay":{"auth":`+auth+`}}`))
		if err != nil || res.IsError {
			t.Fatalf("fork %s: %v %s", auth, err, res.Text)
		}
	}
	steps := []struct {
		auth string
		want config.WebhookAuth
	}{
		{`{"secret":"$cred:hook-old"}`, config.WebhookAuth{Secret: "$cred:hook-old"}},
		{`{"secret":"$cred:hook","previous_secret":"$cred:hook-old"}`, config.WebhookAuth{Secret: "$cred:hook", PreviousSecret: "$cred:hook-old"}},
		{`{"header":"X-Hub-Signature-256"}`, config.WebhookAuth{Header: "X-Hub-Signature-256", Secret: "$cred:hook", PreviousSecret: "$cred:hook-old"}},
		{`{"secret":"$cred:hook-new"}`, config.WebhookAuth{Header: "X-Hub-Signature-256", Secret: "$cred:hook-new"}},
		{`{"signing_secret_env":"LOOMCYCLE_S"}`, config.WebhookAuth{Header: "X-Hub-Signature-256", SigningSecretEnv: "LOOMCYCLE_S"}},
	}
	for _, step := range steps {
		fork(step.auth)
		if got := acmeWebhook(t, tool, "w").Auth; got != step.want {
			t.Errorf("after a fork with %s: auth = %+v, want %+v", step.auth, got, step.want)
		}
	}
}

// An env-form secret may not name one of loomcycle's own secrets, whoever
// writes it. A stored definition that already names one is still restored
// (a restore must accept what it accepted); the receiver refuses to read it.
func TestWebhookDefTool_AnEnvSecretCannotBeLoomcyclesOwn(t *testing.T) {
	tool, ctx, _ := storedSecretFixture(t)
	admin := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeAdmin}})
	for _, who := range []context.Context{ctx, admin} {
		for _, overlay := range []string{
			secretOverlay(`{"signing_secret_env":"LOOMCYCLE_AUTH_TOKEN"}`),
			secretOverlay(`{"kind":"bearer","bearer_token_env":"LOOMCYCLE_SECRET_KEY"}`),
		} {
			if text, isErr := webhookCreate(t, tool, who, "w", overlay); !isErr || !strings.Contains(text, "one of loomcycle's own secrets") {
				t.Errorf("%s: %s, want it refused", overlay, text)
			}
		}
	}
	stored := `{"enabled":true,"delivery":"spawn","agent":"a","auth":{"signing_secret_env":"LOOMCYCLE_AUTH_TOKEN"}}`
	if err := ValidateWebhookDefBody(json.RawMessage(stored)); err != nil {
		t.Errorf("restoring a stored definition that names it: %v, want it accepted", err)
	}
}
