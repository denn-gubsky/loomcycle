package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// A webhook's or a schedule's user_credentials values were named by the
// definition's author, and the run's user_id is not the author (a webhook can
// take it from the delivery): a "$cred:" value there resolves at the tenant
// level of the run's tenant, as its user_bearer does — never for the run's
// user or the calling agent. A run its user started keeps resolving its own
// values for its full identity.
func TestRunCredentials_ATriggersReferenceResolvesAtTenantLevelOnly(t *testing.T) {
	engine, substitute := bearerEngine(t)
	putCred(t, engine, "tenant", "", "tok", "tenant-value")
	putCred(t, engine, "user", "mallory", "tok", "mallorys-own-value")
	putCred(t, engine, "agent", "worker", "tok", "agents-own-value")

	sees := func(atTenant bool) []string {
		t.Helper()
		seen := &keySeen{}
		srv := mcpServer(t, seen)
		c, err := New(Config{URL: srv.URL, Headers: map[string]string{"X-Api-Key": "${run.credentials.gh}"}, HTTPClient: srv.Client(), CredSubstitute: substitute})
		if err != nil {
			t.Fatal(err)
		}
		ctx := tools.WithAgentName(context.Background(), "worker")
		ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{
			TenantID: "t", AgentID: "a", UserID: "mallory",
			UserCredentials:        map[string]string{"gh": "$cred:tok"},
			CredentialRefsAtTenant: atTenant,
		})
		if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
			t.Fatalf("atTenant=%v: %v", atTenant, err)
		}
		return seen.all()
	}

	got := sees(true)
	if len(got) == 0 {
		t.Fatal("the server saw no credential")
	}
	for _, v := range got {
		if v != "tenant-value" {
			t.Fatalf("a trigger run sent %q, want only the tenant's credential (saw %v)", v, got)
		}
	}
	// Unchanged for a run its user started: the agent's own credential wins
	// (agent > user > tenant), as before.
	if got := sees(false); len(got) == 0 || got[0] != "agents-own-value" {
		t.Errorf("a run its user started sent %v, want its full identity's credential", got)
	}
}

// Only the values this server's headers send are opened: a trigger run's
// credential restricted to another server does not refuse calls to this one.
// One that does not resolve here refuses the call rather than sending the
// reference as written, and a stored value that itself spells a reference is
// refused rather than resolved again for the run's user.
func TestRunCredentials_ATriggersReferenceIsOpenedOnlyWhereSent(t *testing.T) {
	seen := &keySeen{}
	srv := mcpServer(t, seen)
	engine, substitute := bearerEngine(t)
	allowed := []string{"https://forge.example.com/mcp"}
	list, err := credential.NormalizeAllowedURLs(&allowed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.PutInline(context.Background(), credential.Identity{TenantID: "t", Scope: "tenant", Name: "forge-only"}, "forge-value", nil, list, nil); err != nil {
		t.Fatal(err)
	}
	putCred(t, engine, "tenant", "", "nested", "$cred:personal")
	putCred(t, engine, "user", "bob", "personal", "bobs-personal-value")

	call := func(headers, creds map[string]string) error {
		t.Helper()
		c, err := New(Config{URL: srv.URL, Headers: headers, HTTPClient: srv.Client(), CredSubstitute: substitute})
		if err != nil {
			t.Fatal(err)
		}
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
			TenantID: "t", AgentID: "a", UserID: "bob", UserCredentials: creds, CredentialRefsAtTenant: true,
		})
		_, err = mcp.Initialize(ctx, c, "t", "1")
		return err
	}
	if err := call(map[string]string{"X-Api-Key": "static"}, map[string]string{"forge": "$cred:forge-only"}); err != nil {
		t.Errorf("a server that does not send the credential was refused: %v", err)
	}
	if err := call(map[string]string{"X-Api-Key": "${run.credentials.forge}"}, map[string]string{"forge": "$cred:forge-only"}); err == nil {
		t.Error("a server outside the credential's list was sent it")
	}
	if err := call(map[string]string{"X-Api-Key": "${run.credentials.n}"}, map[string]string{"n": "$cred:nested"}); err == nil {
		t.Error("a stored value that is itself a reference was sent")
	}
	for _, v := range seen.all() {
		if v != "static" {
			t.Fatalf("the server saw %q", v)
		}
	}
}
