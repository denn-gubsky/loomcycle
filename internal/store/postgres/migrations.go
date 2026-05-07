package postgres

import (
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsSubdir is the path inside migrationsFS where the .sql files
// live. Embed encodes the directory prefix, so the iofs source needs to
// open with "migrations" as the root.
const migrationsSubdir = "migrations"

// MigrateUp runs every pending up-migration. Idempotent: a no-op when the
// schema is already at the latest version. Used by both the auto-migrate
// startup path (LOOMCYCLE_PG_AUTOMIGRATE=1) and the explicit
// `loomcycle migrate up` subcommand.
//
// dsn is the same Postgres connection string the Store opens with. We
// don't reuse the Store's pgxpool here — golang-migrate's pgx5 driver
// wants a *sql.DB and would clobber the pool's lifecycle on close. A
// short-lived migrator connection is cheap and avoids that coupling.
//
// One caveat: golang-migrate accepts URL-form DSNs only, with a `pgx5://`
// scheme prefix to select the driver. If the operator's DSN uses
// keyword=value form ("host=... port=... user=..."), the wrapper below
// can't translate it. The Open() entry point validates this upfront.
func MigrateUp(dsn string) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer closeMigrator(m)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// MigrateDown rolls every applied migration back. Used only by
// `loomcycle migrate down` (never auto-run on startup — too destructive).
// Useful in dev / CI fixtures to reset state between test runs without
// dropping the whole database.
func MigrateDown(dsn string) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer closeMigrator(m)
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// MigrateStatus returns (currentVersion, dirty) for the database.
// dirty=true means a previous migration failed mid-flight; the operator
// must inspect the schema and either complete or roll back manually
// before any further migration call will succeed.
//
// Returns (0, false, nil) for a fresh database with no schema_migrations
// row yet — that's the no-migrations-applied baseline.
func MigrateStatus(dsn string) (uint, bool, error) {
	m, err := newMigrator(dsn)
	if err != nil {
		return 0, false, err
	}
	defer closeMigrator(m)
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return v, dirty, nil
}

// VerifySchemaCurrent returns nil when the embedded migration set is at
// or behind the database's applied version. Used by Open() when
// auto-migrate is disabled — startup refuses to proceed if the embedded
// binary expects a schema the operator hasn't applied yet, so a botched
// rollout surfaces immediately instead of as runtime SQL errors.
//
// If the DB is AHEAD of the embedded set (operator deployed a newer
// binary in parallel, then rolled back to this one), we still return nil
// — the older binary's queries are forward-compatible with the schema
// they were built against, and refusing to start would prevent rollbacks.
func VerifySchemaCurrent(dsn string) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer closeMigrator(m)

	dbVersion, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		// No schema applied at all. Fail closed: the operator must run
		// `loomcycle migrate up` (or set LOOMCYCLE_PG_AUTOMIGRATE=1).
		return fmt.Errorf("postgres schema not initialised: run `loomcycle migrate up` or set LOOMCYCLE_PG_AUTOMIGRATE=1")
	}
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if dirty {
		return fmt.Errorf("postgres schema is dirty at version %d (a prior migration failed mid-flight); inspect schema_migrations and resolve manually", dbVersion)
	}

	embeddedVersion, err := highestEmbeddedVersion()
	if err != nil {
		return err
	}
	if dbVersion < embeddedVersion {
		return fmt.Errorf("postgres schema is at version %d but this binary expects %d: run `loomcycle migrate up` or set LOOMCYCLE_PG_AUTOMIGRATE=1",
			dbVersion, embeddedVersion)
	}
	return nil
}

// newMigrator constructs a *migrate.Migrate from the embedded source
// and a fresh database connection. Caller must defer closeMigrator(m).
//
// We translate the user-supplied DSN to migrate's required `pgx5://`
// scheme. The pgx5 database driver registers itself for that scheme via
// the blank-import in this package; migrate.New is the URL-driven
// factory that picks it up.
func newMigrator(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrationsFS, migrationsSubdir)
	if err != nil {
		return nil, fmt.Errorf("migrations source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsnToMigrateURL(dsn))
	if err != nil {
		return nil, fmt.Errorf("migrate init: %w", err)
	}
	return m, nil
}

// dsnToMigrateURL rewrites a libpq URL DSN into the form golang-migrate's
// pgx5 driver registers for. The driver looks up its scheme via
// `database://...?...` — for pgx/v5 that scheme is "pgx5". A `postgres://`
// or `postgresql://` URL gets the scheme swapped; anything else is
// returned unchanged so an already-correctly-scoped URL passes through.
func dsnToMigrateURL(dsn string) string {
	const newScheme = "pgx5://"
	for _, s := range []string{"postgres://", "postgresql://"} {
		if len(dsn) >= len(s) && dsn[:len(s)] == s {
			return newScheme + dsn[len(s):]
		}
	}
	return dsn
}

// closeMigrator best-effort closes the migrator's source + driver. The
// driver opens its own connection pool that gets torn down here; the
// Store's pgxpool is unaffected.
func closeMigrator(m *migrate.Migrate) {
	_, _ = m.Close()
}

// highestEmbeddedVersion walks migrationsFS and returns the highest
// numeric prefix among the up-migrations. Used by VerifySchemaCurrent
// to know what the binary expects without round-tripping through
// migrate.Migrate's source iterator.
func highestEmbeddedVersion() (uint, error) {
	entries, err := migrationsFS.ReadDir(migrationsSubdir)
	if err != nil {
		return 0, fmt.Errorf("read embedded migrations: %w", err)
	}
	var max uint
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Format: NNNN_<name>.up.sql
		// We just need the leading numeric prefix; use a tiny scanner.
		var n uint
		for i := 0; i < len(name); i++ {
			c := name[i]
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + uint(c-'0')
		}
		if n > max {
			max = n
		}
	}
	if max == 0 {
		return 0, fmt.Errorf("no migrations found in embedded set")
	}
	return max, nil
}
