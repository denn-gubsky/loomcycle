package snapshot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// sqlmem_test.go — RFC AA Phase 3e: end-to-end snapshot integration for the SQL
// Memory facet. Uses a REAL sqlite-backed *sqlmem.Manager (no external DB) as
// the SqlMemSnapshotter so the full envelope path (capture → JSON → restore) is
// exercised, plus the tier-skip + disabled/back-compat carve-outs.

func newSqlMemManager(t *testing.T) *sqlmem.Manager {
	t.Helper()
	m, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// TestSnapshot_SqlMemRoundTrip: a populated scope survives Capture (with a
// manager) → envelope JSON → Restore (into a fresh manager + store).
func TestSnapshot_SqlMemRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, cleanup := newTestStore(t)
	defer cleanup()
	src := newSqlMemManager(t)
	key := sqlmem.ScopeKey{Tenant: "t1", Scope: "agent", ScopeID: "a1"}

	if _, err := src.Exec(ctx, key, "CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := src.Exec(ctx, key, "INSERT INTO notes (body) VALUES (?)", []any{"hi"}, 0); err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, jsonBytes, err := Capture(ctx, s, CaptureOptions{SqlMem: src})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !strings.Contains(string(jsonBytes), `"sqlmem"`) {
		t.Fatal("envelope is missing the sqlmem section")
	}

	s2, cleanup2 := newTestStore(t)
	defer cleanup2()
	dst := newSqlMemManager(t)
	res, err := Restore(ctx, s2, jsonBytes, RestoreOptions{SqlMem: dst})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.SqlMemScopesRestored != 1 {
		t.Fatalf("SqlMemScopesRestored = %d, want 1 (warnings: %v)", res.SqlMemScopesRestored, res.Warnings)
	}
	q, err := dst.Query(ctx, key, "SELECT body FROM notes", nil)
	if err != nil {
		t.Fatalf("Query restored: %v", err)
	}
	if len(q.Rows) != 1 {
		t.Fatalf("restored %d rows, want 1", len(q.Rows))
	}
	if got, _ := q.Rows[0][0].(string); got != "hi" {
		t.Fatalf("body = %q, want hi", got)
	}
}

// TestSnapshot_SqlMemAbsentWhenDisabled: no SqlMem ⇒ no section (byte-compatible
// with a pre-3e envelope).
func TestSnapshot_SqlMemAbsentWhenDisabled(t *testing.T) {
	ctx := context.Background()
	s, cleanup := newTestStore(t)
	defer cleanup()
	_, jsonBytes, err := Capture(ctx, s, CaptureOptions{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if strings.Contains(string(jsonBytes), `"sqlmem"`) {
		t.Fatalf("envelope contains a sqlmem section despite SqlMem=nil: %s", jsonBytes)
	}
}

// TestSnapshot_SqlMemCrossTierSkipped: a postgres-tier section restored into a
// sqlite manager is skipped (with a warning), not applied.
func TestSnapshot_SqlMemCrossTierSkipped(t *testing.T) {
	ctx := context.Background()
	s, cleanup := newTestStore(t)
	defer cleanup()
	src := newSqlMemManager(t)
	key := sqlmem.ScopeKey{Tenant: "t1", Scope: "agent", ScopeID: "a1"}
	if _, err := src.Exec(ctx, key, "CREATE TABLE t (id INTEGER)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, jsonBytes, err := Capture(ctx, s, CaptureOptions{SqlMem: src})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	// Re-tag the section as postgres, then re-export.
	var env Envelope
	if err := json.Unmarshal(jsonBytes, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Sections.SqlMem == nil {
		t.Fatal("captured envelope had no sqlmem section to re-tag")
	}
	env.Sections.SqlMem.Tier = "postgres"
	retagged, err := Export(&env)
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}

	s2, cleanup2 := newTestStore(t)
	defer cleanup2()
	dst := newSqlMemManager(t) // sqlite
	res, err := Restore(ctx, s2, retagged, RestoreOptions{SqlMem: dst})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.SqlMemScopesRestored != 0 {
		t.Fatalf("SqlMemScopesRestored = %d, want 0 (cross-tier must skip)", res.SqlMemScopesRestored)
	}
	if !warningsContain(res.Warnings, "tier") {
		t.Fatalf("expected a tier-mismatch warning, got: %v", res.Warnings)
	}
}

// TestSnapshot_SqlMemPresentButDisabledOnRestore: a section present in the
// archive but no manager on the restoring host ⇒ a skip warning, not an error.
func TestSnapshot_SqlMemPresentButDisabledOnRestore(t *testing.T) {
	ctx := context.Background()
	s, cleanup := newTestStore(t)
	defer cleanup()
	src := newSqlMemManager(t)
	key := sqlmem.ScopeKey{Tenant: "t1", Scope: "agent", ScopeID: "a1"}
	if _, err := src.Exec(ctx, key, "CREATE TABLE t (id INTEGER)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, jsonBytes, err := Capture(ctx, s, CaptureOptions{SqlMem: src})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	s2, cleanup2 := newTestStore(t)
	defer cleanup2()
	res, err := Restore(ctx, s2, jsonBytes, RestoreOptions{}) // no SqlMem
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.SqlMemScopesRestored != 0 {
		t.Fatalf("SqlMemScopesRestored = %d, want 0", res.SqlMemScopesRestored)
	}
	if !warningsContain(res.Warnings, "not enabled") {
		t.Fatalf("expected a 'not enabled' warning, got: %v", res.Warnings)
	}
}

func warningsContain(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
