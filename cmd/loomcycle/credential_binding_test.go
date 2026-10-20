package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/credential/credentialtest"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// bindingWith stores credentials in tenant "t" (name → expires_at; nil for
// none, value "<name>-value") and returns the binding as main wires it, plus
// the values it registered for masking.
func bindingWith(t *testing.T, creds map[string]*time.Time) (credentialBinding, *[]string) {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	engine := credential.NewEngine(st, sealer)
	for name, exp := range creds {
		id := credential.Identity{TenantID: "t", Scope: "tenant", Name: name}
		if _, err := engine.PutInline(context.Background(), id, name+"-value", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if exp != nil {
			credentialtest.Expire(t, st, id, *exp)
		}
	}
	var registered []string
	return credentialBinding{engine: engine, register: func(v string) { registered = append(registered, v) }}, &registered
}

func runCtx() context.Context {
	return tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t", UserID: "u"})
}

// The provider-key override as a driver consumes it: an expired tenant key
// fails the call with an error naming it, and the operator's key is not
// used, whether or not the run may use operator keys. A missing override
// still gets the operator key; a live one is used and masked.
func TestCredentialBinding_ExpiredProviderKeyFailsInsteadOfTheOperatorKey(t *testing.T) {
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	b, registered := bindingWith(t, map[string]*time.Time{"ANTHROPIC_API_KEY": &past, "OPENAI_API_KEY": &future})

	for _, allowed := range []bool{true, false} {
		ctx := providers.WithOperatorKeyAllowed(providers.WithCredentialResolver(runCtx(), b.providerKey), allowed)
		key, source, _, err := providers.ResolveKeyOrOperator(ctx, "ANTHROPIC_API_KEY", "sk-operator")
		if !errors.Is(err, providers.ErrCredentialUnusable) || !errors.Is(err, credential.ErrExpired) {
			t.Fatalf("operator key allowed=%v: err = %v, want ErrCredentialUnusable wrapping ErrExpired", allowed, err)
		}
		if key != "" || source != "" {
			t.Errorf("operator key allowed=%v: key=%q source=%q; want no key at all", allowed, key, source)
		}
		if !strings.Contains(err.Error(), `"ANTHROPIC_API_KEY"`) || !strings.Contains(err.Error(), "expired") {
			t.Errorf("error %q should name the credential and say it expired", err)
		}
		if strings.Contains(err.Error(), "ANTHROPIC_API_KEY-value") {
			t.Error("the error carries the credential's value")
		}
	}

	ctx := providers.WithCredentialResolver(runCtx(), b.providerKey)
	if key, source, _, err := providers.ResolveKeyOrOperator(ctx, "GEMINI_API_KEY", "sk-operator"); err != nil || key != "sk-operator" || source != "operator" {
		t.Errorf("no override: key=%q source=%q err=%v; want the operator key, unchanged", key, source, err)
	}
	if key, source, _, err := providers.ResolveKeyOrOperator(ctx, "OPENAI_API_KEY", "sk-operator"); err != nil || key != "OPENAI_API_KEY-value" || source != "tenant" {
		t.Errorf("a live override: key=%q source=%q err=%v; want the tenant's key", key, source, err)
	}
	if len(*registered) != 1 || (*registered)[0] != "OPENAI_API_KEY-value" {
		t.Errorf("registered %d value(s); want only the live override masked", len(*registered))
	}
}

// The Bash / Bashbox injector reports an expired credential as an error (so
// the tool unsets the variable, host var included), a missing one as a miss
// (so the host var stays), and a live one as its value.
func TestCredentialBinding_EnvVarSeparatesExpiredFromMissing(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	b, _ := bindingWith(t, map[string]*time.Time{"GITHUB_TOKEN": &past, "GITLAB_TOKEN": nil})
	resolve := b.envVar("bash")

	if v, ok, err := resolve(runCtx(), "GITHUB_TOKEN"); !errors.Is(err, credential.ErrExpired) || ok || v != "" {
		t.Errorf("expired: (%q, %v, %v), want an ErrExpired error and no value", v, ok, err)
	}
	if v, ok, err := resolve(runCtx(), "NPM_TOKEN"); err != nil || ok || v != "" {
		t.Errorf("missing: (%q, %v, %v), want a plain miss", v, ok, err)
	}
	if v, ok, err := resolve(runCtx(), "GITLAB_TOKEN"); err != nil || !ok || v != "GITLAB_TOKEN-value" {
		t.Errorf("live: ok=%v err=%v, want its value", ok, err)
	}
}

// RFC AX Layer 1: a provider whose tenant key expired is not keyable, so a
// restricted run is not routed to it (it may not use the operator key there).
func TestCredentialBinding_AnExpiredKeyIsNotKeyable(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	b, _ := bindingWith(t, map[string]*time.Time{"ANTHROPIC_API_KEY": &past, "OPENAI_API_KEY": nil})
	if b.keyable(context.Background(), "t", "", "u", "ANTHROPIC_API_KEY") {
		t.Error("an expired tenant key was reported keyable")
	}
	if !b.keyable(context.Background(), "t", "", "u", "OPENAI_API_KEY") {
		t.Error("a live tenant key was reported not keyable")
	}
}

// A key the run holds but has restricted by allowed_urls is held, not
// missing: the provider call fails instead of taking the operator's key, the
// Bash / Bashbox variable is reported unusable instead of letting the host
// variable through, and a restricted run is not routed on it.
func TestCredentialBinding_ARestrictedKeyIsNeverReplaced(t *testing.T) {
	b, registered := bindingWith(t, nil)
	scoped := &[]string{"https://api.example.com"}
	for _, name := range []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN"} {
		if _, err := b.engine.PutInline(context.Background(), credential.Identity{TenantID: "t", Scope: "tenant", Name: name}, name+"-value", nil, scoped, nil); err != nil {
			t.Fatal(err)
		}
	}

	for _, allowed := range []bool{true, false} {
		ctx := providers.WithOperatorKeyAllowed(providers.WithCredentialResolver(runCtx(), b.providerKey), allowed)
		key, source, _, err := providers.ResolveKeyOrOperator(ctx, "ANTHROPIC_API_KEY", "sk-operator")
		if !errors.Is(err, providers.ErrCredentialUnusable) || !errors.Is(err, credential.ErrRestricted) {
			t.Errorf("operator key allowed=%v: err = %v, want ErrCredentialUnusable wrapping ErrRestricted", allowed, err)
		}
		if key != "" || source != "" {
			t.Errorf("operator key allowed=%v: key=%q source=%q; want no key at all", allowed, key, source)
		}
	}

	if v, ok, err := b.envVar("bash")(runCtx(), "GITHUB_TOKEN"); !errors.Is(err, credential.ErrRestricted) || ok || v != "" {
		t.Errorf("env var: (%q, %v, %v), want a restricted error and no value", v, ok, err)
	}
	if b.keyable(context.Background(), "t", "", "u", "ANTHROPIC_API_KEY") {
		t.Error("a restricted tenant key was reported keyable")
	}
	if len(*registered) != 0 {
		t.Errorf("registered %d value(s) for a key that was not used", len(*registered))
	}
}

