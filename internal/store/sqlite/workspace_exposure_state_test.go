package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// legacyWorkspaceExposuresDDL is workspace_exposures as RFC GW P9 shipped it
// (postgres 0141): one exposure per port of a workspace, by a table key.
const legacyWorkspaceExposuresDDL = `CREATE TABLE workspace_exposures (
	id           TEXT PRIMARY KEY,
	tenant_id    TEXT NOT NULL DEFAULT '',
	user_id      TEXT NOT NULL DEFAULT '',
	workspace_id TEXT NOT NULL,
	port         INTEGER NOT NULL,
	title        TEXT NOT NULL DEFAULT '',
	kind         TEXT NOT NULL DEFAULT 'page',
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL,
	UNIQUE (workspace_id, port)
)`

// TestWorkspaceExposureState_AnUpgradedTableKeepsItsRowsAndFreesAClosedPort —
// on a database written before RFC HI, an old exposure reads back live with
// no lease and the operator's timeout, and the old table key is gone: once
// it closes, a new page may take its port, while two live pages still may
// not share one (the partial index).
func TestWorkspaceExposureState_AnUpgradedTableKeepsItsRowsAndFreesAClosedPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixNano()
	for _, q := range []string{legacyWorkspaceExposuresDDL,
		`CREATE INDEX workspace_exposures_by_workspace ON workspace_exposures (tenant_id, workspace_id)`} {
		if _, err := seed.Exec(q); err != nil {
			t.Fatalf("legacy schema: %v", err)
		}
	}
	if _, err := seed.Exec(`INSERT INTO workspace_exposures (id, tenant_id, user_id, workspace_id, port, created_at, last_used_at)
		VALUES ('ex_old', 'acme', 'alice', 'ws1', 8000, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a legacy DB must migrate it, got: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	old, err := st.WorkspaceExposureGet(ctx, "acme", "ex_old")
	if err != nil || !old.Live() || old.IdleTimeoutS != nil || old.LeaseExpiresAt != nil || old.ClosedReason != "" {
		t.Fatalf("the legacy row after upgrade = %+v, %v; want live, no lease, the operator's timeout", old, err)
	}
	if has, err := hasConstraintKey(ctx, st.db, "workspace_exposures", []string{"workspace_id", "port"}); err != nil || has {
		t.Fatalf("the upgraded table still has its port key = %v, %v", has, err)
	}
	var conflict *store.ErrConflict
	if _, err := st.WorkspaceExposureCreate(ctx, store.WorkspaceExposure{ID: "ex_twin", TenantID: "acme", UserID: "alice",
		WorkspaceID: "ws1", Port: 8000, Kind: store.WorkspaceExposurePage}, 2); !errors.As(err, &conflict) {
		t.Fatalf("a second live page on the port = %v, want a conflict", err)
	}
	if ok, err := st.WorkspaceExposureClose(ctx, "acme", "ex_old", store.WorkspaceExposureClosedUnexposed, time.Now()); !ok || err != nil {
		t.Fatalf("close = %v, %v", ok, err)
	}
	if _, err := st.WorkspaceExposureCreate(ctx, store.WorkspaceExposure{ID: "ex_new", TenantID: "acme", UserID: "alice",
		WorkspaceID: "ws1", Port: 8000, Kind: store.WorkspaceExposurePage}, 2); err != nil {
		t.Fatalf("a new page on a closed page's port: %v", err)
	}
}
