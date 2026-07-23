package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// fakeStore embeds store.Store (nil) and overrides only the two methods the
// sweeper calls, so the sweeper's own logic (export-then-delete, dry-run,
// mode/export-dir gating) can be tested without a real backend. Any other
// interface method would panic on the nil embed — the sweeper never calls them.
type fakeStore struct {
	store.Store
	purgeable   map[string][]store.RetiredDefRef
	deleteCalls []deleteCall
}

type deleteCall struct {
	defType string
	ids     []string
}

func (f *fakeStore) ListPurgeableRetiredDefVersions(_ context.Context, defType string, _ time.Time, _, _ int) ([]store.RetiredDefRef, error) {
	return f.purgeable[defType], nil
}

func (f *fakeStore) DeleteDefVersions(_ context.Context, defType string, defIDs []string) (int, error) {
	f.deleteCalls = append(f.deleteCalls, deleteCall{defType: defType, ids: append([]string(nil), defIDs...)})
	return len(defIDs), nil
}

func agentRefs(ids ...string) []store.RetiredDefRef {
	out := make([]store.RetiredDefRef, len(ids))
	for i, id := range ids {
		out[i] = store.RetiredDefRef{
			DefType:    "agent",
			DefID:      id,
			Name:       "a",
			Version:    i + 1,
			Definition: json.RawMessage(`{"model":"x"}`),
			CreatedAt:  time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		}
	}
	return out
}

func quietLogger(string, ...any) {}

// TestSweeper_DryRunDeletesNothing: DryRun counts purgeable versions but never
// calls DeleteDefVersions.
func TestSweeper_DryRunDeletesNothing(t *testing.T) {
	fs := &fakeStore{purgeable: map[string][]store.RetiredDefRef{"agent": agentRefs("d1", "d2")}}
	sw := New(fs, Config{DefsMode: "prune", DryRun: true, Logger: quietLogger})
	res, err := sw.sweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if res.Total != 2 || res.PerType["agent"] != 2 {
		t.Errorf("res = %+v, want total 2 / agent 2", res)
	}
	if len(fs.deleteCalls) != 0 {
		t.Errorf("dry-run issued %d delete call(s), want 0", len(fs.deleteCalls))
	}
}

// TestSweeper_ExportPruneWritesFilesThenDeletes: each version is written to
// ExportDir as JSON before it is deleted.
func TestSweeper_ExportPruneWritesFilesThenDeletes(t *testing.T) {
	dir := t.TempDir()
	fs := &fakeStore{purgeable: map[string][]store.RetiredDefRef{"agent": agentRefs("d1", "d2")}}
	sw := New(fs, Config{DefsMode: "export+prune", ExportDir: dir, Logger: quietLogger})
	res, err := sw.sweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if res.Total != 2 {
		t.Errorf("res.Total = %d, want 2", res.Total)
	}
	// Files written under <dir>/2026-07-01/agent/<id>.json.
	for _, id := range []string{"d1", "d2"} {
		p := filepath.Join(dir, "2026-07-01", "agent", id+".json")
		blob, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read export %s: %v", p, err)
		}
		var ref store.RetiredDefRef
		if err := json.Unmarshal(blob, &ref); err != nil {
			t.Fatalf("export %s not valid RetiredDefRef JSON: %v", p, err)
		}
		if ref.DefID != id {
			t.Errorf("export %s has def_id %q", p, ref.DefID)
		}
	}
	// And the delete followed the export.
	if len(fs.deleteCalls) != 1 || len(fs.deleteCalls[0].ids) != 2 {
		t.Fatalf("delete calls = %+v, want one call of 2 ids", fs.deleteCalls)
	}
}

// TestSweeper_ExportPruneWithoutExportDirDisabled: export+prune with no
// ExportDir is a no-op (never delete a version we can't export).
func TestSweeper_ExportPruneWithoutExportDirDisabled(t *testing.T) {
	fs := &fakeStore{purgeable: map[string][]store.RetiredDefRef{"agent": agentRefs("d1")}}
	sw := New(fs, Config{DefsMode: "export+prune", ExportDir: "", Logger: quietLogger})
	if sw.defsPurgeEnabled() {
		t.Error("defsPurgeEnabled() = true for export+prune with empty ExportDir, want false")
	}
	res, err := sw.sweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if res.Total != 0 || len(fs.deleteCalls) != 0 {
		t.Errorf("disabled sweep purged %d / %d delete calls, want 0/0", res.Total, len(fs.deleteCalls))
	}
}

