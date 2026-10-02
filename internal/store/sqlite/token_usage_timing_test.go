package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// legacyTokenUsageDDL is token_usage as it stood before per-call timing: a
// long-lived deployment has this shape on disk.
const legacyTokenUsageDDL = `CREATE TABLE token_usage (
	id                    INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id                TEXT NOT NULL,
	session_id            TEXT,
	tenant_id             TEXT NOT NULL DEFAULT '',
	user_id               TEXT,
	agent_id              TEXT,
	parent_run_id         TEXT,
	iteration             INTEGER NOT NULL DEFAULT 0,
	provider              TEXT NOT NULL,
	model                 TEXT NOT NULL,
	credential_source     TEXT NOT NULL,
	credential_scope_id   TEXT NOT NULL DEFAULT '',
	input_tokens          INTEGER NOT NULL DEFAULT 0,
	output_tokens         INTEGER NOT NULL DEFAULT 0,
	cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
	cost                  REAL,
	cost_currency         TEXT,
	ts                    INTEGER NOT NULL
)`

// TestTokenUsageTiming_AnUpgradedLedgerGainsTheColumnsAndKeepsItsRows — the
// timing columns arrive by ALTER on an existing ledger, its old rows read back
// unmeasured, and the boot-seed index exists (it names an ALTER-added column, so
// it must be created after the ALTERs).
func TestTokenUsageTiming_AnUpgradedLedgerGainsTheColumnsAndKeepsItsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(legacyTokenUsageDDL); err != nil {
		t.Fatalf("create legacy token_usage: %v", err)
	}
	if _, err := seed.Exec(`INSERT INTO token_usage (run_id, provider, model, credential_source, output_tokens, ts)
		VALUES ('r-old', 'p', 'm', 'operator', 50, ?)`, time.Now().UnixNano()); err != nil {
		t.Fatalf("seed legacy row: %v", err)
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

	old, err := st.TokenUsageForRun(ctx, "r-old")
	if err != nil || len(old) != 1 || old[0].OutputTokens != 50 || old[0].DurationMs != 0 {
		t.Fatalf("legacy row after upgrade = %+v, %v", old, err)
	}
	if err := st.RecordCallUsage(ctx, store.TokenUsageRow{RunID: "r-new", Provider: "p", Model: "m",
		CredentialSource: "operator", OutputTokens: 60, DurationMs: 1234, TTFTMs: 200}); err != nil {
		t.Fatalf("RecordCallUsage on upgraded DB: %v", err)
	}
	rows, err := st.RecentCallTimings(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil || len(rows) != 1 || rows[0].RunID != "r-new" || rows[0].DurationMs != 1234 {
		t.Fatalf("RecentCallTimings = %+v, %v; want only the timed new row", rows, err)
	}
	var n int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'token_usage_timing'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("token_usage_timing index count = %d, %v", n, err)
	}
}
