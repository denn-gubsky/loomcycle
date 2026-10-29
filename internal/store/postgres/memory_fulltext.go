package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"

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
	return "loomcycle-fulltext v1 config=" + f.effectiveConfig() + " unaccent=" + strconv.FormatBool(f.Unaccent)
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
// rebuilding the column when it was built differently. Called from Open only
// when the table exists. It fails the boot on a configuration Postgres does not
// know: the operator named one, and a search that silently stayed English would
// be the defect this block exists to fix.
func (s *Store) applyFullTextConfig(ctx context.Context, cfg FullTextConfig) error {
	name := cfg.effectiveConfig()
	if !ftsConfigIdent.MatchString(name) {
		return fmt.Errorf("memory.fulltext.config %q is not a text search configuration name", name)
	}
	var resolved string
	if err := s.pool.QueryRow(ctx, `SELECT $1::regconfig::text`, name).Scan(&resolved); err != nil {
		return fmt.Errorf("memory.fulltext.config %q: Postgres has no such text search configuration (list them with \\dF in psql; a language Postgres does not ship needs a dictionary installed first): %w", name, err)
	}
	if cfg.Unaccent {
		if err := s.ensureUnaccentFunc(ctx); err != nil {
			return err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("fulltext: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(ftsLockKey)); err != nil {
		return fmt.Errorf("fulltext: lock: %w", err)
	}
	// Read under the lock: a replica that won the race has already rebuilt.
	var comment *string
	err = tx.QueryRow(ctx, `SELECT col_description(a.attrelid, a.attnum)
		FROM pg_attribute a
		WHERE a.attrelid = 'memory_embeddings'::regclass AND a.attname = 'embed_text_tsv' AND NOT a.attisdropped`).Scan(&comment)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("fulltext: read column: %w", err)
	}
	current := FullTextConfig{}.marker() // no comment = the migrations' english column
	if comment != nil && *comment != "" {
		current = *comment
	}
	want := cfg.marker()
	if current == want && !errors.Is(err, pgx.ErrNoRows) {
		return nil
	}

	var rows int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM memory_embeddings`).Scan(&rows); err != nil {
		return fmt.Errorf("fulltext: count rows: %w", err)
	}
	slog.Warn("postgres: rebuilding the full-text index of memory_embeddings for a changed memory.fulltext setting; the table is locked until it finishes",
		"from", current, "to", want, "rows", rows)

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
			return fmt.Errorf("fulltext: rebuild (%s): %w", stmt, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("fulltext: commit: %w", err)
	}
	slog.Info("postgres: full-text index rebuilt", "config", name, "unaccent", cfg.Unaccent, "rows", rows)
	return nil
}

// ensureUnaccentFunc installs the unaccent extension (a trusted extension since
// Postgres 13, so the database owner may) and the IMMUTABLE wrapper over it.
// The wrapper names the extension's schema explicitly: unqualified, it would
// resolve through the caller's search_path, which a generated column must not
// depend on.
func (s *Store) ensureUnaccentFunc(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS unaccent`); err != nil {
		return fmt.Errorf("memory.fulltext.unaccent needs the Postgres `unaccent` extension (part of contrib), and this role could not create it: %w", err)
	}
	var ns string
	if err := s.pool.QueryRow(ctx, `SELECT quote_ident(n.nspname) FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'unaccent'`).Scan(&ns); err != nil {
		return fmt.Errorf("fulltext: locate unaccent: %w", err)
	}
	ddl := `CREATE OR REPLACE FUNCTION ` + ftsUnaccentFunc + `(text) RETURNS text
		LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
		AS $fn$ SELECT ` + ns + `.unaccent('` + ns + `.unaccent'::regdictionary, $1) $fn$`
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("fulltext: create %s: %w", ftsUnaccentFunc, err)
	}
	return nil
}

// ftsQuery is the tsquery side of the match, built to read the query text the
// way the column read the stored text. Both the configuration and the unaccent
// pass have to agree with the column, or the leg silently stops matching.
func (s *Store) ftsQuery(textParam, configParam string) string {
	src := textParam
	if s.fts.Unaccent {
		src = ftsUnaccentFunc + "(" + textParam + ")"
	}
	return "plainto_tsquery(" + configParam + "::regconfig, " + src + ")"
}
