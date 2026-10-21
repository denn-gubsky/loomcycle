package sqlshared

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "modernc.org/sqlite"
)

// The adapters are exercised against real drivers: a :memory: SQLite always,
// and Postgres when LOOMCYCLE_TEST_PG_DSN is set. Each test pins its database
// to ONE connection, because a :memory: database and a Postgres TEMP table
// are both per-connection.

func sqliteTestDB(t *testing.T) DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return SQLiteDB(db)
}

func postgresTestDB(t *testing.T) DB {
	t.Helper()
	dsn := os.Getenv("LOOMCYCLE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LOOMCYCLE_TEST_PG_DSN not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return PostgresDB(pool)
}

func forEachEngine(t *testing.T, fn func(t *testing.T, db DB)) {
	t.Run("sqlite", func(t *testing.T) { fn(t, sqliteTestDB(t)) })
	t.Run("postgres", func(t *testing.T) { fn(t, postgresTestDB(t)) })
}

// createKV makes the per-connection table every adapter test writes to. Its
// time column is the type each store uses for one; its doc column is the JSON
// type.
func createKV(t *testing.T, db DB) {
	t.Helper()
	ddl := `CREATE TEMP TABLE kv (k TEXT PRIMARY KEY, v TEXT NOT NULL, at INTEGER NOT NULL, doc TEXT NOT NULL)`
	if db.Dialect() == Postgres {
		ddl = `CREATE TEMP TABLE kv (k TEXT PRIMARY KEY, v TEXT NOT NULL, at TIMESTAMPTZ NOT NULL, doc JSONB NOT NULL)`
	}
	if _, err := db.Exec(context.Background(), ddl); err != nil {
		t.Fatalf("create kv: %v", err)
	}
}

func TestAdapter_RoundTripsRowsTimesAndJSON(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db DB) {
		ctx := context.Background()
		createKV(t, db)
		d := db.Dialect()
		at := time.Date(2026, 10, 20, 9, 0, 0, 123456000, time.UTC) // microseconds: timestamptz's resolution
		insert := `INSERT INTO kv (k, v, at, doc) VALUES (?, ?, ?, ` + d.JSONArg() + `)`
		for _, k := range []string{"b", "a", "c?"} {
			n, err := db.Exec(ctx, insert, k, "v-"+k, d.TimeArg(at), `{"k":"`+k+`"}`)
			if err != nil || n != 1 {
				t.Fatalf("insert %q: n=%d err=%v", k, n, err)
			}
		}

		var (
			v   string
			got time.Time
			doc string
		)
		// The '?' literal must survive the rebind; it is part of the match.
		if err := db.QueryRow(ctx, `SELECT v, at, `+d.JSONText("doc")+` FROM kv WHERE k = ? || '?'`, "c").
			Scan(&v, d.TimeDest(&got), &doc); err != nil {
			t.Fatalf("query row: %v", err)
		}
		if v != "v-c?" || !got.Equal(at) || doc != `{"k": "c?"}` && doc != `{"k":"c?"}` {
			t.Errorf("row = (%q, %v, %q), want (v-c?, %v, {\"k\":\"c?\"})", v, got, doc, at)
		}

		rows, err := db.Query(ctx, `SELECT k FROM kv WHERE k IN (`+Placeholders(2)+`) ORDER BY k`, "a", "b")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		var keys []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatalf("scan: %v", err)
			}
			keys = append(keys, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(keys) != 2 || keys[0] != "a" || keys[1] != "b" {
			t.Errorf("keys = %v err=%v, want [a b]", keys, err)
		}

		n, err := db.Exec(ctx, `UPDATE kv SET v = ? WHERE k <> ?`, "x", "a")
		if err != nil || n != 2 {
			t.Errorf("update: rows affected = %d err=%v, want 2", n, err)
		}
	})
}

func TestAdapter_NoRowsAndUniqueViolationClassifyOnTheRealDriver(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db DB) {
		ctx := context.Background()
		createKV(t, db)
		d := db.Dialect()
		var v string
		err := db.QueryRow(ctx, `SELECT v FROM kv WHERE k = ?`, "absent").Scan(&v)
		if !IsNoRows(err) {
			t.Errorf("missing row: err = %v, want no-rows", err)
		}
		insert := `INSERT INTO kv (k, v, at, doc) VALUES (?, ?, ?, ` + d.JSONArg() + `)`
		if _, err := db.Exec(ctx, insert, "k", "v", d.TimeArg(time.Now()), `{}`); err != nil {
			t.Fatalf("insert: %v", err)
		}
		_, err = db.Exec(ctx, insert, "k", "v2", d.TimeArg(time.Now()), `{}`)
		if !IsUniqueViolation(err) {
			t.Errorf("duplicate key: err = %v, want a unique violation", err)
		}
		if IsNoRows(err) {
			t.Errorf("duplicate key: err = %v also classified as no-rows", err)
		}
	})
}

func TestAdapter_InTxCommitsOnNilAndRollsBackOnError(t *testing.T) {
	forEachEngine(t, func(t *testing.T, db DB) {
		ctx := context.Background()
		createKV(t, db)
		d := db.Dialect()
		insert := `INSERT INTO kv (k, v, at, doc) VALUES (?, ?, ?, ` + d.JSONArg() + `)`

		if err := db.InTx(ctx, func(tx Querier) error {
			if tx.Dialect() != d {
				t.Errorf("tx dialect = %v, want %v", tx.Dialect(), d)
			}
			_, err := tx.Exec(ctx, insert, "kept", "v", d.TimeArg(time.Now()), `{}`)
			return err
		}); err != nil {
			t.Fatalf("committing tx: %v", err)
		}

		boom := errors.New("boom")
		err := db.InTx(ctx, func(tx Querier) error {
			if _, err := tx.Exec(ctx, insert, "dropped", "v", d.TimeArg(time.Now()), `{}`); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("failing tx: err = %v, want boom", err)
		}

		var n int
		if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM kv WHERE k IN (?, ?)`, "kept", "dropped").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Errorf("rows after commit + rollback = %d, want 1 (only the committed one)", n)
		}
	})
}
