package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// An MCP server header naming a stored credential past its expires_at,
// through the real credential engine: the call is refused and the server
// never sees the value — nor the literal reference. A live credential in the
// same header position still goes through.
func TestExpiredCredential_MCPHeaderIsRefusedNotSent(t *testing.T) {
	seen := &keySeen{}
	srv := mcpServer(t, seen)

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
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	for name, exp := range map[string]*time.Time{"stale": &past, "live": &future} {
		if _, err := engine.PutInline(context.Background(), credential.Identity{TenantID: "t", Scope: "tenant", Name: name}, name+"-value", exp, nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	// As cmd/loomcycle wires it.
	substitute := func(ctx context.Context, s, targetURL string) (string, []string, error) {
		ri := tools.RunIdentity(ctx)
		return engine.Substitute(ctx, ri.TenantID, tools.AgentName(ctx), ri.UserID, s, targetURL, nil)
	}
	call := func(ref string) error {
		t.Helper()
		c, err := New(Config{
			URL:            srv.URL,
			Headers:        map[string]string{"X-Api-Key": ref},
			HTTPClient:     srv.Client(),
			CredSubstitute: substitute,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t", AgentID: "a"})
		if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
			return err
		}
		_, err = mcp.CallTool(ctx, c, "search", json.RawMessage(`{}`))
		return err
	}

	err = call("$cred:stale")
	if !errors.Is(err, tools.ErrRunCredentialUnavailable) {
		t.Fatalf("an expired credential: err = %v, want the call refused as unavailable", err)
	}
	if got := seen.all(); len(got) != 0 {
		t.Fatalf("the server received %v for an expired credential; want no request carrying the header", got)
	}

	if err := call("$cred:live"); err != nil {
		t.Fatalf("an unexpired credential: %v", err)
	}
	if got := seen.all(); len(got) == 0 || got[0] != "live-value" {
		t.Errorf("the server saw %v, want the live value", got)
	}
}
