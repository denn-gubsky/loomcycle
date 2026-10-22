package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// schemaPool lends the store contract's subtests migrated schemas and takes
// them back emptied. A fresh schema is ~100 migrations, and the contract is
// ~500 subtests run twice; migrating one per subtest was the whole length of
// the CI Postgres job (~800s). A pool needs one schema per subtest running at
// once, and resetting one (a TRUNCATE of every table plus the rows the
// migrations seed) costs a fraction of migrating it.
//
// Only the contract uses it. Tests that run migrations, alter the schema or
// check a fresh one keep freshSchema.
type schemaPool struct {
	dsn   string
	admin *pgxpool.Pool
	free  chan string

	mu      sync.Mutex
	schemas map[string]*pooledSchema
}

// pooledSchema is what resetting a schema needs: its tables and the rows the
// migrations left in them.
type pooledSchema struct {
	tables []string
	seed   map[string]json.RawMessage
}

// poolCounter keeps pooled schema names unique within a process; the clock in
// the name keeps them unique across processes, so a run killed before its
// cleanup leaves schemas the next run cannot collide with.
var poolCounter atomic.Uint64

func newSchemaPool(t *testing.T, dsn string) *schemaPool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 4
	admin, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dial postgres: %v", err)
	}
	p := &schemaPool{dsn: dsn, admin: admin, free: make(chan string, 64), schemas: map[string]*pooledSchema{}}
	t.Cleanup(p.dropAll(t))
	return p
}

// fixture returns a store on an empty, fully migrated schema; its cleanup
// closes the store and hands the schema back.
func (p *schemaPool) fixture(t *testing.T, pgvectorEnabled bool) (store.Store, func()) {
	t.Helper()
	ctx := context.Background()

	var schema string
	reused := false
	select {
	case schema = <-p.free:
		reused = true
	default:
		schema = "lctpool_" + strconv.FormatInt(time.Now().UnixNano(), 36) + "_" + strconv.FormatUint(poolCounter.Add(1), 10)
		if _, err := p.admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
			t.Fatalf("create schema %s: %v", schema, err)
		}
	}

	// public stays on the path after the schema so pgvector, installed
	// database-wide, resolves; see freshSchemaWithVectors. AutoMigrate
	// migrates a new schema and is a version check on a reused one.
	s, err := Open(ctx, Config{
		DSN:             appendOption(p.dsn, "search_path", schema+",public"),
		MaxOpenConns:    8,
		AutoMigrate:     true,
		PgvectorEnabled: pgvectorEnabled,
	})
	if err != nil {
		p.drop(t, schema)
		t.Fatalf("open postgres on %s: %v", schema, err)
	}
	if !reused {
		if err := p.record(ctx, schema); err != nil {
			_ = s.Close()
			p.drop(t, schema)
			t.Fatalf("record seed of %s: %v", schema, err)
		}
	}
	return s, func() {
		_ = s.Close()
		if err := p.reset(ctx, schema); err != nil {
			// A schema that cannot be emptied is never lent again.
			t.Logf("reset %s: %v; dropping it", schema, err)
			p.drop(t, schema)
			return
		}
		p.free <- schema
	}
}

// record notes a freshly migrated schema's tables and the rows the
// migrations put in them, which a reset restores.
func (p *schemaPool) record(ctx context.Context, schema string) error {
	rows, err := p.admin.Query(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname = $1 AND tablename <> 'schema_migrations' ORDER BY tablename`, schema)
	if err != nil {
		return err
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	ps := &pooledSchema{tables: tables, seed: map[string]json.RawMessage{}}
	for _, tbl := range tables {
		var raw json.RawMessage
		q := fmt.Sprintf(`SELECT coalesce(json_agg(t), '[]') FROM %s t`, pgx.Identifier{schema, tbl}.Sanitize())
		if err := p.admin.QueryRow(ctx, q).Scan(&raw); err != nil {
			return err
		}
		if string(raw) != "[]" {
			ps.seed[tbl] = raw
		}
	}
	p.mu.Lock()
	p.schemas[schema] = ps
	p.mu.Unlock()
	return nil
}

// reset empties every table of the schema, restarts its sequences and puts
// back the rows the migrations seeded, in one transaction.
func (p *schemaPool) reset(ctx context.Context, schema string) error {
	p.mu.Lock()
	ps := p.schemas[schema]
	p.mu.Unlock()
	if ps == nil {
		return fmt.Errorf("schema %s was never recorded", schema)
	}
	tx, err := p.admin.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if len(ps.tables) > 0 {
		names := ""
		for i, tbl := range ps.tables {
			if i > 0 {
				names += ", "
			}
			names += pgx.Identifier{schema, tbl}.Sanitize()
		}
		if _, err := tx.Exec(ctx, `TRUNCATE `+names+` RESTART IDENTITY CASCADE`); err != nil {
			return err
		}
	}
	for tbl, raw := range ps.seed {
		id := pgx.Identifier{schema, tbl}.Sanitize()
		if _, err := tx.Exec(ctx, `INSERT INTO `+id+` SELECT * FROM json_populate_recordset(null::`+id+`, $1)`, raw); err != nil {
			return fmt.Errorf("restore %s: %w", tbl, err)
		}
	}
	return tx.Commit(ctx)
}

func (p *schemaPool) drop(t *testing.T, schema string) {
	if _, err := p.admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Logf("drop schema %s: %v", schema, err)
	}
	p.mu.Lock()
	delete(p.schemas, schema)
	p.mu.Unlock()
}

// dropAll runs once every subtest has finished (t.Cleanup on the test that
// owns the pool): it drops every schema the pool made.
func (p *schemaPool) dropAll(t *testing.T) func() {
	return func() {
		p.mu.Lock()
		schemas := make([]string, 0, len(p.schemas))
		for s := range p.schemas {
			schemas = append(schemas, s)
		}
		p.mu.Unlock()
		for _, s := range schemas {
			p.drop(t, s)
		}
		p.admin.Close()
	}
}

// The contract's isolation rests on this: a schema lent a second time holds
// nothing the first borrower wrote, and still holds what the migrations seed.
func TestSchemaPool_ReusedSchemaIsEmptyAndSeeded(t *testing.T) {
	pool := newSchemaPool(t, pgDSNFromEnv(t))
	ctx := context.Background()

	s, release := pool.fixture(t, false)
	if _, err := s.CreateSession(ctx, "t", "agent", "u"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	var first string
	for name := range pool.schemas {
		first = name
	}
	release()

	s, release = pool.fixture(t, false)
	defer release()
	if len(pool.schemas) != 1 {
		t.Fatalf("pool made %d schemas for two sequential borrowers, want 1", len(pool.schemas))
	}
	var sessions, seeded int
	if err := pool.admin.QueryRow(ctx, `SELECT count(*) FROM `+first+`.sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := pool.admin.QueryRow(ctx, `SELECT count(*) FROM `+first+`.runtime_state`).Scan(&seeded); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || seeded != 1 {
		t.Fatalf("reused schema holds %d sessions and %d runtime_state rows, want 0 and 1", sessions, seeded)
	}
	if _, err := s.CreateSession(ctx, "t", "agent", "u"); err != nil {
		t.Fatalf("CreateSession on the reused schema: %v", err)
	}
}