// TestSweeper_ModeOffIsNoOp: the default off mode never touches the store.
func TestSweeper_ModeOffIsNoOp(t *testing.T) {
	fs := &fakeStore{purgeable: map[string][]store.RetiredDefRef{"agent": agentRefs("d1", "d2")}}
	sw := New(fs, Config{DefsMode: "", Logger: quietLogger}) // "" → off
	res, err := sw.sweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if res.Total != 0 || len(fs.deleteCalls) != 0 {
		t.Errorf("off-mode sweep purged %d / %d delete calls, want 0/0", res.Total, len(fs.deleteCalls))
	}
}

// TestSweeper_KeepLastNAndActiveExclusionHonored drives the whole stack against
// a REAL sqlite store: seed 5 versions, retire the 4 oldest, promote a retired
// one as active, then run the sweeper with keep_last_n=1. Only the retired,
// non-active, beyond-keep-N versions must be deleted.
func TestSweeper_KeepLastNAndActiveExclusionHonored(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()

	ids := []string{"v1", "v2", "v3", "v4", "v5"}
	for _, id := range ids {
		row := store.AgentDefRow{DefID: id, Name: "agent-x", Definition: json.RawMessage(`{"model":"x"}`)}
		if _, err := st.AgentDefCreate(ctx, row); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	for _, id := range ids[:4] { // retire v1..v4; v5 stays live
		if err := st.AgentDefSetRetired(ctx, id, true); err != nil {
			t.Fatalf("retire %s: %v", id, err)
		}
	}
	// Promote a retired version (v3) as active AFTER retiring, so the pointer
	// isn't cleared — the active guard must still exclude it.
	if err := st.AgentDefSetActive(ctx, "", "agent-x", "v3", ""); err != nil {
		t.Fatalf("promote v3: %v", err)
	}

	// Now() in the future so the zero DefsMaxAge cutoff clears every row's age.
	sw := New(st, Config{
		DefsMode:      "prune",
		DefsKeepLastN: 1,
		Logger:        quietLogger,
		Now:           func() time.Time { return time.Now().Add(time.Hour) },
	})
	res, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	// Purgeable: v1, v2 (v4 kept by keep-last-N=1, v3 active, v5 live).
	if res.Total != 2 || res.PerType["agent"] != 2 {
		t.Errorf("res = %+v, want total 2 / agent 2", res)
	}
	for _, id := range []string{"v1", "v2"} {
		if _, err := st.AgentDefGet(ctx, id); err == nil {
			t.Errorf("%s still present after sweep", id)
		}
	}
	for _, id := range []string{"v3", "v4", "v5"} {
		if _, err := st.AgentDefGet(ctx, id); err != nil {
			t.Errorf("%s wrongly deleted: %v", id, err)
		}
	}
}

// TestSweeper_DryRunCountsReportsPerType: the endpoint-backing preview counts
// purgeable versions per def-type without deleting.
func TestSweeper_DryRunCountsReportsPerType(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()

	ids := []string{"v1", "v2", "v3"}
	for _, id := range ids {
		if _, err := st.AgentDefCreate(ctx, store.AgentDefRow{DefID: id, Name: "agent-y", Definition: json.RawMessage(`{"model":"x"}`)}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	for _, id := range ids { // retire all three
		if err := st.AgentDefSetRetired(ctx, id, true); err != nil {
			t.Fatalf("retire %s: %v", id, err)
		}
	}
	sw := New(st, Config{
		DefsMode:      "off", // counts are mode-independent
		DefsKeepLastN: 1,
		Logger:        quietLogger,
		Now:           func() time.Time { return time.Now().Add(time.Hour) },
	})
	counts, err := sw.DryRunCounts(ctx)
	if err != nil {
		t.Fatalf("DryRunCounts: %v", err)
	}
	// keep-last-N=1 keeps v3; v1, v2 purgeable.
	if counts["agent"] != 2 {
		t.Errorf("counts[agent] = %d, want 2", counts["agent"])
	}
	for _, dt := range []string{"skill", "webhook", "memory_backend"} {
		if counts[dt] != 0 {
			t.Errorf("counts[%s] = %d, want 0", dt, counts[dt])
		}
	}
	// No sessions seeded → the "chats" preview count is 0 (present regardless of
	// ChatsMode, mirroring the def-type counts).
	if counts["chats"] != 0 {
		t.Errorf("counts[chats] = %d, want 0", counts["chats"])
	}
}

func boolPtr(b bool) *bool { return &b }

// seedCompletedChatSession creates a session with one completed run (with an
// event) — the aged-chat unit the chats sub-sweep prunes. Returns the session id.
func seedCompletedChatSession(t *testing.T, st *sqlite.Store, agentID string) string {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "t", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, run.ID, "text", []byte(`{"t":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, ""); err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

// futureHour pins Now() an hour ahead so a just-completed session lands before
// the cutoff (Now()-ChatsMaxAge with the zero max-age used by these tests).
func futureHour() time.Time { return time.Now().Add(time.Hour) }

// TestSweeper_ChatsPinnedSessionNotPruned: the chats sub-sweep never deletes a
// PINNED session (the store excludes it), while an identical unpinned aged
// session IS pruned. Drives the whole stack against a real sqlite store.
func TestSweeper_ChatsPinnedSessionNotPruned(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()

	pinnedID := seedCompletedChatSession(t, st, "pinned-a")
	unpinnedID := seedCompletedChatSession(t, st, "unpinned-a")
	if err := st.SetSessionMeta(ctx, pinnedID, store.SessionMetaPatch{Pinned: boolPtr(true)}); err != nil {
		t.Fatalf("pin: %v", err)
	}

	sw := New(st, Config{ChatsMode: "prune", Logger: quietLogger, Now: futureHour})
	res, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if res.Chats != 1 {
		t.Errorf("res.Chats = %d, want 1 (only the unpinned session)", res.Chats)
	}
	if _, err := st.GetSession(ctx, pinnedID); err != nil {
		t.Errorf("pinned session was pruned: %v", err)
	}
	if _, err := st.GetSession(ctx, unpinnedID); err == nil {
		t.Errorf("unpinned aged session survived the prune")
	}
}

// TestSweeper_ChatsDryRunDeletesNothing: DryRun counts the aged session but never
// cascade-deletes it.
func TestSweeper_ChatsDryRunDeletesNothing(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()

	sid := seedCompletedChatSession(t, st, "dry-a")
	sw := New(st, Config{ChatsMode: "prune", DryRun: true, Logger: quietLogger, Now: futureHour})
	res, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if res.Chats != 1 {
		t.Errorf("res.Chats = %d, want 1 (counts what WOULD be pruned)", res.Chats)
	}
	if _, err := st.GetSession(ctx, sid); err != nil {
		t.Errorf("dry-run deleted the session: %v", err)
	}
}

// TestSweeper_ChatsExportPruneWritesBundleThenDeletes: each aged session is
// written to <ExportDir>/chats/<day>/<sid>.json (with runs + events) before it is
// cascade-deleted.
func TestSweeper_ChatsExportPruneWritesBundleThenDeletes(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()

	dir := t.TempDir()
	sid := seedCompletedChatSession(t, st, "exp-a")
	sw := New(st, Config{ChatsMode: "export+prune", ExportDir: dir, Logger: quietLogger, Now: futureHour})
	res, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if res.Chats != 1 {
		t.Errorf("res.Chats = %d, want 1", res.Chats)
	}
	// Bundle written under <dir>/chats/<day>/<sid>.json.
	matches, _ := filepath.Glob(filepath.Join(dir, "chats", "*", sid+".json"))
	if len(matches) != 1 {
		t.Fatalf("export glob = %v, want one chats/<day>/%s.json", matches, sid)
	}
	blob, err := os.ReadFile(matches[0])
	if err != nil ||
		!bytes.Contains(blob, []byte(sid)) ||
		!bytes.Contains(blob, []byte(`"runs"`)) ||
		!bytes.Contains(blob, []byte(`"events"`)) {
		t.Errorf("bundle missing session_id / runs / events: err=%v", err)
	}
	// Deletion followed the export.
	if _, err := st.GetSession(ctx, sid); err == nil {
		t.Errorf("session survived export+prune")
	}
}

// TestSweeper_ChatsExportPruneWithoutExportDirDisabled: export+prune with no
// ExportDir is a no-op (never delete a session we can't export).
func TestSweeper_ChatsExportPruneWithoutExportDirDisabled(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()

	sid := seedCompletedChatSession(t, st, "noexp-a")
	sw := New(st, Config{ChatsMode: "export+prune", Logger: quietLogger, Now: futureHour}) // no ExportDir
	if sw.chatsPruneEnabled() {
		t.Error("chatsPruneEnabled() = true for export+prune with empty ExportDir, want false")
	}
	res, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if res.Chats != 0 {
		t.Errorf("disabled chats sweep pruned %d, want 0", res.Chats)
	}
	if _, err := st.GetSession(ctx, sid); err != nil {
		t.Errorf("disabled sweep deleted the session: %v", err)
	}
}

// ---- RFC BM Phase 3: retired-agent memory reclamation ----

func newTestSqlMem(t *testing.T) *sqlmem.Manager {
	t.Helper()
	m, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// seedRetiredAgent creates n versions of (tenant, name) and retires all of them
// (LiveVersionCount == 0). seedLiveAgent leaves the version live.
func seedRetiredAgent(t *testing.T, st *sqlite.Store, tenant, name string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		id := tenant + "-" + name + "-v" + string(rune('1'+i))
		row := store.AgentDefRow{DefID: id, Name: name, TenantID: tenant, Definition: json.RawMessage(`{"model":"x"}`)}
		if _, err := st.AgentDefCreate(ctx, row); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if err := st.AgentDefSetRetired(ctx, id, true); err != nil {
			t.Fatalf("retire %s: %v", id, err)
		}
	}
}

func seedLiveAgent(t *testing.T, st *sqlite.Store, tenant, name string) {
	t.Helper()
	id := tenant + "-" + name + "-live"
	row := store.AgentDefRow{DefID: id, Name: name, TenantID: tenant, Definition: json.RawMessage(`{"model":"x"}`)}
	if _, err := st.AgentDefCreate(context.Background(), row); err != nil {
		t.Fatalf("create live %s: %v", id, err)
	}
}

// seedAgentSQLScope provisions the (tenant, agent, name) SQL-Memory scope with a
// table so ListScopes reports it and DropScope has something to drop.
func seedAgentSQLScope(t *testing.T, sm *sqlmem.Manager, tenant, name string) {
	t.Helper()
	key := sqlmem.ScopeKey{Tenant: tenant, Scope: "agent", ScopeID: name}
	if _, err := sm.Exec(context.Background(), key, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`, nil, 0); err != nil {
		t.Fatalf("seed sql scope %s/%s: %v", tenant, name, err)
	}
	if _, err := sm.Exec(context.Background(), key, `INSERT INTO t (v) VALUES ('x')`, nil, 0); err != nil {
		t.Fatalf("seed sql row %s/%s: %v", tenant, name, err)
	}
}

func seedAgentDirent(t *testing.T, st *sqlite.Store, tenant, name string) {
	t.Helper()
	_, err := st.DirentCreate(context.Background(), store.DirentRow{
		TenantID: tenant, Scope: "agent", ScopeID: name,
		ParentPath: "/", Name: "doc1", Kind: "document", ResourceRef: json.RawMessage(`{"document_id":"doc-1"}`),
	})
	if err != nil {
		t.Fatalf("seed dirent %s/%s: %v", tenant, name, err)
	}
}

func direntCount(t *testing.T, st *sqlite.Store, tenant, name string) int {
	t.Helper()
	rows, err := st.DirentListUnder(context.Background(), tenant, "agent", name, "/")
	if err != nil {
		t.Fatalf("dirent list %s/%s: %v", tenant, name, err)
	}
	return len(rows)
}

func sqlScopeExists(t *testing.T, sm *sqlmem.Manager, tenant, name string) bool {
	t.Helper()
	scopes, err := sm.ListScopes(context.Background())
	if err != nil {
		t.Fatalf("list scopes: %v", err)
	}
	for _, k := range scopes {
		if k.Tenant == tenant && k.Scope == "agent" && k.ScopeID == name {
			return true
		}
	}
	return false
}

func memKeyCount(t *testing.T, st *sqlite.Store, name string) int {
	t.Helper()
	entries, _, err := st.MemoryList(context.Background(), "", store.MemoryScopeAgent, name, "", 1000)
	if err != nil {
		t.Fatalf("memory list %q: %v", name, err)
	}
	return len(entries)
}

// TestSweeper_MemReclaimsRetiredAgent: a fully-retired + old agent's SQL-Memory
// scope, dirents, and base-memory k/v are all reclaimed; a live agent's are not.
func TestSweeper_MemReclaimsRetiredAgent(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)

	// Dead agent (all versions retired) in tenant "acme".
	seedRetiredAgent(t, st, "acme", "dead", 2)
	seedAgentSQLScope(t, sm, "acme", "dead")
	seedAgentDirent(t, st, "acme", "dead")
	if err := st.MemorySet(ctx, "", store.MemoryScopeAgent, "dead", "k1", json.RawMessage(`1`), 0); err != nil {
		t.Fatal(err)
	}
	// Live agent — must be untouched.
	seedLiveAgent(t, st, "acme", "alive")
	seedAgentSQLScope(t, sm, "acme", "alive")
	seedAgentDirent(t, st, "acme", "alive")
	if err := st.MemorySet(ctx, "", store.MemoryScopeAgent, "alive", "k1", json.RawMessage(`1`), 0); err != nil {
		t.Fatal(err)
	}

	sw := New(st, Config{MemMode: "prune", SQLMem: sm, Logger: quietLogger, Now: futureHour})
	res, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	// One (tenant,name) scope reclamation + one globally-dead base-memory drop = 2.
	if res.Mem != 2 {
		t.Errorf("res.Mem = %d, want 2", res.Mem)
	}
	if sqlScopeExists(t, sm, "acme", "dead") {
		t.Error("dead agent's SQL-Memory scope survived")
	}
	if n := direntCount(t, st, "acme", "dead"); n != 0 {
		t.Errorf("dead agent dirents = %d, want 0", n)
	}
	if n := memKeyCount(t, st, "dead"); n != 0 {
		t.Errorf("dead agent base memory keys = %d, want 0", n)
	}
	// Live agent fully intact.
	if !sqlScopeExists(t, sm, "acme", "alive") {
		t.Error("live agent's SQL-Memory scope was dropped")
	}
	if n := direntCount(t, st, "acme", "alive"); n != 1 {
		t.Errorf("live agent dirents = %d, want 1", n)
	}
	if n := memKeyCount(t, st, "alive"); n != 1 {
		t.Errorf("live agent base memory keys = %d, want 1", n)
	}
}

// TestSweeper_MemBaseMemoryOnlyDroppedWhenGloballyDead is the cross-tenant safety
// test: a name retired in tenant A but LIVE in tenant B keeps its base memory
// (scope_id is the bare name, shared across tenants), while tenant A's
// tenant-qualified SQL-Memory scope + dirents are still reclaimed and tenant B's
// are untouched.
func TestSweeper_MemBaseMemoryOnlyDroppedWhenGloballyDead(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)

	// "shared" is retired in tenant A, LIVE in tenant B → NOT globally dead.
	seedRetiredAgent(t, st, "A", "shared", 1)
	seedLiveAgent(t, st, "B", "shared")
	seedAgentSQLScope(t, sm, "A", "shared")
	seedAgentSQLScope(t, sm, "B", "shared")
	seedAgentDirent(t, st, "A", "shared")
	seedAgentDirent(t, st, "B", "shared")
	// Base memory is keyed by the bare name — a single partition both share.
	if err := st.MemorySet(ctx, "", store.MemoryScopeAgent, "shared", "k1", json.RawMessage(`1`), 0); err != nil {
		t.Fatal(err)
	}

	sw := New(st, Config{MemMode: "prune", SQLMem: sm, Logger: quietLogger, Now: futureHour})
	res, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	// Only tenant A's scope reclaimed (1 unit); base memory NOT dropped.
	if res.Mem != 1 {
		t.Errorf("res.Mem = %d, want 1 (tenant A scope only)", res.Mem)
	}
	if n := memKeyCount(t, st, "shared"); n != 1 {
		t.Errorf("shared base memory keys = %d, want 1 (a live tenant still uses it)", n)
	}
	if sqlScopeExists(t, sm, "A", "shared") {
		t.Error("tenant A's retired SQL-Memory scope survived")
	}
	if !sqlScopeExists(t, sm, "B", "shared") {
		t.Error("tenant B's LIVE SQL-Memory scope was wrongly dropped")
	}
	if n := direntCount(t, st, "A", "shared"); n != 0 {
		t.Errorf("tenant A dirents = %d, want 0", n)
	}
	if n := direntCount(t, st, "B", "shared"); n != 1 {
		t.Errorf("tenant B dirents = %d, want 1 (live, untouched)", n)
	}
}

// TestSweeper_MemExportThenDelete: export+prune writes the scope dump, dirents,
// and base-memory bundles before deleting.
func TestSweeper_MemExportThenDelete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)

	seedRetiredAgent(t, st, "acme", "dead", 1)
	seedAgentSQLScope(t, sm, "acme", "dead")
	seedAgentDirent(t, st, "acme", "dead")
	if err := st.MemorySet(ctx, "", store.MemoryScopeAgent, "dead", "k1", json.RawMessage(`1`), 0); err != nil {
		t.Fatal(err)
	}

	sw := New(st, Config{MemMode: "export+prune", ExportDir: dir, SQLMem: sm, Logger: quietLogger, Now: futureHour})
	if _, err := sw.sweepOnce(ctx); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}

	// Each facet wrote at least one JSON file under agents/<day>/<kind>/.
	for _, kind := range []string{"sqlmem", "dirents", "agent-memory"} {
		var found int
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
			if d != nil && !d.IsDir() && filepath.Base(filepath.Dir(p)) == kind {
				found++
			}
			return nil
		})
		if found == 0 {
			t.Errorf("export kind %q wrote no file under %s", kind, dir)
		}
	}
	// And the data is actually gone.
	if sqlScopeExists(t, sm, "acme", "dead") {
		t.Error("scope survived export+prune")
	}
	if memKeyCount(t, st, "dead") != 0 {
		t.Error("base memory survived export+prune")
	}
}

