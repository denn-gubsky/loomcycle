package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// defPlanes are the versioned-definition tables and their active pointers, in
// their shape before the tenant axis: UNIQUE(name, version) and a PK of name.
// sha marks the planes whose table already had content_sha256 by then.
var defPlanes = []struct {
	defs, active string
	sha          bool
}{
	{"agent_defs", "agent_def_active", true},
	{"skill_defs", "skill_def_active", true},
	{"mcp_server_defs", "mcp_server_def_active", true},
	{"memory_backend_defs", "memory_backend_def_active", false},
	{"a2a_agent_defs", "a2a_agent_def_active", false},
	{"schedule_defs", "schedule_def_active", false},
	{"a2a_server_card_defs", "a2a_server_card_def_active", false},
	{"webhook_defs", "webhook_def_active", false},
}

// legacyTenantBlindSchema is each affected table as it was created before its
// tenant migration — copied from the CREATE TABLE text sqlite.go had then — with
// one row in each, under the shared tenant. Columns added since by ALTER are
// absent, exactly as on a long-lived deployment's disk.
func legacyTenantBlindSchema() []string {
	var out []string
	for _, p := range defPlanes {
		sha := ""
		if p.sha {
			sha = "content_sha256 TEXT,"
		}
		out = append(out,
			`CREATE TABLE `+p.defs+` (
				def_id                    TEXT    PRIMARY KEY,
				name                      TEXT    NOT NULL,
				version                   INTEGER NOT NULL,
				parent_def_id             TEXT    REFERENCES `+p.defs+`(def_id),
				definition                TEXT    NOT NULL,
				description               TEXT,
				created_at                INTEGER NOT NULL,
				created_by_agent_id       TEXT,
				created_by_run_id         TEXT,
				retired                   INTEGER NOT NULL DEFAULT 0,
				bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
				`+sha+`
				UNIQUE(name, version)
			)`,
			`CREATE INDEX `+p.defs+`_by_name ON `+p.defs+`(name, version DESC)`,
			`CREATE TABLE `+p.active+` (
				name                  TEXT    PRIMARY KEY,
				def_id                TEXT    NOT NULL REFERENCES `+p.defs+`(def_id),
				promoted_at           INTEGER NOT NULL,
				promoted_by_agent_id  TEXT
			)`,
			`INSERT INTO `+p.defs+` (def_id, name, version, definition, created_at)
			 VALUES ('`+p.defs+`_1', 'shared', 1, '{}', 1)`,
			`INSERT INTO `+p.active+` (name, def_id, promoted_at) VALUES ('shared', '`+p.defs+`_1', 1)`,
		)
	}
	return append(out,
		`CREATE TABLE dynamic_agents (
			name        TEXT PRIMARY KEY,
			definition  BLOB    NOT NULL,
			created_at  INTEGER NOT NULL,
			expires_at  INTEGER NOT NULL DEFAULT 0,
			description TEXT
		)`,
		`INSERT INTO dynamic_agents (name, definition, created_at) VALUES ('shared', '{}', 1)`,
		// The ON DELETE CASCADE child: dropping schedule_defs with foreign keys
		// on would delete this row.
		`CREATE TABLE schedule_run_state (
			def_id          TEXT    PRIMARY KEY REFERENCES schedule_defs(def_id) ON DELETE CASCADE,
			last_run_at     INTEGER,
			last_run_id     TEXT,
			last_status     TEXT,
			last_error      TEXT,
			next_run_at     INTEGER NOT NULL,
			paused_until    INTEGER
		)`,
		`INSERT INTO schedule_run_state (def_id, next_run_at) VALUES ('schedule_defs_1', 1)`,
		`CREATE TABLE channel_messages (
			id                   TEXT    NOT NULL,
			channel              TEXT    NOT NULL,
			scope                TEXT    NOT NULL,
			scope_id             TEXT    NOT NULL,
			payload              TEXT    NOT NULL,
			published_at         INTEGER NOT NULL,
			expires_at           INTEGER,
			visible_at           INTEGER NOT NULL DEFAULT 0,
			published_by_user_id TEXT,
			PRIMARY KEY (channel, scope, scope_id, id)
		)`,
		`INSERT INTO channel_messages (id, channel, scope, scope_id, payload, published_at, visible_at)
		 VALUES ('msg_1', 'ch', 'user', 'alice', '{}', 1, 1)`,
		`CREATE TABLE channel_cursors (
			channel    TEXT    NOT NULL,
			scope      TEXT    NOT NULL,
			scope_id   TEXT    NOT NULL,
			cursor     TEXT    NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (channel, scope, scope_id)
		)`,
		`INSERT INTO channel_cursors (channel, scope, scope_id, cursor, updated_at)
		 VALUES ('ch', 'user', 'alice', '`+legacyCursor+`', 1)`,
		`CREATE TABLE channels (
			name         TEXT    PRIMARY KEY,
			description  TEXT    NOT NULL DEFAULT '',
			scope        TEXT    NOT NULL,
			semantic     TEXT    NOT NULL,
			default_ttl  INTEGER NOT NULL DEFAULT 0,
			max_messages INTEGER NOT NULL DEFAULT 0,
			publisher    TEXT    NOT NULL DEFAULT '',
			period       TEXT    NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL
		)`,
		`INSERT INTO channels (name, scope, semantic, created_at) VALUES ('shared', 'user', 'queue', 1)`,
		`CREATE TABLE memory (
			scope       TEXT NOT NULL,
			scope_id    TEXT NOT NULL,
			key         TEXT NOT NULL,
			value       TEXT NOT NULL,
			expires_at  INTEGER,
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			PRIMARY KEY (scope, scope_id, key)
		)`,
		`CREATE INDEX memory_by_expires_at ON memory(expires_at) WHERE expires_at IS NOT NULL`,
		`INSERT INTO memory (scope, scope_id, key, value, created_at, updated_at)
		 VALUES ('user', 'alice', 'pref', '"legacy"', 1, 1)`,
	)
}

