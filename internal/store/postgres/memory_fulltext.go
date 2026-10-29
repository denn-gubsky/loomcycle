package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// FullTextConfig is how the word leg of a memory search reads text: the text
// search configuration both sides of the match use, and whether diacritics are
// stripped first. The zero value is the `english` configuration without
// unaccent — the shape migrations 0060/0062 create — so a deployment that sets
// nothing is never rebuilt.
type FullTextConfig struct {
	Config   string
	Unaccent bool
}

func (f FullTextConfig) effectiveConfig() string {
	if f.Config == "" {
		return "english"
	}
	return f.Config
}

// marker is what the column's COMMENT records about how it was built. The
// generated expression itself is not compared: Postgres stores it normalised
// (`to_tsvector('english'::regconfig, embed_text)`), and matching our text
// against that normal form would make every Postgres release a chance to
// rebuild a large table for nothing.
func (f FullTextConfig) marker() string {
	return ftsMarkerPrefix + f.effectiveConfig() + ftsMarkerUnaccent + strconv.FormatBool(f.Unaccent)
}

const (
	ftsMarkerPrefix   = "loomcycle-fulltext v1 config="
	ftsMarkerUnaccent = " unaccent="
)

// parseFullTextMarker reads a marker back into the setting it records. ok is
// false for anything marker() would not have written — a hand-edited comment,
// or one from a later format.
func parseFullTextMarker(m string) (FullTextConfig, bool) {
	rest, ok := strings.CutPrefix(m, ftsMarkerPrefix)
	if !ok {
		return FullTextConfig{}, false
	}
	name, flag, ok := strings.Cut(rest, ftsMarkerUnaccent)
	if !ok || !ftsConfigIdent.MatchString(name) {
		return FullTextConfig{}, false
	}
	unaccent, err := strconv.ParseBool(flag)
	if err != nil {
		return FullTextConfig{}, false
	}
	return FullTextConfig{Config: name, Unaccent: unaccent}, true
}

// readFullTextMarker returns the marker the column carries; a column without a
// comment is the english one the migrations built. found is false when the
// column itself is missing.
func readFullTextMarker(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (marker string, found bool, err error) {
	var comment *string
	err = q.QueryRow(ctx, `SELECT col_description(a.attrelid, a.attnum)
		FROM pg_attribute a
		WHERE a.attrelid = 'memory_embeddings'::regclass AND a.attname = 'embed_text_tsv' AND NOT a.attisdropped`).Scan(&comment)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("fulltext: read column: %w", err)
	}
	if comment == nil || *comment == "" {
		return FullTextConfig{}.marker(), true, nil
	}
	return *comment, true, nil
}

// ftsRefreshInterval is how long a store trusts the setting it last read off
// the column. The setting belongs to the column, not to this process: a replica
// booted with a different one rebuilds it under the running ones (the last
// writer wins), and a store still querying the old way finds almost nothing.
const ftsRefreshInterval = 30 * time.Second

// ftsSnapshot is the column's setting as this store last read it.
type ftsSnapshot struct {
	cfg    FullTextConfig
	readAt time.Time
}

// ftsSetting is the setting the column is built with, as of at most one
// refresh interval ago. One caller re-reads a stale setting; the others keep
// using the last one rather than queue behind it.
func (s *Store) ftsSetting(ctx context.Context) FullTextConfig {
	snap := s.fts.Load()
	ttl := s.ftsTTL
	if ttl <= 0 {
		ttl = ftsRefreshInterval
	}
	var last FullTextConfig
	if snap != nil {
		last = snap.cfg
		if time.Since(snap.readAt) < ttl {
			return last
		}
	}
	if !s.ftsRefreshing.CompareAndSwap(false, true) {
		return last
	}
	defer s.ftsRefreshing.Store(false)
	next := last
	marker, found, err := readFullTextMarker(ctx, s.pool)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "postgres: could not re-read the full-text column's setting; searching with the last one", "setting", last.marker(), "err", err)
	case !found:
		slog.WarnContext(ctx, "postgres: memory_embeddings has no full-text column; searching with the last setting", "setting", last.marker())
	default:
		if got, ok := parseFullTextMarker(marker); ok {
			next = got
		} else {
			slog.WarnContext(ctx, "postgres: the full-text column carries a setting this build does not recognise; searching with the last one until a boot rebuilds it", "column", marker, "setting", last.marker())
		}
	}
	// Stamped on failure too, so a failing read costs one query (and one log
	// line) per interval, not one per search.
	s.fts.Store(&ftsSnapshot{cfg: next, readAt: time.Now()})
	return next
}

// ftsConfigIdent mirrors config's check. It is repeated here because the name is
// spliced into DDL, and a store opened by something other than the config
// loader (a test, a tool) must not be able to pass anything else.
var ftsConfigIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

// ftsUnaccentFunc wraps unaccent() so it can sit in a generated column:
// unaccent is STABLE (its dictionary could in principle change), a generated
// column needs IMMUTABLE. Declaring the wrapper IMMUTABLE is the standard
// answer, and is honest here because the column is rebuilt whenever the
// operator changes the setting.
const ftsUnaccentFunc = "loomcycle_fts_unaccent"

// ftsLockKey serialises the rebuild across replicas booting at once. Any
// constant works; it only has to differ from the other advisory locks.
const ftsLockKey = 0x6c6f6f6d_66747331 // "loomfts1"

