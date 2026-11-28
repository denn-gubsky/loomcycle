package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// RFC HH §5 (D4): a workspace's row records the toolchains it was opened
// with, and a template saved from it requires those, not every toolchain its
// worker happens to carry.

// richWorker is a worker carrying more toolchains than a workspace needs.
var richWorker = []string{"python", "node", "go", "cpp"}

// openWith opens name for o, asking for toolchains, and writes a file so a
// checkpoint of it has something to keep.
func (f *fixture) openWith(t *testing.T, o Owner, name string, toolchains []string) store.WorkspaceRow {
	t.Helper()
	r, err := f.m.Open(context.Background(), o, OpenRequest{Name: name, Toolchains: toolchains})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(f.h.Dir(r.ID), "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) saveTemplate(t *testing.T, o Owner, src store.WorkspaceRow, as string) store.WorkspaceTemplateRow {
	t.Helper()
	ctx := context.Background()
	ck, err := f.m.Checkpoint(ctx, src, "ready")
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := f.m.SaveTemplate(ctx, o, src, ck.ID, as, store.WorkspaceTemplateUser)
	if err != nil {
		t.Fatalf("save template %s: %v", as, err)
	}
	return tpl
}

func newToolchainFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, Limits{MaxPerRun: 8}, worker.Options{Toolchains: richWorker})
	f.m.SetTemplateDir(filepath.Join(t.TempDir(), "templates"))
	return f
}

func TestTemplate_RequiresOnlyTheToolchainsItsSourceWasOpenedWith(t *testing.T) {
	f := newToolchainFixture(t)
	alice := f.run(t, "alice")
	src := f.openWith(t, alice, "build", []string{"go"})
	if !slices.Equal(src.Toolchains, []string{"go"}) {
		t.Fatalf("the opened row's toolchains = %#v, want [go]", src.Toolchains)
	}
	if got, err := f.st.WorkspaceGet(context.Background(), "acme", src.ID); err != nil || !slices.Equal(got.Toolchains, []string{"go"}) {
		t.Fatalf("the stored row's toolchains = %#v, %v; want [go]", got.Toolchains, err)
	}
	tpl := f.saveTemplate(t, alice, src, "gobuild")
	if !slices.Equal(tpl.Toolchains, []string{"go"}) {
		t.Errorf("the template requires %#v, want only [go] (the worker carries %v)", tpl.Toolchains, richWorker)
	}
}

func TestTemplate_ALegacySourceRowFallsBackToItsWorkersToolchains(t *testing.T) {
	f := newToolchainFixture(t)
	alice := f.run(t, "alice")
	src := f.openWith(t, alice, "build", []string{"go"})
	// A row from before the column reads back with no toolchains.
	src.Toolchains = nil
	tpl := f.saveTemplate(t, alice, src, "legacy")
	// The dispatcher keeps a worker's list sorted.
	if want := slices.Sorted(slices.Values(richWorker)); !slices.Equal(tpl.Toolchains, want) {
		t.Errorf("the template requires %#v, want the worker's %v", tpl.Toolchains, want)
	}
}

func TestOpen_FromATemplateRecordsTheTemplatesToolchains(t *testing.T) {
	f := newToolchainFixture(t)
	ctx := context.Background()
	alice := f.run(t, "alice")
	tpl := f.saveTemplate(t, alice, f.openWith(t, alice, "build", []string{"go"}), "gobuild")
	later := f.run(t, "alice")
	ws, err := f.m.Open(ctx, later, OpenRequest{Name: "copy", Template: &tpl})
	if err != nil {
		t.Fatalf("open from the template: %v", err)
	}
	if !slices.Equal(ws.Toolchains, []string{"go"}) {
		t.Errorf("a workspace opened from the template records %#v, want [go]", ws.Toolchains)
	}
	// A template saved from it requires the same, not the worker's list.
	if again := f.saveTemplate(t, later, ws, "gobuild2"); !slices.Equal(again.Toolchains, []string{"go"}) {
		t.Errorf("a template of the copy requires %#v, want [go]", again.Toolchains)
	}
}

func TestFork_KeepsItsSourcesToolchains(t *testing.T) {
	f := newToolchainFixture(t)
	alice := f.run(t, "alice")
	src := f.openWith(t, alice, "build", []string{"go"})
	fork, _, err := f.m.Fork(context.Background(), alice, src, "copy", "")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if !slices.Equal(fork.Toolchains, []string{"go"}) {
		t.Errorf("the fork records %#v, want its source's [go]", fork.Toolchains)
	}
	legacy := src
	legacy.Toolchains = nil
	fork2, _, err := f.m.Fork(context.Background(), alice, legacy, "copy2", "")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if fork2.Toolchains != nil {
		t.Errorf("the fork of a legacy row records %#v, want nil (unknown)", fork2.Toolchains)
	}
}

func TestOpen_ADurableWorkspaceKeepsItsToolchainsAcrossAPark(t *testing.T) {
	f := newToolchainFixture(t)
	f.m.SetDurable(DurableLimits{Enabled: true, MaxPerUser: 8})
	ctx := context.Background()
	alice := f.run(t, "alice")
	r, err := f.m.Open(ctx, alice, OpenRequest{Name: "keep", Toolchains: []string{"go"}, Lifetime: store.WorkspaceLifetimeDurable})
	if err != nil {
		t.Fatalf("open durable: %v", err)
	}
	if !slices.Equal(r.Toolchains, []string{"go"}) {
		t.Fatalf("the durable row records %#v, want [go]", r.Toolchains)
	}
	if err := f.m.Release(ctx, r); err != nil {
		t.Fatalf("park: %v", err)
	}
	later := f.run(t, "alice")
	again, err := f.m.Open(ctx, later, OpenRequest{Name: "keep", Toolchains: []string{"go"}, Lifetime: store.WorkspaceLifetimeDurable})
	if err != nil || again.ID != r.ID {
		t.Fatalf("open the parked workspace again: %v (id %s, want %s)", err, again.ID, r.ID)
	}
	if !slices.Equal(again.Toolchains, []string{"go"}) {
		t.Errorf("the re-pinned row records %#v, want [go]", again.Toolchains)
	}
}

func TestRootOpen_RecordsNoToolchains(t *testing.T) {
	f, _, _ := rootFixture(t)
	r, err := f.m.Open(context.Background(), f.run(t, "alice"), OpenRequest{Name: "src", Root: "code"})
	if err != nil {
		t.Fatalf("root open: %v", err)
	}
	if r.Toolchains != nil {
		t.Errorf("a root records toolchains %#v, want none", r.Toolchains)
	}
}
