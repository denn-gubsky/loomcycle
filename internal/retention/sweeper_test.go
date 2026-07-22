package retention

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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
}
