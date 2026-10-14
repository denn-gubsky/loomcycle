package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
)

// A HookDef's signing_secret is a reference to a credential of the author's
// own tenant that a call to the hook's url can resolve. Anything else is
// refused when the definition is written, before a call could use it.
func TestHookDef_SigningSecretMustNameAnAuthorBoundCredential(t *testing.T) {
	signed := func(url, ref string) string {
		return `{"op":"create","name":"signed","overlay":{"event":"pre","body":{"kind":"http","url":"` + url + `","signing_secret":"` + ref + `"}}}`
	}
	const url = "https://hooks.example/gate"
	cases := []struct {
		name   string
		admin  bool
		tenant string
		input  string
		ok     bool
	}{
		{"own tenant credential", false, "acme", signed(url, "$cred:hook_sign"), true},
		{"literal secret", false, "acme", signed(url, "s3cr3t-literal"), false},
		{"literal secret, admin", true, "acme", signed(url, "s3cr3t-literal"), false},
		{"credential the tenant does not have", false, "acme", signed(url, "$cred:absent"), false},
		{"another tenant's credential", false, "acme", signed(url, "$cred:globex_sign"), false},
		{"author with no tenant", false, "", signed(url, "$cred:hook_sign"), false},
		{"restricted to the hook's url", false, "acme", signed(url, "$cred:scoped_sign"), true},
		{"restricted to other urls", false, "acme", signed(url, "$cred:elsewhere_sign"), false},
		{"admin names one created later", true, "acme", signed(url, "$cred:absent"), true},
		{"on a code body", true, "acme", `{"op":"create","name":"signed","overlay":{"event":"pre","body":{"kind":"code-js","code":"function hook(){}","signing_secret":"$cred:hook_sign"}}}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool, s := hookDefFixture(t)
			tool.CompileCode = func(string) error { return nil }
			tool.Credentials = peerCredsOver(t, s,
				tenantCred("acme", "hook_sign", "v1"),
				tenantCred("globex", "globex_sign", "v2"),
				storedCred{tenant: "acme", scope: "tenant", name: "scoped_sign", value: "v3", allowed: []string{"https://hooks.example"}},
				storedCred{tenant: "acme", scope: "tenant", name: "elsewhere_sign", value: "v4", allowed: []string{"https://other.example"}},
			)
			ctx := hookTenantCtx(c.tenant)
			if c.admin {
				ctx = auth.WithPrincipal(ctx, auth.Principal{TenantID: c.tenant, Subject: "op", Scopes: []string{auth.ScopeAdmin}})
			}
			_, text, isErr := runHookDef(t, tool, ctx, c.input)
			if isErr == c.ok {
				t.Fatalf("ok = %v, want %v: %s", !isErr, c.ok, text)
			}
			for _, v := range []string{"v1", "v2", "v3", "v4"} {
				if isErr && strings.Contains(text, `"`+v+`"`) {
					t.Fatalf("a credential value appears in the refusal: %s", text)
				}
			}
		})
	}
}

// A fork re-checks the merged definition: a signing secret whose credential is
// gone is refused even when the fork does not touch it.
func TestHookDef_ForkRechecksTheInheritedSigningSecret(t *testing.T) {
	tool, s := hookDefFixture(t)
	tool.Credentials = peerCredsOver(t, s, tenantCred("acme", "hook_sign", "v1"))
	ctx := hookTenantCtx("acme")
	mustHookDef(t, tool, ctx, `{"op":"create","name":"signed","overlay":{"event":"pre","body":{"kind":"http","url":"https://hooks.example/gate","signing_secret":"$cred:hook_sign"}}}`)

	tool.Credentials = peerCredsOver(t, s) // a different engine over the same store still has it
	mustHookDef(t, tool, ctx, `{"op":"fork","name":"signed","overlay":{"timeout_ms":900}}`)

	tool.Credentials = nil // no credential store: nothing can be checked, so a non-admin is refused
	if _, text, isErr := runHookDef(t, tool, ctx, `{"op":"fork","name":"signed","overlay":{"timeout_ms":1000}}`); !isErr {
		t.Fatalf("a fork kept a signing secret nobody could check: %s", text)
	}
}

// The stored definition keeps the reference, and its content hash covers it.
func TestHookDef_SigningSecretIsStoredAsTheReference(t *testing.T) {
	tool, s := hookDefFixture(t)
	tool.Credentials = peerCredsOver(t, s, tenantCred("acme", "hook_sign", "the-value"))
	ctx := hookTenantCtx("acme")
	mustHookDef(t, tool, ctx, `{"op":"create","name":"signed","overlay":{"event":"pre","body":{"kind":"http","url":"https://hooks.example/gate","signing_secret":"$cred:hook_sign"}}}`)
	row, err := s.HookDefGetActive(ctx, "acme", "signed")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(row.Definition), "the-value") {
		t.Fatalf("the credential's value was stored: %s", row.Definition)
	}
	var def hooks.Def
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		t.Fatal(err)
	}
	if def.Body.SigningSecret != "$cred:hook_sign" || row.ContentSHA256 != hooks.SignDef("signed", def) {
		t.Fatalf("stored %+v, hash %s", def.Body, row.ContentSHA256)
	}
}
