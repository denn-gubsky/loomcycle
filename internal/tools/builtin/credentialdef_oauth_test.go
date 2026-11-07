package builtin

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/credential/oauthtest"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// oauthToolFixture is the CredentialDef tool with an OAuth manager over a
// fake authorization server that polls every second.
func oauthToolFixture(t *testing.T) (*CredentialDef, context.Context, *oauthtest.Server) {
	t.Helper()
	c, ctx := credFixture(t, "alice", testKEK())
	f := oauthtest.New(t)
	// Every grant carries an id_token, so the no-leak checks over
	// f.Secrets() cover it too.
	f.Set(func(f *oauthtest.Server) { f.Interval = 1; f.IDToken = oauthtest.IDTokenSigned })
	c.OAuth = credential.NewOAuthManager(c.Engine, f.Client(), "replica-test", nil)
	bg, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.OAuth.SetBackground(bg)
	return c, ctx, f
}

func oauthCreateBody(f *oauthtest.Server, scope, name string) string {
	return `{"op":"create","usage":"oauth","scope":"` + scope + `","name":"` + name + `","oauth":{"resource":"` + f.Resource() + `"}}`
}

func TestCredentialDefOAuth_ConnectRefusedInsideRun(t *testing.T) {
	c, ctx, f := oauthToolFixture(t)
	if _, res := credExec(t, c, ctx, oauthCreateBody(f, "tenant", "docs")); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	inRun := tools.WithRunID(ctx, "run-1")
	for _, body := range []string{
		`{"op":"oauth_connect_start","scope":"tenant","name":"docs"}`,
		`{"op":"oauth_connect_status","scope":"tenant","name":"docs"}`,
		`{"op":"oauth_disconnect","scope":"tenant","name":"docs"}`,
		`{"op":"delete","scope":"tenant","name":"docs"}`,
		oauthCreateBody(f, "tenant", "other"),
	} {
		if _, res := credExec(t, c, inRun, body); !res.IsError || !strings.Contains(res.Text, "outside a run") {
			t.Errorf("%s inside a run: %q, want refused", body, res.Text)
		}
	}
	if f.Count("device") != 0 {
		t.Error("a connect was started from inside a run")
	}
}

func TestCredentialDefOAuth_TenantConnectRequiresOperator(t *testing.T) {
	c, base, f := oauthToolFixture(t)
	member := auth.WithPrincipal(base, auth.Principal{TenantID: "tnt", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}})
	tenantOp := auth.WithPrincipal(base, auth.Principal{TenantID: "tnt", Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	if _, res := credExec(t, c, member, oauthCreateBody(f, "tenant", "docs")); !res.IsError {
		t.Error("a member created a tenant OAuth credential")
	}
	if _, res := credExec(t, c, tenantOp, oauthCreateBody(f, "tenant", "docs")); res.IsError {
		t.Fatalf("a tenant operator: %s", res.Text)
	}
	if _, res := credExec(t, c, member, `{"op":"oauth_connect_start","scope":"tenant","name":"docs"}`); !res.IsError {
		t.Error("a member connected a tenant OAuth credential")
	}
	if _, res := credExec(t, c, tenantOp, oauthCreateBody(f, "agent", "mine")); !res.IsError {
		t.Error("a tenant operator created an agent OAuth credential")
	}
	if _, res := credExec(t, c, base, oauthCreateBody(f, "agent", "mine")); res.IsError {
		t.Errorf("the operator: %s", res.Text)
	}
}

func TestCredentialDefOAuth_IsolatedUserConnectsOwnUserScopeOnly(t *testing.T) {
	c, base, f := oauthToolFixture(t)
	alice := auth.WithPrincipal(base, auth.Principal{TenantID: "tnt", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}})
	if _, res := credExec(t, c, alice, oauthCreateBody(f, "user", "docs")); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	out, res := credExec(t, c, alice, `{"op":"oauth_connect_start","scope":"user","name":"docs"}`)
	if res.IsError || out["user_code"] == "" {
		t.Fatalf("connect: %s", res.Text)
	}
	// Bob addresses his own user scope: alice's credential is not there.
	bobCtx := tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a", UserID: "bob", TenantID: "tnt"})
	bob := auth.WithPrincipal(bobCtx, auth.Principal{TenantID: "tnt", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate}})
	for _, op := range []string{"oauth_connect_status", "oauth_disconnect", "oauth_connect_start"} {
		if _, res := credExec(t, c, bob, `{"op":"`+op+`","scope":"user","name":"docs"}`); !res.IsError || !strings.Contains(res.Text, "no credential") {
			t.Errorf("bob %s: %q, want not found", op, res.Text)
		}
	}
	// The transport pins an isolated user to user scope.
	if _, refused := credential.ConstrainToUserScope([]byte(`{"op":"oauth_connect_start","scope":"tenant","name":"docs"}`)); !refused {
		t.Error("an isolated user may name tenant scope")
	}
}

