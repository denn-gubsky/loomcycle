package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
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
	s, err := Open(context.Background(), Config{DSN: fix.storeDSN, MaxOpenConns: 4, PgvectorEnabled: true, FullText: cfg, ApplyFullText: true})
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

// ftsConcurrentRounds × ftsConcurrentOpens replicas boot together on a fresh
// schema each round. The race this guards was rare (about one boot in sixty),
// so a single round would pass on broken code most of the time.
const (
	ftsConcurrentRounds = 10
	ftsConcurrentOpens  = 6
)

// Replicas booting at once with unaccent on must all come up: the extension and
// its wrapper are DDL on shared catalog rows, and two sessions creating them
// together fail one of the boots ("tuple concurrently updated").
func TestFullText_ConcurrentUnaccentBootsAllSucceed(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	if !pgvectorInstallable(t, dsn) {
		t.Skip("test Postgres cannot install pgvector; there is no word leg without memory_embeddings")
	}
	cfg := FullTextConfig{Config: "simple", Unaccent: true}
	failed := 0
	for round := 0; round < ftsConcurrentRounds; round++ {
		fix := freshSchemaWithVectors(t, dsn, true)
		// Drop the extension a previous round (or test) installed, so every
		// round races on creating it as well as the wrapper.
		pool := rawPool(t, dsn)
		if _, err := pool.Exec(context.Background(), `DROP EXTENSION IF EXISTS unaccent CASCADE`); err != nil {
			t.Fatalf("drop unaccent: %v", err)
		}
		pool.Close()

		var wg sync.WaitGroup
		errs := make([]error, ftsConcurrentOpens)
		stores := make([]*Store, ftsConcurrentOpens)
		for i := range ftsConcurrentOpens {
			wg.Add(1)
			go func() {
				defer wg.Done()
				stores[i], errs[i] = Open(context.Background(), Config{DSN: fix.storeDSN, MaxOpenConns: 2, PgvectorEnabled: true, FullText: cfg, ApplyFullText: true})
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				failed++
				t.Errorf("round %d boot %d: %v", round, i, err)
				continue
			}
			_ = stores[i].Close()
		}
		fix.cleanup()
	}
	t.Logf("%d of %d concurrent boots failed", failed, ftsConcurrentRounds*ftsConcurrentOpens)
}

// A reboot with unaccent on must leave the wrapper's catalog row alone:
// rewriting it needs the role to own the function and is one more write two
// booting replicas can collide on.
func TestFullText_UnaccentRebootKeepsTheWrapperRow(t *testing.T) {
	fix := ftsFixture(t)
	cfg := FullTextConfig{Config: "simple", Unaccent: true}
	if _, err := ftsReopen(t, fix, cfg); err != nil {
		t.Fatal(err)
	}
	xmin := func() string {
		pool := rawPool(t, fix.storeDSN)
		defer pool.Close()
		var x string
		if err := pool.QueryRow(context.Background(),
			`SELECT xmin::text FROM pg_proc WHERE oid = to_regprocedure('`+ftsUnaccentFunc+`(text)')`).Scan(&x); err != nil {
			t.Fatalf("read wrapper row: %v", err)
		}
		return x
	}
	before := xmin()
	if _, err := ftsReopen(t, fix, cfg); err != nil {
		t.Fatal(err)
	}
	if after := xmin(); after != before {
		t.Fatalf("wrapper rewritten on an unchanged reboot: xmin %s → %s", before, after)
	}
}

// The unaccent extension is database-wide but sits in one schema. Installed in
// the first schema of the installing instance's search_path, it went down with
// that schema and took every other instance's word leg with it.
func TestFullText_UnaccentSurvivesAnotherInstancesSchemaDrop(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	if !pgvectorInstallable(t, dsn) {
		t.Skip("test Postgres cannot install pgvector; there is no word leg without memory_embeddings")
	}
	pool := rawPool(t, dsn)
	if _, err := pool.Exec(context.Background(), `DROP EXTENSION IF EXISTS unaccent CASCADE`); err != nil {
		t.Fatalf("drop unaccent: %v", err)
	}
	pool.Close()
	cfg := FullTextConfig{Config: "simple", Unaccent: true}

	other := freshSchemaWithVectors(t, dsn, true)
	if s, err := Open(context.Background(), Config{DSN: other.storeDSN, MaxOpenConns: 2, PgvectorEnabled: true, FullText: cfg, ApplyFullText: true}); err != nil {
		other.cleanup()
		t.Fatal(err)
	} else {
		_ = s.Close()
	}
	fix := ftsFixture(t)
	if _, err := ftsReopen(t, fix, cfg); err != nil {
		other.cleanup()
		t.Fatal(err)
	}
	other.cleanup()

	ftsSeed(t, fix.store, "k", "café")
	if got := ftsFind(t, fix.store, "cafe"); !sameKeys(got, "k") {
		t.Fatalf("unaccent after another instance's schema was dropped: got %v", got)
	}
}

