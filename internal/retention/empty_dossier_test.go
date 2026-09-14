package retention

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC CX-5 — the empty-dossier family, at the sweeper level.
//
// The decision this encodes is that an empty dossier is KEPT: it records that the
// entity was once known, and it usually becomes empty as a side effect of an
// unrelated user's erasure. So the family is off by default and the off-by-default
// case is a test, not a comment.

// seedEmptyDossier writes a subject document with an entity root and no facts, in
// the user scope of `tenant`, through the real Document tool.
func seedEmptyDossier(t *testing.T, st *sqlite.Store, sm *sqlmem.Manager, tenant, user, subject string) (sqlmem.ScopeKey, string) {
	t.Helper()
	d := &builtin.Document{Store: st, SqlMem: sm}
	ctx := tools.WithAgentName(context.Background(), "seed")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: tenant, UserID: user, AgentID: "a_seed"})

	res, err := d.Execute(ctx, json.RawMessage(`{"op":"create_document","scope":"user",
		"title":"`+subject+`","path":"/facts/`+subject+`"}`))
	if err != nil || res.IsError {
		t.Fatalf("create_document: %v %s", err, res.Text)
	}
	var out struct {
		DocumentID  string `json:"document_id"`
		RootChunkID string `json:"root_chunk_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.DocumentID == "" {
		t.Fatalf("decode create_document: %v %s", err, res.Text)
	}
	key := sqlmem.ScopeKey{Tenant: tenant, Scope: "user", ScopeID: user}
	// The sidecar on the ROOT is what makes this a dossier rather than prose.
	if _, err := sm.Exec(context.Background(), key,
		`INSERT INTO chunk_memory_meta (chunk_id, natural_key, valid_at, created_at, class)
		 VALUES (?, ?, ?, ?, ?)`,
		[]any{out.RootChunkID, "person:" + subject, 1, 1, "derived"}, 0); err != nil {
		t.Fatalf("seed entity sidecar: %v", err)
	}
	return key, out.DocumentID
}

func documentExists(t *testing.T, sm *sqlmem.Manager, key sqlmem.ScopeKey, docID string) bool {
	t.Helper()
	res, err := sm.Query(context.Background(), key, `SELECT COUNT(*) FROM documents WHERE id = ?`, []any{docID})
	if err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if len(res.Rows) == 0 {
		return false
	}
	switch v := res.Rows[0][0].(type) {
	case int64:
		return v > 0
	case float64:
		return v > 0
	}
	return false
}

// TestSweeper_EmptyDossierOffByDefault. The default is the DECISION — an empty
// dossier is a record, and a retention subsystem must not quietly drop what it
// holds. A regression that made this family fire unconfigured would delete entity
// records on every deployment that upgraded.
func TestSweeper_EmptyDossierOffByDefault(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)
	key, docID := seedEmptyDossier(t, st, sm, "acme", "u1", "mel")

	s := New(st, Config{SQLMem: sm, ChunkPruner: &builtin.Document{Store: st, SqlMem: sm},
		Logger: quietLogger, Now: futureHour}) // no EmptyDossierMode at all
	res, err := s.sweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if res.EmptyDossiers != 0 {
		t.Errorf("the family reported %d removals while unconfigured", res.EmptyDossiers)
	}
	if !documentExists(t, sm, key, docID) {
		t.Error("an empty dossier was deleted with the family OFF — the default is the decision")
	}
}

// TestSweeper_EmptyDossierPrunesWhenEnabled, and reports the count so
// GET /v1/_retention can show it.
func TestSweeper_EmptyDossierPrunesWhenEnabled(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)
	key, docID := seedEmptyDossier(t, st, sm, "acme", "u1", "mel")

	s := New(st, Config{EmptyDossierMode: "prune", SQLMem: sm,
		ChunkPruner: &builtin.Document{Store: st, SqlMem: sm}, Logger: quietLogger, Now: futureHour})
	res, err := s.sweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if res.EmptyDossiers != 1 {
		t.Errorf("Result.EmptyDossiers = %d, want 1 — the count is what the report reads", res.EmptyDossiers)
	}
	if documentExists(t, sm, key, docID) {
		t.Error("the empty dossier survived a sweep with the family enabled")
	}
}

// TestSweeper_EmptyDossierDryRunDeletesNothing.
func TestSweeper_EmptyDossierDryRunDeletesNothing(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)
	key, docID := seedEmptyDossier(t, st, sm, "acme", "u1", "mel")

	s := New(st, Config{EmptyDossierMode: "prune", DryRun: true, SQLMem: sm,
		ChunkPruner: &builtin.Document{Store: st, SqlMem: sm}, Logger: quietLogger, Now: futureHour})
	res, err := s.sweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if res.EmptyDossiers != 1 {
		t.Errorf("dry run reported %d, want the would-remove count 1", res.EmptyDossiers)
	}
	if !documentExists(t, sm, key, docID) {
		t.Error("the dry run deleted the dossier")
	}
}

// TestSweeper_EmptyDossierExportsTheRecordBeforeRemovingIt. The dossier IS the
// record — "this entity was once known" — so export+prune has to carry the entity's
// name out, or the mode preserves nothing worth having.
func TestSweeper_EmptyDossierExportsTheRecordBeforeRemovingIt(t *testing.T) {
	dir := t.TempDir()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)
	key, docID := seedEmptyDossier(t, st, sm, "acme", "u1", "mel")

	s := New(st, Config{EmptyDossierMode: "export+prune", ExportDir: dir, SQLMem: sm,
		ChunkPruner: &builtin.Document{Store: st, SqlMem: sm}, Logger: quietLogger, Now: futureHour})
	if _, err := s.sweepOnce(context.Background()); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if documentExists(t, sm, key, docID) {
		t.Error("the dossier survived export+prune")
	}
	b, err := os.ReadFile(filepath.Join(dir, "agents", futureHour().UTC().Format("2006-01-02"),
		"empty-dossiers", "acme__user__u1.json"))
	if err != nil {
		t.Fatalf("no empty-dossier export was written before the removal: %v", err)
	}
	if !strings.Contains(string(b), "person:mel") {
		t.Errorf("the export does not name the entity that stopped being known: %s", b)
	}
}

// TestSweeper_EmptyDossierExportPlusPruneIsOffWithoutAnExportDir — the family
// convention: never delete something the operator asked to have exported when there
// is nowhere to export it to.
func TestSweeper_EmptyDossierExportPlusPruneIsOffWithoutAnExportDir(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)
	key, docID := seedEmptyDossier(t, st, sm, "acme", "u1", "mel")

	s := New(st, Config{EmptyDossierMode: "export+prune", SQLMem: sm, // no ExportDir
		ChunkPruner: &builtin.Document{Store: st, SqlMem: sm}, Logger: quietLogger, Now: futureHour})
	if _, err := s.sweepOnce(context.Background()); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if !documentExists(t, sm, key, docID) {
		t.Error("export+prune deleted a dossier with nowhere to export it to")
	}
}

// TestSweeper_EmptyDossierIndependentOfTheOtherFamilies: enabling it must not drag
// in the content prune or the retired-agent reclaim, which is the property every
// family in this sweeper is documented to have.
func TestSweeper_EmptyDossierIndependentOfTheOtherFamilies(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)
	seedEmptyDossier(t, st, sm, "acme", "u1", "mel")
	p := &fakePruner{}

	s := New(st, Config{EmptyDossierMode: "prune", SQLMem: sm, ChunkPruner: p,
		Logger: quietLogger, Now: futureHour})
	if _, err := s.sweepOnce(context.Background()); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if len(p.calls) != 0 {
		t.Errorf("the content prune ran %d times with only the dossier family enabled", len(p.calls))
	}
	if len(p.scopeCalls) != 0 {
		t.Errorf("the whole-scope reclaim ran %d times with only the dossier family enabled", len(p.scopeCalls))
	}
	if len(p.dossierCalls) == 0 {
		t.Error("the dossier family did not run at all")
	}
	for _, ms := range p.dossierDry {
		if ms {
			t.Error("a non-dry-run sweep asked for a dry run")
		}
	}
	_ = store.MemoryScopeUser
}