// errSQLMem is a fake SQLMemory whose ExportScope fails, to prove export-then-
// delete never drops a scope it couldn't export.
type errSQLMem struct {
	scopes  []sqlmem.ScopeKey
	dropped []sqlmem.ScopeKey
}

func (e *errSQLMem) ListScopes(context.Context) ([]sqlmem.ScopeKey, error) { return e.scopes, nil }
func (e *errSQLMem) ExportScope(context.Context, sqlmem.ScopeKey) (*sqlmem.ScopeDump, error) {
	return nil, errExportBoom
}
func (e *errSQLMem) DropScope(_ context.Context, k sqlmem.ScopeKey) (bool, error) {
	e.dropped = append(e.dropped, k)
	return true, nil
}

var errExportBoom = &boomErr{}

type boomErr struct{}

func (*boomErr) Error() string { return "boom" }

// TestSweeper_MemExportFailureSkipsDrop: an export failure on the SQL-Memory
// scope skips BOTH the scope drop and the dirent delete for that agent.
func TestSweeper_MemExportFailureSkipsDrop(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()

	seedRetiredAgent(t, st, "acme", "dead", 1)
	seedAgentDirent(t, st, "acme", "dead")

	fake := &errSQLMem{scopes: []sqlmem.ScopeKey{{Tenant: "acme", Scope: "agent", ScopeID: "dead"}}}
	sw := New(st, Config{MemMode: "export+prune", ExportDir: t.TempDir(), SQLMem: fake, Logger: quietLogger, Now: futureHour})
	res, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if res.Mem != 0 {
		t.Errorf("res.Mem = %d, want 0 (export failed → nothing reclaimed)", res.Mem)
	}
	if len(fake.dropped) != 0 {
		t.Errorf("DropScope called %d time(s) despite export failure", len(fake.dropped))
	}
	if n := direntCount(t, st, "acme", "dead"); n != 1 {
		t.Errorf("dirents = %d, want 1 (dirent delete must be skipped when the scope export failed)", n)
	}
}

