package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// archiveCharsByPurpose reads usage_archive's input_chars per purpose.
func archiveCharsByPurpose(t *testing.T, pool *pgxpool.Pool, tenant string) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT purpose, input_chars FROM usage_archive WHERE tenant_id = $1`, tenant)
	if err != nil {
		t.Fatalf("read usage_archive: %v", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p string
		var c int64
		if err := rows.Scan(&p, &c); err != nil {
			t.Fatal(err)
		}
		out[p] += c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestUsageRollup_SumsTTSInputChars — a day's TTS rows fold into one archive
// row whose input_chars is their sum, beside a chat row with none; a second
// pass adds nothing.
func TestUsageRollup_SumsTTSInputChars(t *testing.T) {
	fx := freshSchema(t, pgDSNFromEnv(t))
	defer fx.cleanup()
	st := fx.store.(*Store)
	ctx := context.Background()
	old := time.Now().Add(-10 * 24 * time.Hour)
	for _, r := range []store.TokenUsageRow{
		{InputTokens: 100},
		{Purpose: store.UsagePurposeTTS, InputChars: 4000, AudioSeconds: 240},
		{Purpose: store.UsagePurposeTTS, InputChars: 1500, AudioSeconds: 90},
	} {
		r.RunID, r.TenantID, r.Provider, r.Model, r.CredentialSource, r.TS = "r", "t-chars", "p", "m", "operator", old
		if err := st.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().Add(-5 * 24 * time.Hour)
	for pass := 0; pass < 2; pass++ {
		if _, err := st.RollupAndPruneUsage(ctx, cutoff); err != nil {
			t.Fatalf("RollupAndPruneUsage pass %d: %v", pass, err)
		}
	}
	got := archiveCharsByPurpose(t, st.pool, "t-chars")
	if len(got) != 2 || got[store.UsagePurposeTTS] != 5500 || got[store.UsagePurposeChat] != 0 {
		t.Errorf("archive input_chars = %v, want tts 5500 and chat 0", got)
	}
	if a := archiveByPurpose(t, st.pool, "t-chars")[store.UsagePurposeTTS]; a != (archivedPurpose{calls: 2, seconds: 330}) {
		t.Errorf("archived tts = %+v, want 2 calls and 330 s", a)
	}
}

// TestMigrate0116_OldUsageRowsReadZeroCharsAndDownDropsTheColumn — ledger and
// archive rows written at schema version 115 read back with 0 characters
// after migrating up, and a TTS row folds into the archive with its
// characters. Migrating down drops the column from both tables and keeps the
// rows; going up again works.
func TestMigrate0116_OldUsageRowsReadZeroCharsAndDownDropsTheColumn(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	ctx := context.Background()
	schema := uniqueSchemaName(t)

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dial postgres: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer func() { _, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`) }()
	storeDSN := appendOption(dsn, "search_path", schema+",public")

	m, err := newMigrator(storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(115); err != nil {
		closeMigrator(m)
		t.Fatalf("migrate to 115: %v", err)
	}
	closeMigrator(m)

	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	bucket := time.Date(old.Year(), old.Month(), old.Day(), 0, 0, 0, 0, time.UTC)
	seed, err := pgxpool.New(ctx, storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO token_usage (run_id, tenant_id, provider, model, credential_source, purpose, audio_seconds, ts)
		  VALUES ('r-stt', 't-up', 'p', 'm', 'operator', 'stt', 12.5, now())`, nil},
		{`INSERT INTO usage_archive (period_start, tenant_id, user_id, provider, model, credential_source, input_tokens, call_count)
		  VALUES ($1, 't-up', '', 'p', 'm', 'operator', 40, 4)`, []any{bucket}},
	} {
		if _, err := seed.Exec(ctx, q.sql, q.args...); err != nil {
			seed.Close()
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	seed.Close()

	st, err := Open(ctx, Config{DSN: storeDSN, MaxOpenConns: 4, AutoMigrate: true})
	if err != nil {
		t.Fatalf("Open (migrating up from 115): %v", err)
	}
	defer st.Close()

	got, err := st.TokenUsageForRun(ctx, "r-stt")
	if err != nil || len(got) != 1 || got[0].Purpose != store.UsagePurposeSTT || got[0].AudioSeconds != 12.5 || got[0].InputChars != 0 {
		t.Fatalf("old ledger row after upgrade = %+v, %v; want stt, 12.5 s, 0 chars", got, err)
	}
	if a := archiveCharsByPurpose(t, st.pool, "t-up"); len(a) != 1 || a[store.UsagePurposeChat] != 0 {
		t.Fatalf("old archive after upgrade = %v, want one chat row with 0 chars", a)
	}
	if err := st.RecordCallUsage(ctx, store.TokenUsageRow{RunID: "r-tts", TenantID: "t-up", Provider: "p", Model: "m",
		CredentialSource: "operator", Purpose: store.UsagePurposeTTS, InputChars: 777, AudioSeconds: 45, TS: old}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RollupAndPruneUsage(ctx, time.Now().Add(-5*24*time.Hour)); err != nil {
		t.Fatalf("RollupAndPruneUsage on the upgraded schema: %v", err)
	}
	if a := archiveCharsByPurpose(t, st.pool, "t-up"); a[store.UsagePurposeTTS] != 777 || a[store.UsagePurposeChat] != 0 {
		t.Errorf("archive after folding a TTS row = %v, want tts 777, chat 0", a)
	}

	hasChars := func() int {
		t.Helper()
		var n int
		if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = $1 AND table_name IN ('token_usage', 'usage_archive') AND column_name = 'input_chars'`,
			schema).Scan(&n); err != nil {
			t.Fatalf("read information_schema: %v", err)
		}
		return n
	}
	down, err := newMigrator(storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer closeMigrator(down)
	if err := down.Migrate(115); err != nil {
		t.Fatalf("migrate down to 115: %v", err)
	}
	if n := hasChars(); n != 0 {
		t.Errorf("input_chars columns after down = %d, want 0", n)
	}
	var rows int64
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM usage_archive WHERE tenant_id = 't-up'`).Scan(&rows); err != nil || rows != 2 {
		t.Errorf("archive rows after down = %d, %v; want the chat and tts rows kept", rows, err)
	}
	if err := down.Up(); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if n := hasChars(); n != 2 {
		t.Errorf("input_chars columns after down + up = %d, want 2", n)
	}
}