func TestCredentialDefOAuth_StatusIsUseVerbOthersManage(t *testing.T) {
	for op, want := range map[string]string{
		"oauth_connect_status": "use", "oauth_connect_start": "manage", "oauth_disconnect": "manage", "get": "use", "create": "manage",
	} {
		if got := credentialACLVerb(op); got != want {
			t.Errorf("%s: verb %q, want %q", op, got, want)
		}
	}
}

func TestCredentialDefOAuth_GetListShowStatusNotSecrets(t *testing.T) {
	c, ctx, f := oauthToolFixture(t)
	if _, res := credExec(t, c, ctx, oauthCreateBody(f, "tenant", "docs")); res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	start, res := credExec(t, c, ctx, `{"op":"oauth_connect_start","scope":"tenant","name":"docs"}`)
	if res.IsError {
		t.Fatalf("connect: %s", res.Text)
	}
	texts := []string{res.Text}
	deadline := time.Now().Add(15 * time.Second)
	for {
		out, res := credExec(t, c, ctx, `{"op":"oauth_connect_status","scope":"tenant","name":"docs"}`)
		texts = append(texts, res.Text)
		if oa, _ := out["oauth"].(map[string]any); oa["state"] == credential.OAuthConnected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not connected: %s", res.Text)
		}
		time.Sleep(100 * time.Millisecond)
	}
	got, res := credExec(t, c, ctx, `{"op":"get","scope":"tenant","name":"docs"}`)
	texts = append(texts, res.Text)
	if got["usage"] != "oauth" {
		t.Errorf("get: %s", res.Text)
	}
	oa, _ := got["oauth"].(map[string]any)
	if oa["state"] != "connected" || oa["resource"] != f.Resource() || oa["access_expires_at"] == nil {
		t.Errorf("get oauth = %v", oa)
	}
	_, res = credExec(t, c, ctx, `{"op":"list","scope":"tenant"}`)
	texts = append(texts, res.Text)
	if !strings.Contains(res.Text, `"state":"connected"`) {
		t.Errorf("list: %s", res.Text)
	}
	if _, ok := start["device_code"]; ok {
		t.Error("the start answer names a device_code field")
	}
	for _, text := range texts {
		for _, s := range f.Secrets() {
			if strings.Contains(text, s) {
				t.Fatalf("a tool result carries a secret (%.8s…): %s", s, text)
			}
		}
	}
	// Delete revokes the grant once, before the row goes.
	if _, res := credExec(t, c, ctx, `{"op":"delete","scope":"tenant","name":"docs"}`); res.IsError {
		t.Fatalf("delete: %s", res.Text)
	}
	var revoked []string
	f.Set(func(f *oauthtest.Server) { revoked = slices.Clone(f.Revoked) })
	if len(revoked) != 1 {
		t.Errorf("revoked %d tokens on delete, want 1", len(revoked))
	}
}