// TestSweeper_MemDryRunCounts: the report preview counts reclaimable agents
// without deleting anything.
func TestSweeper_MemDryRunCounts(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()
	sm := newTestSqlMem(t)

	seedRetiredAgent(t, st, "acme", "dead", 1)
	seedAgentSQLScope(t, sm, "acme", "dead")
	if err := st.MemorySet(ctx, "", store.MemoryScopeAgent, "dead", "k1", json.RawMessage(`1`), 0); err != nil {
		t.Fatal(err)
	}
	seedLiveAgent(t, st, "acme", "alive")
	seedAgentSQLScope(t, sm, "acme", "alive")

	sw := New(st, Config{MemMode: "off", SQLMem: sm, Logger: quietLogger, Now: futureHour})
	counts, err := sw.DryRunCounts(ctx)
	if err != nil {
		t.Fatalf("DryRunCounts: %v", err)
	}
	// dead: 1 tenant scope + 1 globally-dead base memory = 2; alive not counted.
	if counts["mem"] != 2 {
		t.Errorf("counts[mem] = %d, want 2", counts["mem"])
	}
	// Nothing deleted by the preview.
	if !sqlScopeExists(t, sm, "acme", "dead") {
		t.Error("DryRunCounts dropped the scope")
	}
	if memKeyCount(t, st, "dead") != 1 {
		t.Error("DryRunCounts deleted base memory")
	}
}

