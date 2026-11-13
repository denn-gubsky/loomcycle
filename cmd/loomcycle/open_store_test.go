package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A Postgres backend that was only inferred from a DSN must not be opened over
// a data directory that already holds a SQLite store: the instance would come
// up empty on the other database. The refusal comes before any connection and
// never carries the DSN.
func TestOpenStore_RefusesAnInferredPostgresOverAnExistingSQLiteStore(t *testing.T) {
	const dsn = "postgres://someone:hunter2@127.0.0.1:1/x"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "loomcycle.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Env.DataDir = dir
	cfg.Storage.Backend = "postgres"
	cfg.Storage.BackendInferred = true
	cfg.Storage.PgDSN = dsn

	for _, server := range []bool{true, false} {
		_, _, err := openStore(cfg, server)
		if err == nil {
			t.Fatalf("server=%v: openStore opened an inferred Postgres over an existing SQLite store", server)
		}
		msg := err.Error()
		for _, want := range []string{"SQLite store", "storage.backend", "sqlite", "postgres", "migrate sqlite-to-postgres"} {
			if !strings.Contains(msg, want) {
				t.Errorf("server=%v: refusal does not say %q: %s", server, want, msg)
			}
		}
		if strings.Contains(msg, "hunter2") || strings.Contains(msg, "someone") || strings.Contains(msg, "postgres open") {
			t.Errorf("server=%v: refusal carries the DSN or came from a connection attempt: %s", server, msg)
		}
	}

	// Named in the yaml, or with no SQLite store in the data directory, the
	// choice is the operator's and nothing is refused here.
	cfg.Storage.BackendInferred = false
	if err := inferredPostgresOverSQLite(cfg); err != nil {
		t.Errorf("a backend named in the yaml was refused: %v", err)
	}
	cfg.Storage.BackendInferred = true
	cfg.Env.DataDir = t.TempDir()
	if err := inferredPostgresOverSQLite(cfg); err != nil {
		t.Errorf("an empty data directory was refused: %v", err)
	}
}

// The boot line names the backend and where the choice came from.
func TestStoreBackendSource_SaysInferredOrYAML(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.BackendInferred = true
	if got := storeBackendSource(cfg); got != "inferred" {
		t.Errorf("source = %q, want inferred", got)
	}
	cfg.Storage.BackendInferred = false
	if got := storeBackendSource(cfg); got != "yaml" {
		t.Errorf("source = %q, want yaml", got)
	}
}
