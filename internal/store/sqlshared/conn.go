package sqlshared

import (
	"context"
	"database/sql"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Row is one result row: *sql.Row and pgx.Row both satisfy it.
type Row interface {
	Scan(dest ...any) error
}

// Rows is a result set. pgx.Rows satisfies it as is; *sql.Rows is wrapped
// because its Close returns an error.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// Querier runs one statement written with `?` placeholders (see Rebind).
// Both a whole database and an open transaction are Queriers.
type Querier interface {
	Dialect() Dialect
	// Exec runs a statement and reports how many rows it affected.
	Exec(ctx context.Context, query string, args ...any) (int64, error)
	QueryRow(ctx context.Context, query string, args ...any) Row
	Query(ctx context.Context, query string, args ...any) (Rows, error)
}

// DB is a Querier that can also run work in one transaction.
type DB interface {
	Querier
	// InTx runs fn in a transaction, committing when fn returns nil and
	// rolling back otherwise.
	InTx(ctx context.Context, fn func(tx Querier) error) error
}

// ---- database/sql (the SQLite store) ----

type sqlRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SQLiteDB adapts the SQLite store's *sql.DB. Queries pass through unchanged:
// `?` is SQLite's own placeholder.
func SQLiteDB(db *sql.DB) DB {
	return sqliteDB{sqliteQuerier: sqliteQuerier{r: db}, db: db}
}

type sqliteQuerier struct{ r sqlRunner }

func (sqliteQuerier) Dialect() Dialect { return SQLite }

func (q sqliteQuerier) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := q.r.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (q sqliteQuerier) QueryRow(ctx context.Context, query string, args ...any) Row {
	return q.r.QueryRowContext(ctx, query, args...)
}

func (q sqliteQuerier) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := q.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return sqlRows{rows}, nil
}

type sqlRows struct{ *sql.Rows }

// Close drops the error, as every `defer rows.Close()` in the stores does:
// a read's failure surfaces through Err.
func (r sqlRows) Close() { _ = r.Rows.Close() }

type sqliteDB struct {
	sqliteQuerier
	db *sql.DB
}

// InTx opens a deferred transaction (the SQLite default): the write lock is
// taken at the first write, so work whose read must hold the lock from the
// start still needs BEGIN IMMEDIATE in the backend.
func (d sqliteDB) InTx(ctx context.Context, fn func(tx Querier) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(sqliteQuerier{r: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- pgx (the Postgres store) ----

// PgxRunner is the statement surface a *pgxpool.Pool, a *pgx.Conn and a
// pgx.Tx share.
type PgxRunner interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PgxPool is a PgxRunner that can begin a transaction (*pgxpool.Pool).
type PgxPool interface {
	PgxRunner
	Begin(ctx context.Context) (pgx.Tx, error)
}

// PostgresDB adapts the Postgres store's pool. Every query is rebound from `?`
// to `$n` on the way through. That is a scan of a few hundred bytes per
// statement, which is noise beside the round trip; caching it is not worth a
// shared map.
func PostgresDB(pool PgxPool) DB {
	return pgxDB{pgxQuerier: pgxQuerier{r: pool}, pool: pool}
}

type pgxQuerier struct{ r PgxRunner }

func (pgxQuerier) Dialect() Dialect { return Postgres }

func (q pgxQuerier) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	tag, err := q.r.Exec(ctx, Postgres.Rebind(query), args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (q pgxQuerier) QueryRow(ctx context.Context, query string, args ...any) Row {
	return q.r.QueryRow(ctx, Postgres.Rebind(query), args...)
}

func (q pgxQuerier) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := q.r.Query(ctx, Postgres.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

type pgxDB struct {
	pgxQuerier
	pool PgxPool
}

func (d pgxDB) InTx(ctx context.Context, fn func(tx Querier) error) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(pgxQuerier{r: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