// TestSweeper_MemBaseMemoryReclaimedBeyondScopeIDsCap is a regression for the
// review's finding #1: pass 2 must NOT gate on MemoryListScopeIDs (capped at the
// 200 most-recently-updated scopes). A retired agent's base memory is stale, so
// it sorts out of that window in a busy deployment; if pass 2 gated on the cap it
// would never reclaim it. Here the retired agent's memory is seeded FIRST (oldest
// updated_at) and then 200 fresher scopes are added, pushing the retired agent to
// rank 201 — outside the cap. The reclaim must still happen.
func TestSweeper_MemBaseMemoryReclaimedBeyondScopeIDsCap(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()

	// The retired agent, with the OLDEST base-memory row.
	seedRetiredAgent(t, st, "acme", "dead", 1)
	if err := st.MemorySet(ctx, "", store.MemoryScopeAgent, "dead", "k1", json.RawMessage(`1`), 0); err != nil {
		t.Fatal(err)
	}
	// 200 fresher agent-memory scopes (no defs — they exist only to fill the
	// MemoryListScopeIDs top-200 window and evict "dead" from it).
	for i := 0; i < 200; i++ {
		id := "live-" + strconv.Itoa(i)
		if err := st.MemorySet(ctx, "", store.MemoryScopeAgent, id, "k", json.RawMessage(`1`), 0); err != nil {
			t.Fatal(err)
		}
	}
	// Sanity: "dead" is NOT in the (capped) MemoryListScopeIDs window.
	ids, err := st.MemoryListScopeIDs(ctx, "", store.MemoryScopeAgent)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ids {
		if s.ScopeID == "dead" {
			t.Fatalf("test precondition broken: 'dead' is inside the %d-row cap; can't prove the bug", len(ids))
		}
	}

	sw := New(st, Config{MemMode: "prune", Logger: quietLogger, Now: futureHour})
	if _, err := sw.sweepOnce(ctx); err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if n := memKeyCount(t, st, "dead"); n != 0 {
		t.Errorf("retired agent base memory NOT reclaimed (keys=%d) — pass 2 skipped it because it fell outside the MemoryListScopeIDs cap", n)
	}
}

