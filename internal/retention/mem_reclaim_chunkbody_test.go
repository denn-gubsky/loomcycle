package retention

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC CX-3 — the whole-scope reclaim held TWO notions of what a fact is, and they
// disagreed about which key a chunk body hangs from.
//
// Pass 1 drops a fully-retired agent's SQL-Memory scope per (TENANT, name): its
// chunks, edges and sidecar rows. Pass 2 drops the base-memory k/v — where the
// chunk BODIES live, as `doc.chunk:<hex>` rows — but only once the name is
// GLOBALLY dead, retired in every tenant.
//
// Those two gates are not the same gate. An agent retired in one tenant and still
// live in another therefore had its structure dropped and its bodies left behind,
// with nothing that would ever reap them: no read returns an orphaned body and no
// sweeper looks for one. The leak is invisible by construction, which is why it
// needs a test that lists the plane rather than trusting the sweeper's own count.
//
// A single-tenant deployment never sees it — "globally dead" and "dead" are the
// same statement there, so both passes fire together.

// TestSweeper_MemReclaimDropsChunkBodiesWithTheirStructure is the regression.
//
// FAILS BEFORE: the sqlmem scope is gone and the body row is still listable.
func TestSweeper_MemReclaimDropsChunkBodiesWithTheirStructure(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)

	// The same NAME in two tenants: retired in acme, still live in beta. That is
	// what splits the two gates apart.
	seedRetiredAgent(t, st, "acme", "shared", 1)
	seedLiveAgent(t, st, "beta", "shared")
	seedAgentSQLScope(t, sm, "acme", "shared")

	// Seeded through the REAL Document tool, so the chunk and its body are written
	// the way the runtime writes them. A hand-placed `doc.chunk:` row with no chunk
	// behind it would be an orphan already and would prove nothing about a reclaim.
	bodyKey := seedAgentChunk(t, st, sm, "acme", "shared", "Dave moved to Berlin in July 2023.")

	s := New(st, Config{MemMode: "prune", SQLMem: sm, ChunkPruner: newTestDocPruner(t, st, sm),
		Logger: quietLogger, Now: futureHour})
	if _, err := s.sweepMemOnce(ctx); err != nil {
		t.Fatalf("sweepMemOnce: %v", err)
	}

	// The structure went — pass 1 fired, which is what makes the leak reachable.
	scopes, err := sm.ListScopes(ctx)
	if err != nil {
		t.Fatalf("list sqlmem scopes: %v", err)
	}
	for _, k := range scopes {
		if k.Tenant == "acme" && k.ScopeID == "shared" {
			t.Fatal("pass 1 did not drop the sqlmem scope — the premise of this test is gone, re-read it")
		}
	}

	// ASSERT ON THE PLANE, not on the sweeper's count. A reclaim that reports
	// success having left rows behind is the failure this asserts against.
	rows, _, err := st.MemoryList(ctx, "acme", store.MemoryScopeAgent, "shared", "", 100)
	if err != nil {
		t.Fatalf("list base memory: %v", err)
	}
	for _, e := range rows {
		if e.Key == bodyKey {
			t.Errorf("the chunk body %q survived its own structure — nothing reads an orphaned "+
				"body and nothing reaps one, so it is leaked for the life of the deployment", bodyKey)
		}
	}
}

// TestSweeper_MemReclaimLeavesRawMemoryToTheGloballyDeadGate is the other side of
// the same boundary, and it must NOT move. Raw k/v memory is keyed on the bare
// agent name and its cross-tenant policy is deliberate; only the chunk bodies
// belong to the tenant-qualified chunk scope. Widening the reclaim to the whole
// partition would delete a retired-here-live-there agent's raw memory early.
func TestSweeper_MemReclaimLeavesRawMemoryToTheGloballyDeadGate(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)

	seedRetiredAgent(t, st, "acme", "shared", 1)
	seedLiveAgent(t, st, "beta", "shared")
	seedAgentSQLScope(t, sm, "acme", "shared")

	const rawKey = "notes/scratch"
	if err := st.MemorySet(ctx, "acme", store.MemoryScopeAgent, "shared", rawKey,
		json.RawMessage(`"a note"`), 0); err != nil {
		t.Fatalf("seed raw row: %v", err)
	}

	s := New(st, Config{MemMode: "prune", SQLMem: sm, ChunkPruner: newTestDocPruner(t, st, sm),
		Logger: quietLogger, Now: futureHour})
	if _, err := s.sweepMemOnce(ctx); err != nil {
		t.Fatalf("sweepMemOnce: %v", err)
	}

	rows, _, err := st.MemoryList(ctx, "acme", store.MemoryScopeAgent, "shared", "", 100)
	if err != nil {
		t.Fatalf("list base memory: %v", err)
	}
	var found bool
	for _, e := range rows {
		if e.Key == rawKey {
			found = true
		}
	}
	if !found {
		t.Errorf("raw memory row %q was reclaimed while the name is still live in another "+
			"tenant — that is the globally-dead gate's decision, not this phase's", rawKey)
	}
}

