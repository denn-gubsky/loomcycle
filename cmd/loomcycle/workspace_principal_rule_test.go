package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// The release rule's token check (RFC GW D35, P9 plan D-P9-1): a live token
// keeps a workspace; a retired one and one that no longer exists end it; a
// principal declared in config ("cfg:<name>", no token definition) is never
// read as gone.
func TestWorkspacePrincipalRule_TokenEndReleasesAndAConfigPrincipalDoesNot(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "rule.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	mint := func(name string) string {
		t.Helper()
		row, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{DefID: "otd_" + name, Name: name, TenantID: "acme", Subject: "alice", TokenHash: "h-" + name, AllowedScopes: []string{"runs:create"}})
		if err != nil {
			t.Fatal(err)
		}
		return row.DefID
	}
	live, retired := mint("live"), mint("retired")
	if err := st.OperatorTokenDefSetRetiredAt(ctx, retired, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	rule := workspacePrincipalRule(config.WorkspaceConfig{}.Resolved().MCP, st)
	if rule.Idle != time.Hour || rule.MaxLifetime != 24*time.Hour {
		t.Errorf("bounds = %v, %v; want the defaults", rule.Idle, rule.MaxLifetime)
	}
	for id, want := range map[string]bool{live: true, retired: false, "otd_never_minted": false, "cfg:ops": true} {
		got, err := rule.TokenValid(ctx, id)
		if err != nil || got != want {
			t.Errorf("token %q: valid = %v, %v; want %v", id, got, err, want)
		}
	}
}