// A run a trigger started without the right to act as its user does not
// take that user's own provider key or host credential: the tenant's is used,
// or none. Any other run takes the user's, as before.
func TestCredentialBinding_ATriggerRunNeverTakesTheUsersOwnKey(t *testing.T) {
	b, _ := bindingWith(t, nil)
	for scope, id := range map[string]string{"tenant": "", "user": "bob"} {
		if _, err := b.engine.PutInline(context.Background(), credential.Identity{TenantID: "t", Scope: scope, ScopeID: id, Name: "ANTHROPIC_API_KEY"}, scope+"-key", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.engine.PutInline(context.Background(), credential.Identity{TenantID: "t", Scope: "user", ScopeID: "bob", Name: "GITHUB_TOKEN"}, "bobs-token", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	for noUser, want := range map[bool]string{false: "user-key", true: "tenant-key"} {
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t", UserID: "bob", CredentialsNoUserScope: noUser})
		res, found, err := b.providerKey(ctx, "ANTHROPIC_API_KEY")
		if err != nil || !found || res.Value != want {
			t.Errorf("CredentialsNoUserScope=%t: provider key %q found=%t err=%v, want %q", noUser, res.Value, found, err, want)
		}
		_, gotEnv, _ := b.envVar("bashbox")(ctx, "GITHUB_TOKEN")
		if gotEnv == noUser {
			t.Errorf("CredentialsNoUserScope=%t: bob's own GITHUB_TOKEN injected = %t", noUser, gotEnv)
		}
		if keyable := b.keyable(ctx, "t", "", "bob", "GITHUB_TOKEN"); keyable == noUser {
			t.Errorf("CredentialsNoUserScope=%t: keyable on bob's own key = %t", noUser, keyable)
		}
	}
}

// A credential injected into a Bash child, or into a Bashbox host command,
// is registered with the redactor as it is resolved, so a command that prints
// its environment does not put the value into the stored transcript (the
// server masks tool results with this redactor before persisting them). The
// live result still carries it: the model can read any credential it is
// given, and the redactor only keeps it out of what is stored.
func TestBash_ResolvedCredValueRedactedFromOutput(t *testing.T) {
	if _, err := exec.LookPath("env"); err != nil {
		t.Skip("host env not available")
	}
	const value = "GITHUB_TOKEN-value"
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithVolumePolicy(runCtx(), tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "default", Root: root, Default: true}}})
	cmd, _ := json.Marshal(map[string]any{"command": "env"})

	for _, tc := range []struct {
		name string
		tool func(b credentialBinding) tools.Tool
	}{
		{"Bash", func(b credentialBinding) tools.Tool {
			return &builtin.Bash{Enabled: true, AllowedCreds: []string{"GITHUB_TOKEN"}, CredResolve: b.envVar("bash")}
		}},
		{"BashboxFallback", func(b credentialBinding) tools.Tool {
			return &builtin.Bashbox{Enabled: true, FallbackCommands: []string{"env"}, FallbackAllowedCreds: []string{"env:GITHUB_TOKEN"}, CredResolve: b.envVar("bashbox")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := bindingWith(t, map[string]*time.Time{"GITHUB_TOKEN": nil})
			rd := redact.New(nil, true)
			b.register = rd.Register
			res, err := tc.tool(b).Execute(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(res.Text, "GITHUB_TOKEN="+value) {
				t.Fatalf("the credential was not injected, so nothing is tested: %q", res.Text)
			}
			// The value is not secret-shaped: the patterns alone leave it, so
			// masking below can only come from the registration.
			if !strings.Contains(redact.New(nil, true).String(res.Text), value) {
				t.Fatal("a pattern masks the test value; the registration is not what is tested")
			}
			if stored := rd.String(res.Text); strings.Contains(stored, value) {
				t.Errorf("the resolved value survives redaction: %q", stored)
			}
		})
	}
}