// applyFullTextConfig makes memory_embeddings.embed_text_tsv match cfg,
// rebuilding the column when it was built differently, and returns the marker
// the column carries when the lock is released. Called from Open only when the
// table exists and the caller is the server boot. It fails the boot on a
// configuration Postgres does not know: the operator named one, and a search
// that silently stayed English would be the defect this block exists to fix.
func (s *Store) applyFullTextConfig(ctx context.Context, cfg FullTextConfig) (string, error) {
	name := cfg.effectiveConfig()
	if !ftsConfigIdent.MatchString(name) {
		return "", fmt.Errorf("memory.fulltext.config %q is not a text search configuration name", name)
	}
	var resolved string
	if err := s.pool.QueryRow(ctx, `SELECT $1::regconfig::text`, name).Scan(&resolved); err != nil {
		return "", fmt.Errorf("memory.fulltext.config %q: Postgres has no such text search configuration (list them with \\dF in psql; a language Postgres does not ship needs a dictionary installed first): %w", name, err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("fulltext: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(ftsLockKey)); err != nil {
		return "", fmt.Errorf("fulltext: lock: %w", err)
	}
	// The extension and the wrapper are catalog rows every replica would
	// write: created outside the lock, two booting together collide on them
	// ("tuple concurrently updated", a duplicate pg_extension row) and one of
	// the boots fails. DDL is transactional, so they commit with the rebuild.
	if cfg.Unaccent {
		if err := ensureUnaccentFunc(ctx, tx); err != nil {
			return "", err
		}
	}
	// Read under the lock: a replica that won the race has already rebuilt.
	current, found, err := readFullTextMarker(ctx, tx)
	if err != nil {
		return "", err
	}
	want := cfg.marker()
	if found && current == want {
		return current, nil
	}

	var rows int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM memory_embeddings`).Scan(&rows); err != nil {
		return "", fmt.Errorf("fulltext: count rows: %w", err)
	}
	// The column may have been built by another replica still running with its
	// own setting. This boot wins; that replica's searches follow the column
	// once they re-read it, and its next boot rebuilds it back.
	slog.Warn("postgres: memory.fulltext differs from the setting the full-text column of memory_embeddings was built with; rebuilding it to this process's setting (the last process to boot wins). The table is locked until it finishes",
		"column", current, "process", want, "rows", rows)

	src := "embed_text"
	if cfg.Unaccent {
		src = ftsUnaccentFunc + "(embed_text)"
	}
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS memory_embeddings_fts`,
		`ALTER TABLE memory_embeddings DROP COLUMN IF EXISTS embed_text_tsv`,
		`ALTER TABLE memory_embeddings ADD COLUMN embed_text_tsv tsvector GENERATED ALWAYS AS (to_tsvector('` + name + `'::regconfig, ` + src + `)) STORED`,
		`CREATE INDEX memory_embeddings_fts ON memory_embeddings USING GIN (embed_text_tsv)`,
		`COMMENT ON COLUMN memory_embeddings.embed_text_tsv IS '` + want + `'`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return "", fmt.Errorf("fulltext: rebuild (%s): %w", stmt, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("fulltext: commit: %w", err)
	}
	slog.Info("postgres: full-text index rebuilt", "config", name, "unaccent", cfg.Unaccent, "rows", rows)
	return want, nil
}

// ensureUnaccentFunc installs the unaccent extension (a trusted extension since
// Postgres 13, so the database owner may) and the IMMUTABLE wrapper over it.
// The wrapper names the extension's schema explicitly: unqualified, it would
// resolve through the caller's search_path, which a generated column must not
// depend on. It runs in the caller's locked transaction.
//
// A new extension goes into public, not the first schema of this instance's
// search_path: it is one per database, and in a per-instance schema it would
// be dropped with that schema, breaking every other instance's wrapper. One
// that already exists, wherever it is, is used as is.
func ensureUnaccentFunc(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS unaccent WITH SCHEMA public`); err != nil {
		return fmt.Errorf("memory.fulltext.unaccent needs the Postgres `unaccent` extension (part of contrib), and this role could not create it: %w", err)
	}
	var ns string
	if err := tx.QueryRow(ctx, `SELECT quote_ident(n.nspname) FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'unaccent'`).Scan(&ns); err != nil {
		return fmt.Errorf("fulltext: locate unaccent: %w", err)
	}
	body := ` SELECT ` + ns + `.unaccent('` + ns + `.unaccent'::regdictionary, $1) `
	// Replacing an identical function still rewrites its pg_proc row, and needs
	// the role to own it; every boot after the first would do both for nothing.
	var same bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_proc
		WHERE oid = to_regprocedure(quote_ident(current_schema()) || '.`+ftsUnaccentFunc+`(text)')
		  AND prosrc = $1 AND provolatile = 'i')`, body).Scan(&same); err != nil {
		return fmt.Errorf("fulltext: read %s: %w", ftsUnaccentFunc, err)
	}
	if same {
		return nil
	}
	ddl := `CREATE OR REPLACE FUNCTION ` + ftsUnaccentFunc + `(text) RETURNS text
		LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
		AS $fn$` + body + `$fn$`
	if _, err := tx.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("fulltext: create %s: %w", ftsUnaccentFunc, err)
	}
	return nil
}

// ftsQuery is the tsquery side of the match, built to read the query text the
// way the column read the stored text. Both the configuration and the unaccent
// pass have to agree with the column, or the leg silently stops matching, so
// cfg is the column's setting (ftsSetting), never this process's config.
func ftsQuery(cfg FullTextConfig, textParam, configParam string) string {
	src := textParam
	if cfg.Unaccent {
		src = ftsUnaccentFunc + "(" + textParam + ")"
	}
	return "plainto_tsquery(" + configParam + "::regconfig, " + src + ")"
}