// TestSweeper_MemReclaimDryRunDeletesNothing. DryRun is what an operator uses to
// size a destructive mode before enabling it, so it has to be inert on BOTH planes
// — a preview that empties the k/v half is not a preview.
func TestSweeper_MemReclaimDryRunDeletesNothing(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)

	seedRetiredAgent(t, st, "acme", "dead", 1)
	seedAgentSQLScope(t, sm, "acme", "dead")
	bodyKey := seedAgentChunk(t, st, sm, "acme", "dead", "A fact worth keeping for now.")

	s := New(st, Config{MemMode: "prune", DryRun: true, SQLMem: sm,
		ChunkPruner: newTestDocPruner(t, st, sm), Logger: quietLogger, Now: futureHour})
	if _, err := s.sweepMemOnce(ctx); err != nil {
		t.Fatalf("sweepMemOnce: %v", err)
	}
	if _, err := st.MemoryGet(ctx, "acme", store.MemoryScopeAgent, "dead", bodyKey); err != nil {
		t.Errorf("dry run deleted the chunk body %q: %v", bodyKey, err)
	}
}

// TestSweeper_MemReclaimExportsBodiesBeforeDeletingThem. export+prune promises the
// content is written out before it goes. The scope dump carries the chunks and the
// sidecar but NOT the bodies — those are k/v rows.
//
// ⚠️ IT MUST USE THE TWO-TENANT SHAPE, and the first version of this test did not.
// When the name is globally dead the base-memory pass also fires, and its export
// covers the WHOLE k/v partition including `doc.chunk:` rows — so the assertion
// passed with the body export deleted, proving nothing. The export this phase adds
// is load-bearing in exactly one case: the one where the base-memory pass does NOT
// fire, which is the same case as the leak.
func TestSweeper_MemReclaimExportsBodiesBeforeDeletingThem(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)

	const factText = "Dave moved to Berlin in July 2023."
	seedRetiredAgent(t, st, "acme", "shared", 1)
	seedLiveAgent(t, st, "beta", "shared") // keeps the base-memory pass out of it
	seedAgentSQLScope(t, sm, "acme", "shared")
	bodyKey := seedAgentChunk(t, st, sm, "acme", "shared", factText)

	s := New(st, Config{MemMode: "export+prune", ExportDir: dir, SQLMem: sm,
		ChunkPruner: newTestDocPruner(t, st, sm), Logger: quietLogger, Now: futureHour})
	if _, err := s.sweepMemOnce(ctx); err != nil {
		t.Fatalf("sweepMemOnce: %v", err)
	}

	// Gone from the plane...
	if _, err := st.MemoryGet(ctx, "acme", store.MemoryScopeAgent, "shared", bodyKey); err == nil {
		t.Errorf("the chunk body %q survived export+prune", bodyKey)
	}
	// ...and the ROW THAT WAS DELETED was written out, in the export this phase adds.
	//
	// ⚠️ ASSERT ON THAT FILE, not on "the text appears somewhere under ExportDir".
	// The fact's words also reach disk through the SQL-Memory dump, because
	// `chunk_revisions.body` keeps every body a chunk ever had — so a
	// text-appears-somewhere assertion passes with this export deleted, and the
	// first version of it did. What export+prune owes is the rows it deletes.
	bodies, err := os.ReadFile(filepath.Join(dir, "agents",
		futureHour().UTC().Format("2006-01-02"), "chunk-bodies", "acme__shared.json"))
	if err != nil {
		t.Fatalf("no chunk-body export was written before the bodies were deleted: %v", err)
	}
	if !strings.Contains(string(bodies), bodyKey) {
		t.Errorf("the chunk-body export does not name the deleted row %q", bodyKey)
	}
	if !strings.Contains(string(bodies), factText) {
		t.Errorf("the chunk-body export names the row but not its text")
	}

}

// seedAgentChunk writes one real chunk into (tenant, agent, name) through the
// Document tool and returns the k/v key its body landed under. Both planes are
// written by the code that owns them, which is what makes the reclaim assertion
// meaningful.
func seedAgentChunk(t *testing.T, st *sqlite.Store, sm *sqlmem.Manager, tenant, agent, text string) string {
	t.Helper()
	d := &builtin.Document{Store: st, SqlMem: sm}
	ctx := tools.WithAgentName(context.Background(), agent)
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: tenant, AgentID: "a_seed"})

	res, err := d.Execute(ctx, json.RawMessage(`{"op":"create_document","scope":"agent","title":"Notes","path":"/n/notes"}`))
	if err != nil || res.IsError {
		t.Fatalf("create_document: %v %s", err, res.Text)
	}
	var doc struct {
		DocumentID string `json:"document_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &doc); err != nil || doc.DocumentID == "" {
		t.Fatalf("decode create_document: %v %s", err, res.Text)
	}
	res, err = d.Execute(ctx, json.RawMessage(`{"op":"create_chunk","scope":"agent","document_id":"`+
		doc.DocumentID+`","title":"A fact","body":"`+text+`"}`))
	if err != nil || res.IsError {
		t.Fatalf("create_chunk: %v %s", err, res.Text)
	}
	var chunk struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &chunk); err != nil || chunk.ID == "" {
		t.Fatalf("decode create_chunk: %v %s", err, res.Text)
	}
	key := memory.DocumentChunkKeyPrefix + chunk.ID
	// Prove the premise before the sweep: a test that asserts a row is gone must
	// first establish the row was there.
	if _, err := st.MemoryGet(ctx, tenant, store.MemoryScopeAgent, agent, key); err != nil {
		t.Fatalf("seeded chunk body is not in the k/v plane at %s: %v", key, err)
	}
	return key
}

// newTestDocPruner builds the REAL Document-tool pruner over the same two planes,
// so the cascade under test is the one that ships rather than a fake that agrees
// with whatever the sweeper happens to do. A test-only import: builtin does not
// import retention, so there is no cycle to create.
func newTestDocPruner(t *testing.T, st *sqlite.Store, sm *sqlmem.Manager) ChunkPruner {
	t.Helper()
	return &builtin.Document{Store: st, SqlMem: sm}
}
