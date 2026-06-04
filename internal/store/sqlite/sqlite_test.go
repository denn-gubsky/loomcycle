package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/storetest"
)

// newTestStore opens a fresh on-disk SQLite under t.TempDir(). On-disk (vs
// :memory:) so the `cache=shared` modernc semantics don't surprise tests.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestStoreContract runs the shared behavioural test suite against the
// SQLite adapter. The same suite runs against the Postgres adapter in
// internal/store/postgres/postgres_test.go — any contract divergence
// surfaces as a failed sub-test on whichever adapter regressed.
func TestStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) (store.Store, func()) {
		s := newTestStore(t)
		return s, func() { _ = s.Close() }
	})
}

// ---- SQLite-specific tests below this line ----
//
// Tests that verify SQLite-only behaviour (the ALTER-COLUMN idempotency
// guard, NULL columns inspected via direct SQL access to s.db) live here.
// Anything that's true of every Store adapter belongs in
// storetest/contract.go instead.

// TestOpen_ConnectionPragmas is the regression guard for the CI-only
// double-fire flake in internal/api/http TestSchedulerBearerCompound. The
// :memory: DSN branch used to drop busy_timeout + foreign_keys (only the
// on-disk path carried them). With SetMaxOpenConns(8) + cache=shared,
// concurrent writers on the shared cache then got SQLITE_BUSY *immediately*
// (default busy_timeout 0) instead of waiting — under CI load the BUSY'd
// scheduler RecordResult writes failed to advance next_run_at and the
// still-due rows re-fired. Both the in-memory and on-disk DSNs must carry
// busy_timeout=5000 (writers wait) and foreign_keys=1 (parity with prod).
//
// Fails on the pre-fix code: in-memory busy_timeout would read back 0.
func TestOpen_ConnectionPragmas(t *testing.T) {
	for _, path := range []string{":memory:", filepath.Join(t.TempDir(), "pragmas.db")} {
		path := path
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open(%q): %v", path, err)
		}
		t.Cleanup(func() { _ = s.Close() })

		var busy int
		if err := s.db.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatalf("path=%q PRAGMA busy_timeout: %v", path, err)
		}
		if busy != 5000 {
			t.Errorf("path=%q busy_timeout = %d, want 5000 (concurrent writers must wait, not SQLITE_BUSY)", path, busy)
		}

		var fk int
		if err := s.db.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("path=%q PRAGMA foreign_keys: %v", path, err)
		}
		if fk != 1 {
			t.Errorf("path=%q foreign_keys = %d, want 1 (FK enforcement parity with prod)", path, fk)
		}
	}
}

// Idempotent migration: opening the same DB twice MUST NOT error. The
// "duplicate column name" tolerance in migrate() is the only thing that
// makes this safe.
//
// EMPIRICAL: removing the strings.Contains "duplicate column name" guard
// from the addColumns loop in sqlite.go makes the second Open() error.
func TestMigrate_AddsColumnsIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open should not error after schema is already in place: %v", err)
	}
	defer s2.Close()
}

// Empty userID writes NULL (not ""), so partial indexes on
// user_id IS NOT NULL stay small. Verified by direct SQL because the
// abstract Store interface only surfaces empty-vs-non-empty strings.
func TestCreateSession_EmptyUserIDIsNullInDB(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	emptySess, _ := s.CreateSession(ctx, "t", "a", "")
	var nullCheck *string
	row := s.db.QueryRowContext(ctx, `SELECT user_id FROM sessions WHERE id = ?`, emptySess.ID)
	if err := row.Scan(&nullCheck); err != nil {
		t.Fatal(err)
	}
	if nullCheck != nil {
		t.Errorf("empty userID should write NULL, got %q", *nullCheck)
	}
}

