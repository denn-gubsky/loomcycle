package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// A webhook's or a schedule's user_bearer is a "$cred:<name>" reference that
// rides into the run unopened. ${run.user_bearer} in a header becomes the
// reference, and the credential engine resolves it for this server's URL:
// a credential restricted by allowed_urls reaches a server its list covers,
// and never one it does not.
func TestRunBearer_ACredentialReferenceResolvesForTheServer(t *testing.T) {
	coveredSeen, otherSeen := &keySeen{}, &keySeen{}
	covered := mcpServer(t, coveredSeen)
	other := mcpServer(t, otherSeen)

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
	allowed := []string{covered.URL}
	list, err := credential.NormalizeAllowedURLs(&allowed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.PutInline(context.Background(), credential.Identity{TenantID: "t", Scope: "tenant", Name: "hook-token"}, "the-bearer-value", nil, list, nil); err != nil {
		t.Fatal(err)
	}
	substitute := func(ctx context.Context, s, targetURL string) (string, []string, error) {
		ri := tools.RunIdentity(ctx)
		return engine.Substitute(ctx, ri.TenantID, tools.AgentName(ctx), ri.UserID, s, targetURL, nil)
	}
	call := func(url string) error {
		t.Helper()
		c, err := New(Config{
			URL:            url,
			Headers:        map[string]string{"X-Api-Key": "${run.user_bearer}"},
			HTTPClient:     covered.Client(),
			CredSubstitute: substitute,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t", AgentID: "a", UserBearer: "$cred:hook-token"})
		if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
			return err
		}
		_, err = mcp.CallTool(ctx, c, "search", json.RawMessage(`{}`))
		return err
	}

	if err := call(covered.URL); err != nil {
		t.Fatalf("a covered server: %v", err)
	}
	if got := coveredSeen.all(); len(got) == 0 || got[0] != "the-bearer-value" {
		t.Errorf("the covered server saw %v, want the resolved value", got)
	}
	if err := call(other.URL); err == nil {
		t.Error("a server outside the list was called")
	}
	for _, v := range otherSeen.all() {
		if v != "" {
			t.Fatalf("a server outside the list saw %q", v)
		}
	}
}

// bearerEngine is a credential engine over a fresh store, and the substitute
// cmd/loomcycle wires from it.
func bearerEngine(t *testing.T) (*credential.Engine, func(context.Context, string, string) (string, []string, error)) {
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
	return engine, func(ctx context.Context, s, targetURL string) (string, []string, error) {
		ri := tools.RunIdentity(ctx)
		return engine.Substitute(ctx, ri.TenantID, tools.AgentName(ctx), ri.UserID, s, targetURL, nil)
	}
}

func putCred(t *testing.T, e *credential.Engine, scope, scopeID, name, value string) {
	t.Helper()
	if _, err := e.PutInline(context.Background(), credential.Identity{TenantID: "t", Scope: scope, ScopeID: scopeID, Name: name}, value, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// The bearer resolves at tenant level only. The run's user and the calling
// agent did not write the definition, and a personal credential of the same
// name at their scope — which wins for any other $cred: — must not be sent
// in its place. ${run.credentials.default}, the bearer's copy, matches.
func TestRunBearer_AUsersOrAgentsCredentialNeverStandsInForIt(t *testing.T) {
	seen := &keySeen{}
	srv := mcpServer(t, seen)
	engine, substitute := bearerEngine(t)
	putCred(t, engine, "tenant", "", "tok", "tenant-value")
	putCred(t, engine, "user", "bob", "tok", "bobs-own-value")
	putCred(t, engine, "agent", "worker", "tok", "agents-own-value")

	for _, header := range []string{"${run.user_bearer}", "${run.credentials.default}"} {
		c, err := New(Config{URL: srv.URL, Headers: map[string]string{"X-Api-Key": header}, HTTPClient: srv.Client(), CredSubstitute: substitute})
		if err != nil {
			t.Fatal(err)
		}
		ctx := tools.WithAgentName(context.Background(), "worker")
		ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "t", AgentID: "a", UserID: "bob", UserBearer: "$cred:tok"})
		if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
			t.Fatalf("%s: %v", header, err)
		}
	}
	for _, v := range seen.all() {
		if v != "tenant-value" {
			t.Fatalf("the server saw %q, want only the tenant's credential (saw %v)", v, seen.all())
		}
	}
	if len(seen.all()) == 0 {
		t.Fatal("the server saw no bearer")
	}
}

// A server whose headers do not send the bearer never resolves it, so a
// bearer restricted to another server does not refuse its calls. A stored
// value that itself spells a credential reference is refused: the header's
// own $cred: pass would resolve it for the run's user.
func TestRunBearer_OnlyAServerThatSendsItResolvesIt(t *testing.T) {
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

	call := func(headers map[string]string, bearer string) error {
		c, err := New(Config{URL: srv.URL, Headers: headers, HTTPClient: srv.Client(), CredSubstitute: substitute})
		if err != nil {
			t.Fatal(err)
		}
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t", AgentID: "a", UserID: "bob", UserBearer: bearer})
		_, err = mcp.Initialize(ctx, c, "t", "1")
		return err
	}
	if err := call(map[string]string{"X-Api-Key": "static"}, "$cred:forge-only"); err != nil {
		t.Errorf("a server that does not send the bearer was refused: %v", err)
	}
	if err := call(map[string]string{"X-Api-Key": "${run.user_bearer}"}, "$cred:forge-only"); err == nil {
		t.Error("a server outside the bearer's list was sent it")
	}
	if err := call(map[string]string{"X-Api-Key": "${run.user_bearer}"}, "$cred:nested"); err == nil {
		t.Error("a stored bearer whose value is a reference was sent")
	}
	for _, v := range seen.all() {
		if v != "static" {
			t.Fatalf("the server saw %q", v)
		}
	}
}
