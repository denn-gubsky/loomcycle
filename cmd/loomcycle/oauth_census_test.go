package main

import (
	"context"
	"encoding/base64"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Before anything resolved $oauth:, the resolvers knew only $cred: and
// $ghapp:, so a header naming "$oauth:x" was sent with the reference in it as
// written. Every path must report it unresolved, so the caller drops or
// refuses the header.

func newCensusEngine(t *testing.T, kek string) *credential.Engine {
	t.Helper()
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := credential.NewSealer(kek, "")
	if err != nil {
		t.Fatal(err)
	}
	return credential.NewEngine(st, sealer)
}

func TestLateCredSubstitute_ReportsOAuthRefUnresolved(t *testing.T) {
	var ref func(ctx context.Context, s, targetURL string) (string, []string, error)
	_, unresolved, err := lateCredSubstitute(&ref)(context.Background(), "Bearer $oauth:docs", "https://mcp.example/")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(unresolved, "docs") {
		t.Errorf("unresolved = %v, want the $oauth: reference reported", unresolved)
	}
}

func TestCredSubstituteFastPath_ReportsOAuthRefUnresolved(t *testing.T) {
	// No key configured: nothing can resolve, so the fast path answers.
	sub := newCredSubstitute(newCensusEngine(t, ""), nil, nil)
	_, unresolved, err := sub(context.Background(), "Bearer $oauth:docs", "https://mcp.example/")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(unresolved, "docs") {
		t.Errorf("unresolved = %v, want the $oauth: reference reported", unresolved)
	}
}

func TestCredSubstitute_ReportsOAuthRefUnresolvedWhenEngineResolves(t *testing.T) {
	sub := newCredSubstitute(newCensusEngine(t, base64.StdEncoding.EncodeToString(make([]byte, 32))), nil, nil)
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "t"})
	_, unresolved, err := sub(ctx, "Bearer $oauth:docs", "https://mcp.example/")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(unresolved, "docs") {
		t.Errorf("unresolved = %v, want the $oauth: reference reported", unresolved)
	}
}
