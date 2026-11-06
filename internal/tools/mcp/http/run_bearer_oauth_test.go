package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// oauthSubstitute is the resolver cmd/loomcycle wires, with a stand-in for
// the OAuth pass: every $oauth:docs becomes a token naming the run's user, so
// a test can see which identity it was resolved for. As there, $oauth: is
// resolved before $cred:, so a stored value that spells one is not expanded
// in the same pass.
func oauthSubstitute(base func(context.Context, string, string) (string, []string, error)) func(context.Context, string, string) (string, []string, error) {
	return func(ctx context.Context, s, targetURL string) (string, []string, error) {
		s = strings.ReplaceAll(s, "$oauth:docs", "oauth-token-of-"+tools.RunIdentity(ctx).UserID)
		return base(ctx, s, targetURL)
	}
}

// A stored bearer whose value spells an $oauth: reference is refused, as one
// spelling $cred: is: the header's own pass would resolve it for the run's
// user, not at tenant level where the bearer belongs.
func TestMCPHTTPClient_RefusesRunBearerHoldingOAuthRef(t *testing.T) {
	seen := &keySeen{}
	srv := mcpServer(t, seen)
	engine, substitute := bearerEngine(t)
	putCred(t, engine, "tenant", "", "nested", "$oauth:docs")
	c, err := New(Config{URL: srv.URL, Headers: map[string]string{"X-Api-Key": "${run.user_bearer}"}, HTTPClient: srv.Client(), CredSubstitute: oauthSubstitute(substitute)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t", AgentID: "a", UserID: "bob", UserBearer: "$cred:nested"})
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err == nil {
		t.Error("a stored bearer whose value is an $oauth: reference was sent")
	}
	if got := seen.all(); len(got) > 0 {
		t.Fatalf("the server saw %v", got)
	}
}

// A run's user_bearer that is itself an $oauth: reference is refused: the
// field takes a $cred: reference or a literal, and a trigger author's
// $oauth: there would otherwise resolve for whichever user the run is for.
func TestMCPHTTPClient_RefusesOAuthRefAsRunBearer(t *testing.T) {
	seen := &keySeen{}
	srv := mcpServer(t, seen)
	_, substitute := bearerEngine(t)
	c, err := New(Config{URL: srv.URL, Headers: map[string]string{"X-Api-Key": "${run.user_bearer}"}, HTTPClient: srv.Client(), CredSubstitute: oauthSubstitute(substitute)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t", AgentID: "a", UserID: "bob", UserBearer: "$oauth:docs"})
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err == nil {
		t.Error("an $oauth: user_bearer was resolved and sent")
	}
	if got := seen.all(); len(got) > 0 {
		t.Fatalf("the server saw %v", got)
	}
}
