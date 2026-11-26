package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/dispatch"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// RFC GW P13: a workspace opened on a root is the user's own folder, on
// that user's personal worker.

// rootFixture is a fixture with alice's personal worker offering root
// "code", a folder holding keep.txt.
func rootFixture(t *testing.T) (f *fixture, user, personal string) {
	t.Helper()
	f = newFixture(t, Limits{MaxPerRun: 8}, worker.Options{})
	user = t.TempDir()
	if err := os.WriteFile(filepath.Join(user, "keep.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	personal = f.h.AddPersonalWorker("alice", []worker.Root{{Name: "code", Path: user}})
	return f, user, personal
}

// A root open lands on the user's own personal worker and records the root
// on the row; another user's root open finds no worker, and an ordinary
// open never lands on a personal worker. Release leaves the folder.
func TestRootOpen_IsTheUsersFolderOnTheirOwnWorker(t *testing.T) {
	f, user, personal := rootFixture(t)
	ctx := context.Background()
	alice := f.run(t, "alice")
	r, err := f.m.Open(ctx, alice, OpenRequest{Name: "src", Root: "code"})
	if err != nil {
		t.Fatalf("root open: %v", err)
	}
	if r.WorkerID != personal || r.Root != "code" {
		t.Fatalf("row = worker %q root %q, want %s code", r.WorkerID, r.Root, personal)
	}
	res, err := f.m.Do(ctx, r, readReq("keep.txt"))
	if err != nil || string(res.Output) == "" {
		t.Fatalf("read the user's file: %v", err)
	}
	if _, err := f.m.Open(ctx, f.run(t, "bob"), OpenRequest{Name: "src", Root: "code"}); err == nil {
		t.Error("bob's root open was placed")
	}
	plain, err := f.m.Open(ctx, alice, OpenRequest{Name: "scratch", Toolchains: []string{"python", "node"}})
	if err != nil || strings.HasPrefix(plain.WorkerID, "user:") {
		t.Errorf("an ordinary open = %q, %v; want the tenant worker", plain.WorkerID, err)
	}
	if err := f.m.Release(ctx, r); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(user, "keep.txt")); err != nil {
		t.Fatalf("the user's file after release: %v", err)
	}
}

// Ops that copy, keep or hand over the user's folder are refused on a root
// row: checkpoints, restore, fork, save_template and share (D-P13-14, deny
// by default). Over the MCP server a root is not opened at all (D-P13-9),
// and a root does not go with a template, a toolchain or durable.
func TestRootOpen_RefusesWhatARootDoesNotTake(t *testing.T) {
	f, _, _ := rootFixture(t)
	ctx := context.Background()
	alice := f.run(t, "alice")
	r, err := f.m.Open(ctx, alice, OpenRequest{Name: "src", Root: "code"})
	if err != nil {
		t.Fatalf("root open: %v", err)
	}
	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrRootRefused) {
			t.Errorf("%s on a root: %v, want ErrRootRefused", what, err)
		}
	}
	_, err = f.m.Checkpoint(ctx, r, "x")
	refused("checkpoint", err)
	_, err = f.m.Checkpoints(ctx, r)
	refused("checkpoints", err)
	_, err = f.m.Restore(ctx, r, "ck_0123456789abcdef")
	refused("restore", err)
	_, err = f.m.DeleteCheckpoint(ctx, r, "ck_0123456789abcdef")
	refused("delete_checkpoint", err)
	_, _, err = f.m.Fork(ctx, alice, r, "copy", "")
	refused("fork", err)
	_, err = f.m.SaveTemplate(ctx, alice, r, "", "tpl", store.WorkspaceTemplateUser)
	refused("save_template", err)
	_, err = f.m.Share(ctx, alice, "src", Peer{RunID: "r_y", AgentID: "ag_y"}, store.WorkspaceShareRead, false)
	refused("share", err)

	principal := PrincipalOwner("acme", "alice", "def-1")
	if _, err := f.m.Open(ctx, principal, OpenRequest{Name: "src2", Root: "code"}); !errors.Is(err, ErrRootOverMCP) {
		t.Errorf("a root over MCP: %v, want ErrRootOverMCP", err)
	}
	for name, req := range map[string]OpenRequest{
		"with a toolchain": {Name: "a", Root: "code", Toolchains: []string{"python"}},
		"durable":          {Name: "b", Root: "code", Lifetime: store.WorkspaceLifetimeDurable},
		"with a template":  {Name: "c", Root: "code", Template: &store.WorkspaceTemplateRow{TenantID: "acme"}},
		"a bad root name":  {Name: "d", Root: "../x"},
	} {
		if _, err := f.m.Open(ctx, alice, req); !errors.Is(err, ErrInvalidName) {
			t.Errorf("%s: %v, want ErrInvalidName", name, err)
		}
	}
	// The open of a name the run holds must ask for the same root.
	if _, err := f.m.Open(ctx, alice, OpenRequest{Name: "src", Toolchains: []string{"python", "node"}}); err == nil {
		t.Error("an ordinary open of a root workspace's name returned it")
	}
	if again, err := f.m.Open(ctx, alice, OpenRequest{Name: "src", Root: "code"}); err != nil || again.ID != r.ID {
		t.Errorf("a repeated root open = %v, %v", again.ID, err)
	}
}

func readReq(path string) dispatch.Request {
	return dispatch.Request{Op: workspace.OpRead, Args: []byte(`{"path":"` + path + `"}`)}
}
