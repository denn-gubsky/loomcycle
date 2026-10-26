package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// archivedPurpose is one usage_archive row's purpose split, read off the table.
type archivedPurpose struct {
	calls   int64
	tokens  int64
	seconds float64
}

func archiveByPurpose(t *testing.T, st *Store, tenant string) map[string]archivedPurpose {
	t.Helper()
	rows, err := st.db.Query(`SELECT purpose, call_count, input_tokens, audio_seconds
		FROM usage_archive WHERE tenant_id = ?`, tenant)
	if err != nil {
		t.Fatalf("read usage_archive: %v", err)
	}
	defer func() { _ = rows.Close() }()
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
	st, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	// A fresh database is created on the widened key and has nothing to rebuild.
	for _, lk := range widenedLegacyKeys {
		if has, err := hasConstraintKey(ctx, st.db, lk.table, lk.key); err != nil || has {
			t.Fatalf("fresh %s still has its legacy key %v: %v, %v", lk.table, lk.key, has, err)
		}
	}
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
	got := archiveByPurpose(t, st, "t-arch")
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

// legacyUsageArchiveDDL is usage_archive as it stood before purpose: its key
// is the six report dimensions.
const legacyUsageArchiveDDL = `CREATE TABLE usage_archive (
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
	PRIMARY KEY (period_start, tenant_id, user_id, provider, model, credential_source)
)`

// TestUsagePurpose_AnUpgradedLedgerReadsChatAndKeysTheArchiveByPurpose — on a
// database written before purpose existed, old ledger and archive rows read
// back as chat with no seconds, and the archive's key gains purpose: a later
// fold of an OCR row on the same day and dimensions as an archived chat row
// lands beside it instead of being refused or merged into it.
func TestUsagePurpose_AnUpgradedLedgerReadsChatAndKeysTheArchiveByPurpose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * 24 * time.Hour)
	bucket := (old.UnixNano() / nanosPerDay) * nanosPerDay
	for _, q := range []string{legacyTokenUsageDDL, legacyUsageArchiveDDL,
		`CREATE INDEX usage_archive_tenant_period ON usage_archive(tenant_id, period_start)`} {
		if _, err := seed.Exec(q); err != nil {
			t.Fatalf("legacy schema: %v", err)
		}
	}
	if _, err := seed.Exec(`INSERT INTO token_usage (run_id, tenant_id, provider, model, credential_source, input_tokens, ts)
		VALUES ('r-old', 't-up', 'p', 'm', 'operator', 7, ?)`, time.Now().UnixNano()); err != nil {
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
		t.Fatalf("Open on a legacy DB must migrate it, got: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	got, err := st.TokenUsageForRun(ctx, "r-old")
	if err != nil || len(got) != 1 || got[0].Purpose != store.UsagePurposeChat || got[0].AudioSeconds != 0 {
		t.Fatalf("legacy ledger row after upgrade = %+v, %v; want purpose chat, 0 seconds", got, err)
	}
	if a := archiveByPurpose(t, st, "t-up"); len(a) != 1 || a[store.UsagePurposeChat] != (archivedPurpose{calls: 4, tokens: 40}) {
		t.Fatalf("legacy archive after upgrade = %+v, want one chat row of 4 calls / 40 tokens", a)
	}
	has, err := hasConstraintKey(ctx, st.db, "usage_archive",
		[]string{"period_start", "tenant_id", "user_id", "provider", "model", "credential_source", "purpose"})
	if err != nil || !has {
		t.Fatalf("upgraded usage_archive key includes purpose = %v, %v", has, err)
	}
	var idx int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'usage_archive_tenant_period'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("usage_archive_tenant_period after the rebuild: %d, %v", idx, err)
	}

	if err := st.RecordCallUsage(ctx, store.TokenUsageRow{RunID: "r-ocr", TenantID: "t-up", Provider: "p", Model: "m",
		CredentialSource: "operator", InputTokens: 500, Purpose: store.UsagePurposeOCR, TS: old}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCallUsage(ctx, store.TokenUsageRow{RunID: "r-chat", TenantID: "t-up", Provider: "p", Model: "m",
		CredentialSource: "operator", InputTokens: 2, TS: old}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RollupAndPruneUsage(ctx, time.Now().Add(-5*24*time.Hour)); err != nil {
		t.Fatalf("RollupAndPruneUsage on the upgraded DB: %v", err)
	}
	a := archiveByPurpose(t, st, "t-up")
	if a[store.UsagePurposeChat] != (archivedPurpose{calls: 5, tokens: 42}) || a[store.UsagePurposeOCR] != (archivedPurpose{calls: 1, tokens: 500}) {
		t.Errorf("archive after folding into the upgraded table = %+v, want chat 5/42 and ocr 1/500", a)
	}

	// A second boot finds nothing to rebuild and keeps the rows.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = st2.Close() }()
	if a2 := archiveByPurpose(t, st2, "t-up"); len(a2) != 2 {
		t.Errorf("archive after a second boot = %+v, want the two rows", a2)
	}
}
