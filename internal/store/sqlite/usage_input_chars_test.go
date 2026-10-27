package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// archiveCharsByPurpose reads usage_archive's input_chars per purpose.
func archiveCharsByPurpose(t *testing.T, st *Store, tenant string) map[string]int64 {
	t.Helper()
	rows, err := st.db.Query(`SELECT purpose, input_chars FROM usage_archive WHERE tenant_id = ?`, tenant)
	if err != nil {
		t.Fatalf("read usage_archive: %v", err)
	}
	defer func() { _ = rows.Close() }()
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
	st, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
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
	got := archiveCharsByPurpose(t, st, "t-chars")
	if len(got) != 2 || got[store.UsagePurposeTTS] != 5500 || got[store.UsagePurposeChat] != 0 {
		t.Errorf("archive input_chars = %v, want tts 5500 and chat 0", got)
	}
	if a := archiveByPurpose(t, st, "t-chars")[store.UsagePurposeTTS]; a != (archivedPurpose{calls: 2, seconds: 330}) {
		t.Errorf("archived tts = %+v, want 2 calls and 330 s", a)
	}
}

// TestUsageInputChars_AnUpgradedLedgerReadsZeroAndArchivesChars — on a
// database written before input_chars existed (purpose and audio_seconds
// already there), the column arrives by ALTER on both tables, old ledger and
// archive rows read back 0, and a TTS row recorded afterwards folds into the
// archive with its characters.
func TestUsageInputChars_AnUpgradedLedgerReadsZeroAndArchivesChars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-chars.db")
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
			PRIMARY KEY (period_start, tenant_id, user_id, provider, model, credential_source, purpose)
		)`,
		`CREATE INDEX usage_archive_tenant_period ON usage_archive(tenant_id, period_start)`,
	} {
		if _, err := seed.Exec(q); err != nil {
			t.Fatalf("pre-chars schema: %v", err)
		}
	}
	if _, err := seed.Exec(`INSERT INTO token_usage (run_id, tenant_id, provider, model, credential_source, purpose, audio_seconds, ts)
		VALUES ('r-stt', 't-up', 'p', 'm', 'operator', 'stt', 12.5, ?)`, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO usage_archive (period_start, tenant_id, user_id, provider, model, credential_source, input_tokens, call_count)
		VALUES (?, 't-up', '', 'p', 'm', 'operator', 40, 4)`, bucket); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-chars DB must migrate it, got: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	got, err := st.TokenUsageForRun(ctx, "r-stt")
	if err != nil || len(got) != 1 || got[0].Purpose != store.UsagePurposeSTT || got[0].AudioSeconds != 12.5 || got[0].InputChars != 0 {
		t.Fatalf("old ledger row after upgrade = %+v, %v; want stt, 12.5 s, 0 chars", got, err)
	}
	if a := archiveCharsByPurpose(t, st, "t-up"); len(a) != 1 || a[store.UsagePurposeChat] != 0 {
		t.Fatalf("old archive after upgrade = %v, want one chat row with 0 chars", a)
	}

	if err := st.RecordCallUsage(ctx, store.TokenUsageRow{RunID: "r-tts", TenantID: "t-up", Provider: "p", Model: "m",
		CredentialSource: "operator", Purpose: store.UsagePurposeTTS, InputChars: 777, AudioSeconds: 45, TS: old}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RollupAndPruneUsage(ctx, time.Now().Add(-5*24*time.Hour)); err != nil {
		t.Fatalf("RollupAndPruneUsage on the upgraded DB: %v", err)
	}
	if a := archiveCharsByPurpose(t, st, "t-up"); a[store.UsagePurposeTTS] != 777 || a[store.UsagePurposeChat] != 0 {
		t.Errorf("archive after folding a TTS row = %v, want tts 777, chat 0", a)
	}
}
