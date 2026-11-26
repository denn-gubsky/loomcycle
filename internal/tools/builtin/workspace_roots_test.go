package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// RFC GW P13 (D-P13-3, owner ruling Q1): opening a workspace on a root, the
// user's own folder on their personal worker, needs workspace.roots beside
// open.

func rootsFixture(t *testing.T) (*wsFixture, string) {
	t.Helper()
	f := newWSFixture(t, lifecycle.Limits{})
	user := t.TempDir()
	if err := os.WriteFile(filepath.Join(user, "notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.h.AddPersonalWorker("alice", []worker.Root{{Name: "code", Path: user}})
	return f, user
}

// Without workspace.roots a root open is refused before anything is
// placed; with it the workspace is the user's folder, list and status say
// which root, and release leaves the folder.
func TestRootOpen_NeedsTheRootsGrant(t *testing.T) {
	f, user := rootsFixture(t)
	noRoots := f.runCtx(t, "alice", "", wsFull)
	if res := f.call(t, noRoots, `{"op":"open","name":"src","root":"code"}`); refusalClass(res) != "not_granted" {
		t.Fatalf("open without the grant: %s", res.Text)
	}
	pol := wsFull
	pol.Roots = true
	ctx := f.runCtx(t, "alice", "", pol)
	res := f.mustOK(t, ctx, `{"op":"open","name":"src","root":"code"}`)
	if !strings.Contains(res.Text, `"root":"code"`) {
		t.Errorf("open answer %s does not name the root", res.Text)
	}
	if res := f.mustOK(t, ctx, `{"op":"read","name":"src","path":"notes.txt"}`); !strings.Contains(res.Text, "hello") {
		t.Errorf("read the user's file: %s", res.Text)
	}
	for _, in := range []string{`{"op":"list"}`, `{"op":"status","name":"src"}`} {
		if res := f.mustOK(t, ctx, in); !strings.Contains(res.Text, `"root":"code"`) {
			t.Errorf("%s: %s, want the root named", in, res.Text)
		}
	}
	for _, in := range []string{`{"op":"checkpoint","name":"src"}`, `{"op":"fork","name":"src","as":"copy"}`} {
		if res := f.call(t, ctx, in); refusalClass(res) != "not_on_root" {
			t.Errorf("%s: %s, want not_on_root", in, res.Text)
		}
	}
	f.mustOK(t, ctx, `{"op":"release","name":"src"}`)
	if _, err := os.Stat(filepath.Join(user, "notes.txt")); err != nil {
		t.Fatalf("the user's file after release: %v", err)
	}
	// repo or root, never both: a root is never a git workspace.
	if res := f.call(t, ctx, `{"op":"open","name":"g","root":"code","repo":"https://example.com/r.git"}`); refusalClass(res) != "invalid_argument" || !strings.Contains(res.Text, "repo or root") {
		t.Errorf("repo and root together: %s", res.Text)
	}
	if res := f.call(t, ctx, `{"op":"open","name":"x","root":"code","toolchain":"python"}`); refusalClass(res) != "invalid_argument" {
		t.Errorf("root with a toolchain: %s", res.Text)
	}
}

// A child holds roots only when it and its parent both do: narrowing never
// lets a child without the grant open its parent's user's folder.
func TestRootOpen_ChildWithoutTheGrantCannotOpenTheParentsRoot(t *testing.T) {
	parent := tools.WorkspacePolicyValue{Read: true, Open: true, Roots: true}
	for name, c := range map[string]struct {
		parent tools.WorkspacePolicyValue
		own    *config.AgentWorkspace
		want   bool
	}{
		"child without":      {parent, &config.AgentWorkspace{Ops: []string{"read"}, Open: true}, false},
		"parent without":     {tools.WorkspacePolicyValue{Read: true, Open: true}, &config.AgentWorkspace{Ops: []string{"read"}, Open: true, Roots: true}, false},
		"both":               {parent, &config.AgentWorkspace{Ops: []string{"read"}, Open: true, Roots: true}, true},
		"child no workspace": {parent, nil, false},
	} {
		if got := tools.NarrowWorkspace(c.parent, tools.WorkspaceFor(c.own)).Roots; got != c.want {
			t.Errorf("%s: roots = %v, want %v", name, got, c.want)
		}
	}
	// The policy carries it from the definition and back.
	v := tools.WorkspaceFor(&config.AgentWorkspace{Open: true, Roots: true})
	if !v.Roots || !tools.WorkspaceFor(v.Grant()).Roots || v.Report()["roots"] != true {
		t.Errorf("roots does not ride the policy: %+v", v)
	}
}