// TestMigrate_UpgradeFromV084ChannelMessages — regression test for
// the v0.8.6 migration ordering bug surfaced 2026-05-13 during the
// v0.8.9 deploy. Setup: hand-create a v0.8.4 channel_messages schema
// (no visible_at, no published_by_user_id) directly via sql.DB,
// then invoke migrate() and verify both columns + the index exist.
//
// Pre-fix, this test FAILS with:
//
//	migrate: SQL logic error: no such column: visible_at (1)
//
// because `CREATE INDEX channel_messages_by_visible` ran inside the
// first `stmts` loop (before the `addColumns` ALTER block) and tried
// to reference a column that didn't exist yet on an upgrade path.
//
// The fix moves the index creation into `addIndexes` (which runs
// AFTER `addColumns`). On a fresh deploy the order doesn't matter
// (CREATE TABLE declared visible_at); on an upgrade it's required.
func TestMigrate_UpgradeFromV084ChannelMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v084.db")

	// Open a raw sql.DB and create the v0.8.4 channel_messages
	// schema (channel_messages without the v0.8.6 columns; the
	// expires_at index from v0.8.4). Close and reopen via the
	// loomcycle store path — that re-runs migrate(), which must
	// successfully upgrade.
	{
		s, err := Open(path)
		if err != nil {
			t.Fatalf("initial open: %v", err)
		}
		// Drop the v0.8.6 columns + index to simulate the
		// pre-v0.8.6 schema state. SQLite doesn't support DROP
		// COLUMN before 3.35; modernc/sqlite is recent enough,
		// but the portable way (and the one that mirrors how a
		// real pre-v0.8.6 deploy looks) is to drop the table and
		// rebuild it without the columns.
		stmts := []string{
			`DROP INDEX IF EXISTS channel_messages_by_visible`,
			`DROP TABLE channel_messages`,
			`CREATE TABLE channel_messages (
				id           TEXT    NOT NULL,
				channel      TEXT    NOT NULL,
				scope        TEXT    NOT NULL,
				scope_id     TEXT    NOT NULL,
				payload      TEXT    NOT NULL,
				published_at INTEGER NOT NULL,
				expires_at   INTEGER,
				PRIMARY KEY (channel, scope, scope_id, id)
			)`,
			`CREATE INDEX channel_messages_by_expires_at ON channel_messages(expires_at) WHERE expires_at IS NOT NULL`,
			// Insert one pre-v0.8.6 row to ensure the data
			// fixup (UPDATE ... SET visible_at = published_at)
			// also runs correctly on upgrade.
			`INSERT INTO channel_messages(id, channel, scope, scope_id, payload, published_at, expires_at)
			   VALUES ('msg_legacy_001', 'findings', 'agent', 'researcher', '{}', 1000, NULL)`,
		}
		ctx := context.Background()
		for _, q := range stmts {
			if _, err := s.db.ExecContext(ctx, q); err != nil {
				t.Fatalf("setup pre-v0.8.6 schema: %q: %v", q, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Re-open. migrate() must successfully add visible_at +
	// published_by_user_id, create the by_visible index, and
	// backfill visible_at from published_at on the legacy row.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade open: %v (this is the v0.8.6 migration order bug if the message mentions `no such column: visible_at`)", err)
	}
	defer s.Close()

	ctx := context.Background()
	// 1. visible_at column exists.
	var hasVisibleAt int
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('channel_messages') WHERE name = 'visible_at'`)
	if err := row.Scan(&hasVisibleAt); err != nil {
		t.Fatal(err)
	}
	if hasVisibleAt != 1 {
		t.Error("visible_at column not added by migration")
	}
	// 2. published_by_user_id column exists.
	var hasPublishedBy int
	row = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('channel_messages') WHERE name = 'published_by_user_id'`)
	if err := row.Scan(&hasPublishedBy); err != nil {
		t.Fatal(err)
	}
	if hasPublishedBy != 1 {
		t.Error("published_by_user_id column not added by migration")
	}
	// 3. channel_messages_by_visible index exists.
	var hasIndex int
	row = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'channel_messages_by_visible'`)
	if err := row.Scan(&hasIndex); err != nil {
		t.Fatal(err)
	}
	if hasIndex != 1 {
		t.Error("channel_messages_by_visible index not created by migration")
	}
	// 4. Backfill ran: legacy row's visible_at = published_at.
	var visibleAt int64
	row = s.db.QueryRowContext(ctx, `SELECT visible_at FROM channel_messages WHERE id = 'msg_legacy_001'`)
	if err := row.Scan(&visibleAt); err != nil {
		t.Fatal(err)
	}
	if visibleAt != 1000 {
		t.Errorf("pre-v0.8.6 row visible_at not backfilled from published_at; got %d, want 1000", visibleAt)
	}
}

// TestMigrate_UpgradeFromLegacyAgentDefPlaneTenantUpserts — RFC N
// regression for the in-place SQLite upgrade bug. Setup: hand-create the
// LEGACY (pre-RFC-N) agent-def-plane schema — agent_defs with
// UNIQUE(name, version), agent_def_active with PRIMARY KEY(name),
// dynamic_agents with PRIMARY KEY(name), none carrying tenant_id. Then
// reopen via the store path (re-runs migrate(), which ALTERs in tenant_id
// but CANNOT rewrite the PK/UNIQUE in place) and assert the three runtime
// upserts SUCCEED.
//
// Pre-fix, this FAILS: the upserts' ON CONFLICT(tenant_id, name) /
// version-bump UNIQUE(tenant_id, name, version) have no matching index on
// the upgraded table, so SQLite refuses with "ON CONFLICT clause does not
// match any PRIMARY KEY or UNIQUE constraint" on the FIRST register/promote
// — broken even single-tenant. The fix adds idempotent CREATE UNIQUE INDEX
// statements in addIndexes that supply the ON CONFLICT target.
func TestMigrate_UpgradeFromLegacyAgentDefPlaneTenantUpserts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy_agentdef.db")

	// 1. Stand up the legacy schema: open (which builds the current
	// schema), then drop + rebuild the three tables in their pre-RFC-N
	// shape (no tenant_id; old PK/UNIQUE). Mirrors how a pre-RFC-N deploy
	// looks on disk.
	{
		s, err := Open(path)
		if err != nil {
			t.Fatalf("initial open: %v", err)
		}
		ctx := context.Background()
		stmts := []string{
			`DROP INDEX IF EXISTS uniq_agent_def_active_tenant_name`,
			`DROP INDEX IF EXISTS uniq_dynamic_agents_tenant_name`,
			`DROP INDEX IF EXISTS uniq_agent_defs_tenant_name_version`,
			`DROP TABLE agent_def_active`,
			`DROP TABLE dynamic_agents`,
			`DROP TABLE agent_defs`,
			// Legacy agent_defs: UNIQUE(name, version), no tenant_id.
			`CREATE TABLE agent_defs (
				def_id                    TEXT    PRIMARY KEY,
				name                      TEXT    NOT NULL,
				version                   INTEGER NOT NULL,
				parent_def_id             TEXT    REFERENCES agent_defs(def_id),
				definition                TEXT    NOT NULL,
				description               TEXT,
				created_at                INTEGER NOT NULL,
				created_by_agent_id       TEXT,
				created_by_run_id         TEXT,
				retired                   INTEGER NOT NULL DEFAULT 0,
				bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
				content_sha256            TEXT,
				UNIQUE(name, version)
			)`,
			// Legacy agent_def_active: PRIMARY KEY(name), no tenant_id.
			`CREATE TABLE agent_def_active (
				name                  TEXT    PRIMARY KEY,
				def_id                TEXT    NOT NULL REFERENCES agent_defs(def_id),
				promoted_at           INTEGER NOT NULL,
				promoted_by_agent_id  TEXT
			)`,
			// Legacy dynamic_agents: PRIMARY KEY(name), no tenant_id.
			`CREATE TABLE dynamic_agents (
				name        TEXT    PRIMARY KEY,
				definition  BLOB    NOT NULL,
				created_at  INTEGER NOT NULL,
				expires_at  INTEGER NOT NULL DEFAULT 0,
				description TEXT
			)`,
		}
		for _, q := range stmts {
			if _, err := s.db.ExecContext(ctx, q); err != nil {
				t.Fatalf("setup legacy schema: %q: %v", q, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// 2. Reopen — migrate() ALTERs in tenant_id + creates the ON CONFLICT
	// target indexes.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// 3. AgentDefCreate must succeed (exercises UNIQUE(tenant_id, name,
	// version) via the version-bump path on a second create).
	created, err := s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID:      "def_upgrade_v1",
		Name:       "upgrade-agent",
		Definition: []byte(`{"system_prompt":"hi"}`),
		TenantID:   "",
	})
	if err != nil {
		t.Fatalf("AgentDefCreate on upgraded DB failed (RFC N ON CONFLICT target missing?): %v", err)
	}
	if created.Version != 1 {
		t.Errorf("first create version = %d, want 1", created.Version)
	}
	// Second create of the same name bumps the version — relies on the
	// MAX(version) read + UNIQUE(tenant_id, name, version) insert.
	created2, err := s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID:      "def_upgrade_v2",
		Name:       "upgrade-agent",
		Definition: []byte(`{"system_prompt":"hi2"}`),
		TenantID:   "",
	})
	if err != nil {
		t.Fatalf("second AgentDefCreate on upgraded DB failed: %v", err)
	}
	if created2.Version != 2 {
		t.Errorf("second create version = %d, want 2", created2.Version)
	}

	// 4. AgentDefSetActive must succeed (exercises ON CONFLICT(tenant_id,
	// name) on agent_def_active).
	if err := s.AgentDefSetActive(ctx, "", "upgrade-agent", "def_upgrade_v1", "a_admin"); err != nil {
		t.Fatalf("AgentDefSetActive on upgraded DB failed (ON CONFLICT target missing?): %v", err)
	}
	// A re-promote to a different def_id exercises the DO UPDATE branch.
	if err := s.AgentDefSetActive(ctx, "", "upgrade-agent", "def_upgrade_v2", "a_admin"); err != nil {
		t.Fatalf("AgentDefSetActive re-promote on upgraded DB failed: %v", err)
	}

	// 5. DynamicAgentUpsert must succeed (exercises ON CONFLICT(tenant_id,
	// name) on dynamic_agents).
	if err := s.DynamicAgentUpsert(ctx, store.DynamicAgent{
		Name:       "dyn-agent",
		Definition: []byte(`{"system_prompt":"dyn"}`),
		TenantID:   "",
	}); err != nil {
		t.Fatalf("DynamicAgentUpsert on upgraded DB failed (ON CONFLICT target missing?): %v", err)
	}
	// Re-upsert exercises the DO UPDATE branch.
	if err := s.DynamicAgentUpsert(ctx, store.DynamicAgent{
		Name:       "dyn-agent",
		Definition: []byte(`{"system_prompt":"dyn2"}`),
		TenantID:   "",
	}); err != nil {
		t.Fatalf("DynamicAgentUpsert re-upsert on upgraded DB failed: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close errored: %v", err)
	}
}

// BenchmarkConcurrentRuns drives the storetest contract bench against
// SQLite. Run via `go test -bench=. ./internal/store/sqlite/...`.
// Operator-facing throughput numbers from this benchmark are
// captured in docs/POSTGRES.md as the SQLite baseline.
func BenchmarkConcurrentRuns(b *testing.B) {
	for i := 0; i < b.N; i++ {
		path := filepath.Join(b.TempDir(), "bench.db")
		s, err := Open(path)
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		r := storetest.RunConcurrencyBench(b, s, storetest.BenchmarkConfig{})
		b.Logf("sqlite: %s", storetest.FormatResult(r))
		_ = s.Close()
	}
}
