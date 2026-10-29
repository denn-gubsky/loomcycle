package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// memory.fulltext: the word leg of a search reads text in the configured
// language. Each test stages rows under the migrations' english column, reopens
// the store with a setting, and asserts on what the leg finds — the property an
// operator actually needs, not the DDL that produces it.

const ftsScope = "fts"

func ftsSeed(t *testing.T, s store.Store, key, text string) {
	t.Helper()
	ctx := context.Background()
	v, _ := json.Marshal(text)
	if err := s.MemorySet(ctx, "", store.MemoryScopeUser, ftsScope, key, v, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.MemoryEmbedSet(ctx, "", store.MemoryScopeUser, ftsScope, key, store.MemoryEmbedding{
		Provider: "test", Model: "m", Dimension: 4, Vector: []float32{1, 0, 0, 0}, EmbedText: text, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func ftsFind(t *testing.T, s store.Store, query string) []string {
	t.Helper()
	rows, err := s.MemoryFullTextSearch(context.Background(), "", store.MemoryScopeUser, ftsScope, store.MemorySearchFilter{}, query, 10)
	if err != nil {
		t.Fatalf("MemoryFullTextSearch(%q): %v", query, err)
	}
	return keysOf(rows)
}

// ftsReopen closes the fixture's store and opens a new one on the same schema
// with the given setting — a restart with a changed config.
func ftsReopen(t *testing.T, fix *freshFixture, cfg FullTextConfig) (*Store, error) {
	t.Helper()
	_ = fix.store.Close()
	s, err := Open(context.Background(), Config{DSN: fix.storeDSN, MaxOpenConns: 4, PgvectorEnabled: true, FullText: cfg})
	if err == nil {
		fix.store = s
	}
	return s, err
}

func ftsColumn(t *testing.T, dsn string) (attnum int, comment string) {
	t.Helper()
	pool := rawPool(t, dsn)
	defer pool.Close()
	var c *string
	if err := pool.QueryRow(context.Background(), `SELECT a.attnum, col_description(a.attrelid, a.attnum)
		FROM pg_attribute a WHERE a.attrelid = 'memory_embeddings'::regclass AND a.attname = 'embed_text_tsv' AND NOT a.attisdropped`).Scan(&attnum, &c); err != nil {
		t.Fatalf("read embed_text_tsv: %v", err)
	}
	if c != nil {
		comment = *c
	}
	return attnum, comment
}

func ftsFixture(t *testing.T) *freshFixture {
	t.Helper()
	dsn := pgDSNFromEnv(t)
	if !pgvectorInstallable(t, dsn) {
		t.Skip("test Postgres cannot install pgvector; there is no word leg without memory_embeddings")
	}
	fix := freshSchemaWithVectors(t, dsn, true)
	f := &fix
	// The fixture's cleanup closes the store it opened; a reopened one is ours.
	t.Cleanup(func() { _ = f.store.Close(); f.cleanup() })
	return f
}

// An unset block must not touch the column: every existing deployment boots on
// it, and a rebuild would lock memory_embeddings for nothing.
func TestFullText_UnsetLeavesTheMigratedColumnAlone(t *testing.T) {
	fix := ftsFixture(t)
	before, comment := ftsColumn(t, fix.storeDSN)
	if comment != "" {
		t.Fatalf("staging invalid: the migrated column already carries %q", comment)
	}
	if _, err := ftsReopen(t, fix, FullTextConfig{}); err != nil {
		t.Fatal(err)
	}
	// Naming the default explicitly is the same setting.
	if _, err := ftsReopen(t, fix, FullTextConfig{Config: "english"}); err != nil {
		t.Fatal(err)
	}
	after, comment := ftsColumn(t, fix.storeDSN)
	if after != before || comment != "" {
		t.Fatalf("column rebuilt for the default setting: attnum %d → %d, comment %q", before, after, comment)
	}
	ftsSeed(t, fix.store, "k", "the contracts were signed")
	if got := ftsFind(t, fix.store, "contract"); !sameKeys(got, "k") {
		t.Fatalf("english stemming lost: %v", got)
	}
}

// The defect: an inflected non-English term never met its other forms, because
// both sides were read as English. Russian is the language Postgres ships a
// stemmer for that stands in for the Ukrainian corpus (whose dictionary has to
// be installed by the operator).
func TestFullText_ConfiguredLanguageMatchesInflectedForms(t *testing.T) {
	fix := ftsFixture(t)
	ftsSeed(t, fix.store, "k", "стороны подписали договорами")
	if got := ftsFind(t, fix.store, "договор"); len(got) != 0 {
		t.Fatalf("staging invalid: english already matches the inflected form: %v", got)
	}
	if _, err := ftsReopen(t, fix, FullTextConfig{Config: "russian"}); err != nil {
		t.Fatal(err)
	}
	if got := ftsFind(t, fix.store, "договор"); !sameKeys(got, "k") {
		t.Fatalf("russian config: inflected form not matched, got %v", got)
	}
	attnum, comment := ftsColumn(t, fix.storeDSN)
	if !strings.Contains(comment, "config=russian") {
		t.Fatalf("rebuilt column does not record its setting: %q", comment)
	}
	// A restart with the same setting is not another rebuild.
	if _, err := ftsReopen(t, fix, FullTextConfig{Config: "russian"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := ftsColumn(t, fix.storeDSN); again != attnum {
		t.Fatalf("column rebuilt on an unchanged setting: attnum %d → %d", attnum, again)
	}
	// And back to the default rebuilds again, so English stems once more.
	if _, err := ftsReopen(t, fix, FullTextConfig{}); err != nil {
		t.Fatal(err)
	}
	ftsSeed(t, fix.store, "e", "the contracts were signed")
	if got := ftsFind(t, fix.store, "contract"); !sameKeys(got, "e") {
		t.Fatalf("back on english: got %v", got)
	}
}

// unaccent has to apply on BOTH sides: indexing alone would leave an accented
// query unmatched, querying alone an accented row.
func TestFullText_UnaccentMatchesAcrossDiacritics(t *testing.T) {
	fix := ftsFixture(t)
	ftsSeed(t, fix.store, "k", "café résumé")
	if _, err := ftsReopen(t, fix, FullTextConfig{Config: "simple"}); err != nil {
		t.Fatal(err)
	}
	if got := ftsFind(t, fix.store, "cafe"); len(got) != 0 {
		t.Fatalf("staging invalid: simple already ignores accents: %v", got)
	}
	if _, err := ftsReopen(t, fix, FullTextConfig{Config: "simple", Unaccent: true}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"cafe", "café", "resume"} {
		if got := ftsFind(t, fix.store, q); !sameKeys(got, "k") {
			t.Errorf("unaccent: %q not matched, got %v", q, got)
		}
	}
}

// A configuration Postgres does not have fails the boot: a search that quietly
// stayed English is the defect the setting exists to fix.
func TestFullText_UnknownConfigurationFailsTheBoot(t *testing.T) {
	fix := ftsFixture(t)
	_, err := ftsReopen(t, fix, FullTextConfig{Config: "klingon"})
	if err == nil || !strings.Contains(err.Error(), "klingon") {
		t.Fatalf("want a boot error naming the configuration, got %v", err)
	}
	// The name is spliced into DDL, so anything but a name is refused before
	// Postgres sees it.
	_, err = ftsReopen(t, fix, FullTextConfig{Config: "english'); DROP TABLE memory; --"})
	if err == nil || !strings.Contains(err.Error(), "not a text search configuration name") {
		t.Fatalf("want the name refused, got %v", err)
	}
}