// Only the server boot (and `migrate up`) applies the setting. Any other process
// that opens the store — a CLI subcommand run with a different config, or none —
// must leave the column alone, or it rebuilds it under the running servers and
// their word leg stops matching.
func TestFullText_NonApplyingOpenLeavesTheColumnAndFollowsIt(t *testing.T) {
	fix := ftsFixture(t)
	if _, err := ftsReopen(t, fix, FullTextConfig{Config: "russian"}); err != nil {
		t.Fatal(err)
	}
	ftsSeed(t, fix.store, "k", "стороны подписали договорами")
	attnum, comment := ftsColumn(t, fix.storeDSN)

	cli, err := Open(context.Background(), Config{DSN: fix.storeDSN, MaxOpenConns: 2, PgvectorEnabled: true, FullText: FullTextConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	if gotAttnum, gotComment := ftsColumn(t, fix.storeDSN); gotAttnum != attnum || gotComment != comment {
		t.Fatalf("a non-applying open rebuilt the column: attnum %d → %d, %q → %q", attnum, gotAttnum, comment, gotComment)
	}
	// Its own config says english; the column is russian, and the query has
	// to read text the way the column did.
	if got := ftsFind(t, cli, "договор"); !sameKeys(got, "k") {
		t.Fatalf("non-applying open does not query the way the column was built: got %v", got)
	}
}

// A replica whose config disagrees rebuilds the column (last writer wins). The
// replicas already running must follow the column within the refresh interval,
// not keep querying with the setting they booted with.
func TestFullText_RunningStoreFollowsARebuildByAnotherOpen(t *testing.T) {
	fix := ftsFixture(t)
	a, err := ftsReopen(t, fix, FullTextConfig{Config: "russian"})
	if err != nil {
		t.Fatal(err)
	}
	a.ftsTTL = time.Nanosecond
	ftsSeed(t, a, "k", "стороны подписали договорами")
	if got := ftsFind(t, a, "договор"); !sameKeys(got, "k") {
		t.Fatalf("staging invalid: russian does not match the stem: %v", got)
	}

	b, err := Open(context.Background(), Config{DSN: fix.storeDSN, MaxOpenConns: 2, PgvectorEnabled: true, FullText: FullTextConfig{}, ApplyFullText: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	if _, comment := ftsColumn(t, fix.storeDSN); !strings.Contains(comment, "config=english") {
		t.Fatalf("staging invalid: the second open did not rebuild to english: %q", comment)
	}
	time.Sleep(time.Millisecond)
	// An english column holds the word form as written; read as russian, the
	// query is the stem and matches nothing.
	if got := ftsFind(t, a, "договорами"); !sameKeys(got, "k") {
		t.Fatalf("running store still queries with its boot setting after another open rebuilt the column: got %v", got)
	}
}

// The query side is rebuilt from the column's marker, so what marker() writes
// has to read back as the same setting, and anything else must be refused
// rather than half-parsed into a config name spliced into a query.
func TestFullTextMarker_ReadsBackWhatItWrites(t *testing.T) {
	for _, cfg := range []FullTextConfig{
		{Config: "english"},
		{Config: "russian", Unaccent: true},
		{Config: "public.ukrainian"},
	} {
		got, ok := parseFullTextMarker(cfg.marker())
		if !ok || got != cfg {
			t.Errorf("marker %q read back as %+v (ok=%v)", cfg.marker(), got, ok)
		}
	}
	if got, ok := parseFullTextMarker(FullTextConfig{}.marker()); !ok || got.effectiveConfig() != "english" || got.Unaccent {
		t.Errorf("default marker read back as %+v (ok=%v)", got, ok)
	}
	for _, m := range []string{
		"",
		"rebuild",
		"loomcycle-fulltext v2 config=english unaccent=false",
		"loomcycle-fulltext v1 config=english",
		"loomcycle-fulltext v1 config=english unaccent=maybe",
		"loomcycle-fulltext v1 config=english'); -- unaccent=false",
	} {
		if got, ok := parseFullTextMarker(m); ok {
			t.Errorf("marker %q accepted as %+v", m, got)
		}
	}
}

// An in-place change to a text search configuration (a new dictionary mapped
// into it) leaves the setting's name unchanged, so the marker matches and no
// boot rebuilds. The documented way to force one is to overwrite the marker.
func TestFullText_OverwrittenMarkerForcesARebuild(t *testing.T) {
	fix := ftsFixture(t)
	if _, err := ftsReopen(t, fix, FullTextConfig{Config: "simple"}); err != nil {
		t.Fatal(err)
	}
	before, _ := ftsColumn(t, fix.storeDSN)
	pool := rawPool(t, fix.storeDSN)
	if _, err := pool.Exec(context.Background(), `COMMENT ON COLUMN memory_embeddings.embed_text_tsv IS 'rebuild'`); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	if _, err := ftsReopen(t, fix, FullTextConfig{Config: "simple"}); err != nil {
		t.Fatal(err)
	}
	if after, comment := ftsColumn(t, fix.storeDSN); after == before || !strings.Contains(comment, "config=simple") {
		t.Fatalf("overwritten marker did not force a rebuild: attnum %d → %d, comment %q", before, after, comment)
	}
}
