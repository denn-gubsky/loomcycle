package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// `migrate up --config` runs as the role that owns the schema, so it is where
// an operator whose server role may not rebuild memory_embeddings applies
// memory.fulltext. Without it the setting could only take effect at a boot.
func TestRunMigrate_UpAppliesTheFullTextSetting(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	ctx := context.Background()
	schema := fmt.Sprintf("clim_fulltext_%d", time.Now().UnixNano())
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Closed by Cleanup, not defer: the DROP SCHEMA cleanup below runs after
	// this function returns and needs the pool (cleanups run last-in first).
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })

	// public stays on the path after the per-test schema: pgvector lives
	// there, and without it migration 0017 skips memory_embeddings.
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	scoped := dsn + sep + "search_path=" + schema + "%2Cpublic"

	t.Setenv("LOOMCYCLE_PG_DSN", "")
	yamlPath := writeTempConfig(t, `
defaults: { provider: anthropic, model: claude-sonnet-4-6 }
agents:
  default:
    system_prompt: "x"
    tools: []
storage:
  backend: postgres
  pg_dsn: "`+scoped+`"
memory:
  fulltext:
    config: russian
`)
	var stdout, stderr bytes.Buffer
	if code := RunMigrate([]string{"up", "--config", yamlPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("migrate up: exit %d, stderr %q", code, stderr.String())
	}

	pool, err := pgxpool.New(ctx, scoped)
	if err != nil {
		t.Fatalf("dial scoped: %v", err)
	}
	defer pool.Close()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('memory_embeddings') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Skip("test Postgres has no pgvector, so no memory_embeddings and no word leg to set")
	}
	var comment *string
	if err := pool.QueryRow(ctx, `SELECT col_description('memory_embeddings'::regclass,
		(SELECT attnum FROM pg_attribute WHERE attrelid = 'memory_embeddings'::regclass AND attname = 'embed_text_tsv'))`).Scan(&comment); err != nil {
		t.Fatal(err)
	}
	if comment == nil || !strings.Contains(*comment, "config=russian") {
		t.Fatalf("migrate up did not apply memory.fulltext: column marker %v, stdout %q", comment, stdout.String())
	}
}