// TestSweeper_MemRunsBeforeDefsPurge is a regression for the review's finding #2:
// with keep_last_n=0 the defs purge and the mem reclaim run in the same tick, and
// the mem sweep keys off AgentDefListNames — so it MUST run before the defs purge,
// or the purge removes the agent's last def version first and the mem sweep can no
// longer find it, orphaning its base memory forever.
func TestSweeper_MemRunsBeforeDefsPurge(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = st.Close() }()

	seedRetiredAgent(t, st, "acme", "dead", 1) // 1 retired, non-active, old version
	if err := st.MemorySet(ctx, "", store.MemoryScopeAgent, "dead", "k1", json.RawMessage(`1`), 0); err != nil {
		t.Fatal(err)
	}

	// Defs purge (keep_last_n=0 → purges the sole retired version) AND mem reclaim
	// both enabled in the same sweep.
	sw := New(st, Config{
		DefsMode: "prune", DefsKeepLastN: 0,
		MemMode: "prune",
		Logger:  quietLogger, Now: futureHour,
	})
	res, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	// The def version is purged AND the base memory is reclaimed (mem ran first).
	if res.Total != 1 {
		t.Errorf("res.Total = %d, want 1 (the retired def version purged)", res.Total)
	}
	if n := memKeyCount(t, st, "dead"); n != 0 {
		t.Errorf("base memory NOT reclaimed (keys=%d) — the defs purge removed the agent before the mem sweep saw it", n)
	}
}
