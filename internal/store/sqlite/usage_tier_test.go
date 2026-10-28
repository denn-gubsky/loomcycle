package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// currentUsageArchiveKey is the archive's key today: the report dimensions,
// purpose, and the model and user tiers.
var currentUsageArchiveKey = []string{"period_start", "tenant_id", "user_id", "provider", "model",
	"credential_source", "purpose", "tier", "user_tier"}

// TestUsageTier_AnArchiveKeyedByPurposeGainsTheTiers — on a database written
// after purpose joined the archive's key but before the tiers did, the
// archive is rebuilt onto the current key with its rows kept (reading empty for
// both tiers), and a later fold of the same day and dimensions under another
// tier lands beside the old row instead of being refused or merged into it.
func TestUsageTier_AnArchiveKeyedByPurposeGainsTheTiers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "purpose-keyed.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	bucket := (old.UnixNano() / nanosPerDay) * nanosPerDay
	for _, q := range []string{
		legacyTokenUsageDDL,
		`ALTER TABLE token_usage ADD COLUMN purpose TEXT NOT NULL DEFAULT 'chat'`,
		`ALTER TABLE token_usage ADD COLUMN audio_seconds REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE token_usage ADD COLUMN input_chars INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE usage_archive (
			period_start          INTEGER NOT NULL,
			tenant_id             TEXT NOT NULL DEFAULT '',
			user_id               TEXT NOT NULL DEFAULT '',
			provider              TEXT NOT NULL,
			model                 TEXT NOT NULL,
			credential_source     TEXT NOT NULL,
			input_tokens          INTEGER NOT NULL DEFAULT 0,
			output_tokens         INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
			cost                  REAL NOT NULL DEFAULT 0,
			cost_currency         TEXT NOT NULL DEFAULT '',
			call_count            INTEGER NOT NULL DEFAULT 0,
			unpriced_calls        INTEGER NOT NULL DEFAULT 0,
			purpose               TEXT NOT NULL DEFAULT 'chat',
			audio_seconds         REAL NOT NULL DEFAULT 0,
			input_chars           INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (period_start, tenant_id, user_id, provider, model, credential_source, purpose)
		)`,
		`CREATE INDEX usage_archive_tenant_period ON usage_archive(tenant_id, period_start)`,
	} {
		if _, err := seed.Exec(q); err != nil {
			t.Fatalf("purpose-keyed schema: %v", err)
		}
	}
	if _, err := seed.Exec(`INSERT INTO usage_archive (period_start, tenant_id, user_id, provider, model, credential_source, purpose, input_tokens, call_count)
		VALUES (?, 't-tier', '', 'p', 'm', 'operator', 'chat', 40, 4)`, bucket); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a purpose-keyed DB must migrate it, got: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	if has, err := hasConstraintKey(ctx, st.db, "usage_archive", currentUsageArchiveKey); err != nil || !has {
		t.Fatalf("upgraded usage_archive key includes the tiers = %v, %v", has, err)
	}

	if err := st.RecordCallUsage(ctx, store.TokenUsageRow{RunID: "r-mid", TenantID: "t-tier", Provider: "p", Model: "m",
		CredentialSource: "operator", InputTokens: 5, Tier: "middle", UserTier: "pro", TS: old}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RollupAndPruneUsage(ctx, time.Now().Add(-5*24*time.Hour)); err != nil {
		t.Fatalf("RollupAndPruneUsage on the upgraded DB: %v", err)
	}
	got := archiveByTier(t, st, "t-tier")
	want := map[[2]string]int64{{"", ""}: 40, {"middle", "pro"}: 5}
	if len(got) != len(want) {
		t.Fatalf("archive by tier = %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("archive[%v] = %d tokens, want %d", k, got[k], w)
		}
	}
}

// archiveByTier reads the archive's input tokens per (tier, user tier).
func archiveByTier(t *testing.T, st *Store, tenant string) map[[2]string]int64 {
	t.Helper()
	rows, err := st.db.Query(`SELECT tier, user_tier, input_tokens FROM usage_archive WHERE tenant_id = ?`, tenant)
	if err != nil {
		t.Fatalf("read usage_archive: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[[2]string]int64{}
	for rows.Next() {
		var tier, userTier string
		var in int64
		if err := rows.Scan(&tier, &userTier, &in); err != nil {
			t.Fatal(err)
		}
		k := [2]string{tier, userTier}
		if _, dup := out[k]; dup {
			t.Fatalf("two archive rows for tiers %v in one day bucket", k)
		}
		out[k] = in
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