var legacyCursor = store.EncodeChannelCursor(time.Unix(0, 1), "msg_000000000000000000000001")

// openLegacyTenantBlindDB lays the legacy schema down on disk, closes it, and
// opens it through the store so migrate() faces a real pre-existing database.
func openLegacyTenantBlindDB(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy_tenant_blind.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	for _, q := range legacyTenantBlindSchema() {
		if _, err := seed.Exec(q); err != nil {
			t.Fatalf("legacy schema %q: %v", firstLine(q), err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on the legacy DB must migrate it, got: %v", err)
	}
	return st, path
}

func firstLine(q string) string {
	return strings.TrimSpace(strings.SplitN(q, "\n", 2)[0])
}

func tenantRows(t *testing.T, st *Store, table, tenant string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM "`+table+`" WHERE tenant_id = ?`, tenant).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestMigrate_UpgradedDBAdmitsASecondTenantOnEveryTenantKeyedTable is the
// regression test for the tenant-blind keys an upgraded SQLite database kept.
//
// The tenant migrations added tenant_id and a tenant-leading unique index, but
// SQLite cannot alter a PRIMARY KEY or UNIQUE constraint, so the old key stayed
// and fired first: a second tenant acking the same cursor, writing the same
// memory key, or creating a same-named channel or definition failed with
// "UNIQUE constraint failed". A fresh database never had the old key, which is
// why every test that starts empty passed.
func TestMigrate_UpgradedDBAdmitsASecondTenantOnEveryTenantKeyedTable(t *testing.T) {
	st, path := openLegacyTenantBlindDB(t)
	ctx := context.Background()

	// Every legacy row survived, under the shared tenant.
	for _, lk := range tenantBlindLegacyKeys {
		if n := tenantRows(t, st, lk.table, ""); n != 1 {
			t.Errorf("%s: %d rows under tenant '' after migrate, want the 1 legacy row", lk.table, n)
		}
	}
	// The cascade child of schedule_defs survived the drop of its parent.
	var runState int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM schedule_run_state WHERE def_id = 'schedule_defs_1'`).Scan(&runState); err != nil {
		t.Fatalf("count schedule_run_state: %v", err)
	}
	if runState != 1 {
		t.Errorf("schedule_run_state row lost across the schedule_defs rebuild (got %d rows)", runState)
	}
	// The legacy table's own indexes came back with it.
	for _, idx := range []string{"agent_defs_by_name", "webhook_defs_by_name", "memory_by_expires_at", "uniq_channel_cursors_tenant"} {
		var name string
		if err := st.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&name); err != nil {
			t.Errorf("index %s missing after the rebuild: %v", idx, err)
		}
	}

	// A second tenant, through the store, on the same old key of each headline
	// write path.
	const b = "tenant-b"
	cursorB := store.EncodeChannelCursor(time.Unix(0, 2), "msg_000000000000000000000002")
	if err := st.ChannelAck(ctx, b, "ch", store.MemoryScopeUser, "alice", cursorB); err != nil {
		t.Errorf("ChannelAck for a second tenant: %v", err)
	}
	for tenant, want := range map[string]string{"": legacyCursor, b: cursorB} {
		got, err := st.ChannelCommittedCursor(ctx, tenant, "ch", store.MemoryScopeUser, "alice")
		if err != nil || got != want {
			t.Errorf("cursor for tenant %q = %q, %v; want %q", tenant, got, err, want)
		}
	}
	restored, err := st.SnapshotRestoreChannelCursor(ctx, store.ChannelCursorEntry{
		Channel: "ch", TenantID: "tenant-c", Scope: store.MemoryScopeUser, ScopeID: "alice", Cursor: cursorB,
	})
	if err != nil || !restored {
		t.Errorf("snapshot restore of a third tenant's cursor = %v, %v; want inserted", restored, err)
	}

	if err := st.MemorySet(ctx, b, store.MemoryScopeUser, "alice", "pref", json.RawMessage(`"b"`), 0); err != nil {
		t.Errorf("MemorySet for a second tenant: %v", err)
	}
	for tenant, want := range map[string]string{"": `"legacy"`, b: `"b"`} {
		e, err := st.MemoryGet(ctx, tenant, store.MemoryScopeUser, "alice", "pref")
		if err != nil || string(e.Value) != want {
			t.Errorf("memory for tenant %q = %s, %v; want %s", tenant, e.Value, err, want)
		}
	}

	if err := st.ChannelsCreate(ctx, store.ChannelRow{Name: "shared", TenantID: b, Scope: "user", Semantic: "queue"}); err != nil {
		t.Errorf("ChannelsCreate for a second tenant: %v", err)
	}
	for _, tenant := range []string{"", b} {
		if c, err := st.ChannelGet(ctx, tenant, "shared"); err != nil || c.TenantID != tenant {
			t.Errorf("channel for tenant %q = %+v, %v", tenant, c, err)
		}
	}

	if _, err := st.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "adf_b", Name: "shared", Version: 1, Definition: json.RawMessage(`{}`), TenantID: b,
	}); err != nil {
		t.Errorf("AgentDefCreate for a second tenant: %v", err)
	}
	if err := st.AgentDefSetActive(ctx, b, "shared", "adf_b", ""); err != nil {
		t.Errorf("AgentDefSetActive for a second tenant: %v", err)
	}
	for tenant, want := range map[string]string{"": "agent_defs_1", b: "adf_b"} {
		if row, err := st.AgentDefGetActive(ctx, tenant, "shared"); err != nil || row.DefID != want {
			t.Errorf("active agent for tenant %q = %q, %v; want %q", tenant, row.DefID, err, want)
		}
	}

	if err := st.DynamicAgentUpsert(ctx, store.DynamicAgent{Name: "shared", Definition: []byte(`{}`), TenantID: b}); err != nil {
		t.Errorf("DynamicAgentUpsert for a second tenant: %v", err)
	}
	for _, tenant := range []string{"", b} {
		if a, err := st.DynamicAgentGet(ctx, tenant, "shared"); err != nil || a.TenantID != tenant {
			t.Errorf("dynamic agent for tenant %q = %+v, %v", tenant, a, err)
		}
	}

	// Every other table, directly: a copy of the legacy row under another
	// tenant must be admitted. def_id is a global id, so the copy gets its own.
	const raw = "tenant-raw"
	for _, lk := range tenantBlindLegacyKeys {
		cols, err := queryStrings(ctx, st.db, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, lk.table)
		if err != nil {
			t.Fatalf("table_info %s: %v", lk.table, err)
		}
		exprs := make([]string, len(cols))
		for i, c := range cols {
			switch c {
			case "tenant_id":
				exprs[i] = `'` + raw + `'`
			case "def_id":
				exprs[i] = `def_id || '_raw'`
			default:
				exprs[i] = `"` + c + `"`
			}
		}
		q := `INSERT INTO "` + lk.table + `" ("` + strings.Join(cols, `", "`) + `") SELECT ` +
			strings.Join(exprs, ", ") + ` FROM "` + lk.table + `" WHERE tenant_id = ''`
		if _, err := st.db.ExecContext(ctx, q); err != nil {
			t.Errorf("%s: a second tenant's copy of the legacy row was refused: %v", lk.table, err)
		}
	}

	// Nothing is left to rebuild, and a second boot is a no-op that keeps
	// every row.
	if left, err := tenantKeyRebuildCandidates(ctx, st.db); err != nil || len(left) != 0 {
		t.Fatalf("tables still on a tenant-blind key after migrate: %v, %v", left, err)
	}
	counts := map[string]int{}
	for _, lk := range tenantBlindLegacyKeys {
		counts[lk.table] = tenantRows(t, st, lk.table, raw)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = st2.Close() }()
	for _, lk := range tenantBlindLegacyKeys {
		if got := tenantRows(t, st2, lk.table, raw); got != counts[lk.table] {
			t.Errorf("%s: %d rows for %s after the second boot, want %d", lk.table, got, raw, counts[lk.table])
		}
		if got := tenantRows(t, st2, lk.table, ""); got != 1 {
			t.Errorf("%s: %d legacy rows after the second boot, want 1", lk.table, got)
		}
	}
}

// TestMigrate_FreshDBCarriesEveryTenantLeadingKeyAndNeedsNoRebuild pins the
// other side: a fresh database has none of the legacy keys (so the rebuild never
// runs) and does have each tenant-leading key — which also catches a legacy-key
// entry that no longer describes its table's current key.
func TestMigrate_FreshDBCarriesEveryTenantLeadingKeyAndNeedsNoRebuild(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	if left, err := tenantKeyRebuildCandidates(ctx, st.db); err != nil || len(left) != 0 {
		t.Fatalf("a fresh DB reports tenant-blind keys: %v, %v", left, err)
	}
	for _, lk := range tenantBlindLegacyKeys {
		want := append([]string{"tenant_id"}, lk.key...)
		has, err := hasConstraintKey(ctx, st.db, lk.table, want)
		if err != nil {
			t.Fatalf("%s: %v", lk.table, err)
		}
		if !has {
			t.Errorf("%s: fresh schema has no key on %v", lk.table, want)
		}
	}
}
