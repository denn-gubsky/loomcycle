package retention

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fakePruner records what it was asked to prune.
type fakePruner struct {
	calls   []sqlmem.ScopeKey
	scopes  []store.MemoryScope
	cutoffs []int64
	dry     []bool
	n       int
}

func (f *fakePruner) PruneRetiredChunks(_ context.Context, key sqlmem.ScopeKey, ms store.MemoryScope, cutoff int64, dryRun bool) (int, error) {
	f.calls = append(f.calls, key)
	f.scopes = append(f.scopes, ms)
	f.cutoffs = append(f.cutoffs, cutoff)
	f.dry = append(f.dry, dryRun)
	return f.n, nil
}

// fakeSQLMem lists a fixed scope set.
type fakeSQLMem struct{ scopes []sqlmem.ScopeKey }

func (f *fakeSQLMem) ListScopes(context.Context) ([]sqlmem.ScopeKey, error) { return f.scopes, nil }
func (f *fakeSQLMem) ExportScope(context.Context, sqlmem.ScopeKey) (*sqlmem.ScopeDump, error) {
	return &sqlmem.ScopeDump{}, nil
}
func (f *fakeSQLMem) DropScope(context.Context, sqlmem.ScopeKey) (bool, error) { return false, nil }

// TestMemContent_OffByDefault: every destructive retention family is opt-in, and
// this one deletes content out of LIVE scopes rather than reclaiming dead ones — so
// a deployment that upgrades and says nothing must lose nothing.
func TestMemContent_OffByDefault(t *testing.T) {
	p := &fakePruner{n: 3}
	s := New(nil, Config{ChunkPruner: p, SQLMem: &fakeSQLMem{}})
	if s.memContentEnabled() {
		t.Error("the mem_content family must be OFF unless a mode is configured")
	}
	for _, mode := range []string{"", "off", "nonsense"} {
		s := New(nil, Config{MemContentMode: mode, ChunkPruner: p, SQLMem: &fakeSQLMem{}})
		if s.memContentEnabled() {
			t.Errorf("mode %q must not enable the family", mode)
		}
	}
	if len(p.calls) != 0 {
		t.Errorf("nothing should have been pruned; got %d calls", len(p.calls))
	}
}

// TestMemContent_DisabledWithoutAPruner: a nil pruner means there is no Document
// tool wired, so there is no entity content to prune. Reporting the family enabled
// while doing nothing would be worse than reporting it off.
func TestMemContent_DisabledWithoutAPruner(t *testing.T) {
	s := New(nil, Config{MemContentMode: "prune", SQLMem: &fakeSQLMem{}})
	if s.memContentEnabled() {
		t.Error("no pruner wired → the family must report disabled")
	}
	s2 := New(nil, Config{MemContentMode: "prune", ChunkPruner: &fakePruner{}})
	if s2.memContentEnabled() {
		t.Error("no SQL Memory → the family must report disabled")
	}
}

// TestMemContent_WalksEveryDurableScope: retired content lives in whatever scope
// wrote it, INCLUDING a tenant scope shared by many agents. An agent-keyed walk
// would miss exactly the shared graph the entity tier exists for.
func TestMemContent_WalksEveryDurableScope(t *testing.T) {
	scopes := []sqlmem.ScopeKey{
		{Tenant: "acme", Scope: "agent", ScopeID: "curator"},
		{Tenant: "acme", Scope: "user", ScopeID: "alice"},
		{Tenant: "acme", Scope: "tenant", ScopeID: "acme"},
		// A run scope has no durable body partition and must be SKIPPED, not guessed
		// at — pruning bodies out of the wrong partition would delete another
		// scope's text.
		{Tenant: "acme", Scope: "run", ScopeID: "r_123"},
	}
	p := &fakePruner{n: 2}
	s := New(nil, Config{MemContentMode: "prune", MemContentMaxAge: time.Hour,
		ChunkPruner: p, SQLMem: &fakeSQLMem{scopes: scopes}})

	n, err := s.sweepMemContentOnce(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(p.calls) != 3 {
		t.Fatalf("want 3 durable scopes pruned (run skipped), got %d: %+v", len(p.calls), p.calls)
	}
	if n != 6 {
		t.Errorf("the sweep should total each scope's count (3 x 2), got %d", n)
	}
	wantScopes := map[store.MemoryScope]bool{
		store.MemoryScopeAgent: false, store.MemoryScopeUser: false, store.MemoryScopeTenant: false,
	}
	for _, ms := range p.scopes {
		if _, known := wantScopes[ms]; !known {
			t.Errorf("pruned an unexpected memory scope %q", ms)
		}
		wantScopes[ms] = true
	}
	for ms, seen := range wantScopes {
		if !seen {
			t.Errorf("scope %q was never pruned", ms)
		}
	}
}

// TestMemContent_CutoffIsDerivedFromMaxAge: the age setting has to reach the pruner
// as an absolute instant, or every retired row would go on the first sweep.
func TestMemContent_CutoffIsDerivedFromMaxAge(t *testing.T) {
	fixed := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	p := &fakePruner{}
	s := New(nil, Config{MemContentMode: "prune", MemContentMaxAge: 24 * time.Hour,
		ChunkPruner: p, SQLMem: &fakeSQLMem{scopes: []sqlmem.ScopeKey{
			{Tenant: "acme", Scope: "user", ScopeID: "alice"},
		}}})
	s.now = func() time.Time { return fixed }

	if _, err := s.sweepMemContentOnce(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(p.cutoffs) != 1 {
		t.Fatalf("want 1 call, got %d", len(p.cutoffs))
	}
	want := fixed.Add(-24 * time.Hour).UnixNano()
	if p.cutoffs[0] != want {
		t.Errorf("cutoff = %d, want %d (now minus max age)", p.cutoffs[0], want)
	}
}

// TestMemContent_DryRunPropagates: an operator sizing a destructive policy must get
// counts without deletions, and the flag has to reach the pruner to do that.
func TestMemContent_DryRunPropagates(t *testing.T) {
	p := &fakePruner{n: 5}
	s := New(nil, Config{MemContentMode: "prune", DryRun: true, ChunkPruner: p,
		SQLMem: &fakeSQLMem{scopes: []sqlmem.ScopeKey{{Tenant: "acme", Scope: "user", ScopeID: "alice"}}}})
	if _, err := s.sweepMemContentOnce(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(p.dry) != 1 || !p.dry[0] {
		t.Errorf("DryRun did not reach the pruner: %v", p.dry)
	}
}
