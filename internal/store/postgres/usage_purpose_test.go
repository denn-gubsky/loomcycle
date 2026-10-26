package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

type archivedPurpose struct {
	calls   int64
	tokens  int64
	seconds float64
}

func archiveByPurpose(t *testing.T, pool *pgxpool.Pool, tenant string) map[string]archivedPurpose {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT purpose, call_count, input_tokens, audio_seconds
		FROM usage_archive WHERE tenant_id = $1`, tenant)
	if err != nil {
		t.Fatalf("read usage_archive: %v", err)
	}
	defer rows.Close()
	out := map[string]archivedPurpose{}
	for rows.Next() {
		var p string
		var a archivedPurpose
		if err := rows.Scan(&p, &a.calls, &a.tokens, &a.seconds); err != nil {
			t.Fatal(err)
		}
		if _, dup := out[p]; dup {
			t.Fatalf("two archive rows for purpose %q in one day bucket", p)
		}
		out[p] = a
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestUsageRollup_KeepsPurposeApartAndSumsAudioSeconds — rows that share every
// report dimension but differ in purpose fold into one archive row per purpose,
// and an STT row's seconds survive the fold summed.
func TestUsageRollup_KeepsPurposeApartAndSumsAudioSeconds(t *testing.T) {
	fx := freshSchema(t, pgDSNFromEnv(t))
	defer fx.cleanup()
	st := fx.store.(*Store)
	ctx := context.Background()
	old := time.Now().Add(-10 * 24 * time.Hour)
	for _, r := range []store.TokenUsageRow{
		{Purpose: "", InputTokens: 100},
		{Purpose: store.UsagePurposeOCR, InputTokens: 200},
		{Purpose: store.UsagePurposeOCR, InputTokens: 300},
		{Purpose: store.UsagePurposeSTT, AudioSeconds: 60.5},
		{Purpose: store.UsagePurposeSTT, AudioSeconds: 30.25},
	} {
		r.RunID, r.TenantID, r.Provider, r.Model, r.CredentialSource, r.TS = "r", "t-arch", "p", "m", "operator", old
		if err := st.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RollupAndPruneUsage(ctx, time.Now().Add(-5*24*time.Hour)); err != nil {
		t.Fatalf("RollupAndPruneUsage: %v", err)
	}
	got := archiveByPurpose(t, st.pool, "t-arch")
	want := map[string]archivedPurpose{
		store.UsagePurposeChat: {calls: 1, tokens: 100},
		store.UsagePurposeOCR:  {calls: 2, tokens: 500},
		store.UsagePurposeSTT:  {calls: 2, seconds: 90.75},
	}
	if len(got) != len(want) {
		t.Fatalf("archive = %+v, want %+v", got, want)
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("archive[%s] = %+v, want %+v", p, got[p], w)
		}
	}
}

// TestMigrate0113_OldUsageRowsReadChatAndTheArchiveKeysByPurpose — ledger and
// archive rows written at schema version 112 read back as chat with no seconds
// after migrating up; a later fold of an OCR row on the same day and dimensions
// as an archived chat row lands beside it. Migrating down folds the purposes of
// a day back into one row so the old key can return, and going up again works.
func TestMigrate0113_OldUsageRowsReadChatAndTheArchiveKeysByPurpose(t *testing.T) {
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
	if err := m.Migrate(112); err != nil {
		closeMigrator(m)
		t.Fatalf("migrate to 112: %v", err)
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
		{`INSERT INTO token_usage (run_id, tenant_id, provider, model, credential_source, input_tokens, ts)
		  VALUES ('r-old', 't-up', 'p', 'm', 'operator', 7, now())`, nil},
		{`INSERT INTO usage_archive (period_start, tenant_id, user_id, provider, model, credential_source, input_tokens, cost, cost_currency, call_count)
		  VALUES ($1, 't-up', '', 'p', 'm', 'operator', 40, 1.5, 'USD', 4)`, []any{bucket}},
	} {
		if _, err := seed.Exec(ctx, q.sql, q.args...); err != nil {
			seed.Close()
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	seed.Close()

	st, err := Open(ctx, Config{DSN: storeDSN, MaxOpenConns: 4, AutoMigrate: true})
	if err != nil {
		t.Fatalf("Open (migrating up from 112): %v", err)
	}
	defer st.Close()

	got, err := st.TokenUsageForRun(ctx, "r-old")
	if err != nil || len(got) != 1 || got[0].Purpose != store.UsagePurposeChat || got[0].AudioSeconds != 0 {
		t.Fatalf("legacy ledger row after upgrade = %+v, %v; want purpose chat, 0 seconds", got, err)
	}
	if a := archiveByPurpose(t, st.pool, "t-up"); len(a) != 1 || a[store.UsagePurposeChat] != (archivedPurpose{calls: 4, tokens: 40}) {
		t.Fatalf("legacy archive after upgrade = %+v, want one chat row of 4 calls / 40 tokens", a)
	}

	for _, r := range []store.TokenUsageRow{
		{RunID: "r-ocr", Purpose: store.UsagePurposeOCR, InputTokens: 500},
		{RunID: "r-stt", Purpose: store.UsagePurposeSTT, AudioSeconds: 12.5, Cost: 0.5, CostCurrency: "USD"},
		{RunID: "r-chat", InputTokens: 2},
	} {
		r.TenantID, r.Provider, r.Model, r.CredentialSource, r.TS = "t-up", "p", "m", "operator", old
		if err := st.RecordCallUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RollupAndPruneUsage(ctx, time.Now().Add(-5*24*time.Hour)); err != nil {
		t.Fatalf("RollupAndPruneUsage on the upgraded schema: %v", err)
	}
	a := archiveByPurpose(t, st.pool, "t-up")
	if a[store.UsagePurposeChat] != (archivedPurpose{calls: 5, tokens: 42}) ||
		a[store.UsagePurposeOCR] != (archivedPurpose{calls: 1, tokens: 500}) ||
		a[store.UsagePurposeSTT] != (archivedPurpose{calls: 1, seconds: 12.5}) {
		t.Errorf("archive after folding into the upgraded table = %+v, want chat 5/42, ocr 1/500, stt 1/12.5s", a)
	}

	down, err := newMigrator(storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer closeMigrator(down)
	if err := down.Migrate(112); err != nil {
		t.Fatalf("migrate down to 112: %v", err)
	}
	var rows, calls, tokens int64
	var cost float64
	var currency string
	if err := st.pool.QueryRow(ctx, `SELECT COUNT(*), SUM(call_count), SUM(input_tokens), SUM(cost), MAX(cost_currency)
		FROM usage_archive WHERE tenant_id = 't-up'`).Scan(&rows, &calls, &tokens, &cost, &currency); err != nil {
		t.Fatalf("read the folded archive: %v", err)
	}
	if rows != 1 || calls != 7 || tokens != 542 || cost != 2 || currency != "USD" {
		t.Errorf("archive after down = (rows %d, calls %d, tokens %d, cost %v %s), want (1, 7, 542, 2 USD)",
			rows, calls, tokens, cost, currency)
	}
	if err := down.Up(); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if a := archiveByPurpose(t, st.pool, "t-up"); len(a) != 1 || a[store.UsagePurposeChat].calls != 7 {
		t.Errorf("archive after down + up = %+v, want the one folded chat row", a)
	}
}
