package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// newPersonalWorker is a personal worker with one root, "work": a user's
// directory holding keep.txt, outside the worker's state folder.
func newPersonalWorker(t *testing.T) (w *worker, state, user string) {
	t.Helper()
	state = t.TempDir()
	user = t.TempDir()
	writeFile(t, filepath.Join(user, "keep.txt"), 5)
	roots, err := checkRoots(state, []Root{{Name: "work", Path: user}})
	if err != nil {
		t.Fatal(err)
	}
	w = &worker{opt: Options{Root: state, Capacity: 4, Personal: true, Roots: roots}, engine: &engine.Host{}, log: slog.New(slog.DiscardHandler)}
	if err := w.loadBindings(); err != nil {
		t.Fatal(err)
	}
	return w, state, user
}

func openRoot(t *testing.T, w *worker, id, root string) answer {
	t.Helper()
	return fop(t, w, id, workspace.OpOpen, workspace.OpenArgs{Root: root})
}

// RFC GW §11 "Personal workers": a root is still there after release. A
// remove, and the reconciliation's removal of an id the core no longer
// knows, unbind a root workspace and never delete the user's directory.
func TestRoots_ReleaseNeverRemovesTheDirectory(t *testing.T) {
	w, _, user := newPersonalWorker(t)
	mustOK(t, openRoot(t, w, "ws1", "work"), "open the root")
	mustOK(t, w.run(context.Background(), &workspacepb.OpStart{WorkspaceId: "ws1", Op: workspace.OpRemove}, nil), "remove")
	if _, err := os.Stat(filepath.Join(user, "keep.txt")); err != nil {
		t.Fatalf("the root's file after remove: %v", err)
	}
	if held, _ := w.held(); slices.Contains(held, "ws1") {
		t.Errorf("the unbound id is still held: %v", held)
	}
	// Bound again, then removed by reconciliation.
	mustOK(t, openRoot(t, w, "ws2", "work"), "open the root again")
	if gone := w.removeAll([]string{"ws2"}, w.logger()); !slices.Equal(gone, []string{"ws2"}) {
		t.Errorf("reconciliation's remove = %v", gone)
	}
	if _, err := os.Stat(filepath.Join(user, "keep.txt")); err != nil {
		t.Fatalf("the root's file after reconciliation: %v", err)
	}
	// And the directory itself is there, with nothing added at its side.
	if _, err := os.Stat(user); err != nil {
		t.Fatalf("the root: %v", err)
	}
}

// The root's path is the worker's alone: no answer names it, a refusal
// included, and the labels name the root by its name only.
func TestRoots_AnswersNeverNameTheRootsPath(t *testing.T) {
	w, _, user := newPersonalWorker(t)
	real, _ := filepath.EvalSymlinks(user)
	mustOK(t, openRoot(t, w, "ws1", "work"), "open")
	for _, a := range []answer{
		fop(t, w, "ws1", workspace.OpRead, workspace.ReadArgs{Path: "missing.txt"}),
		fop(t, w, "ws1", workspace.OpRead, workspace.ReadArgs{Path: "../escape"}),
		fop(t, w, "ws1", workspace.OpFStat, workspace.FStatArgs{Path: "nope"}),
		fop(t, w, "ws1", workspace.OpCheckpoint, workspace.CheckpointArgs{}),
		openRoot(t, w, "ws2", "work"),
		openRoot(t, w, "ws3", "elsewhere"),
	} {
		for _, p := range []string{user, real} {
			if strings.Contains(a.detail, p) || strings.Contains(string(a.output), p) {
				t.Errorf("an answer names the root's path: %q %q", a.detail, a.output)
			}
		}
	}
	for _, l := range w.rootLabels() {
		if strings.Contains(l, user) || strings.Contains(l, real) || l != workspace.RootLabelPrefix+"work" {
			t.Errorf("label %q", l)
		}
	}
}

// File ops on a root workspace stay inside the root: a climbing path, an
// absolute one and a link out are refused.
func TestRoots_FileOpsStayInsideTheRoot(t *testing.T) {
	w, _, user := newPersonalWorker(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), 3)
	if err := os.Symlink(outside, filepath.Join(user, "out")); err != nil {
		t.Fatal(err)
	}
	mustOK(t, openRoot(t, w, "ws1", "work"), "open")
	for _, p := range []string{"../" + filepath.Base(outside) + "/secret.txt", filepath.Join(outside, "secret.txt"), "out/secret.txt"} {
		if a := fop(t, w, "ws1", workspace.OpRead, workspace.ReadArgs{Path: p}); a.class == "" {
			t.Errorf("read %q = %q, want refused", p, a.output)
		}
	}
	a := fop(t, w, "ws1", workspace.OpRead, workspace.ReadArgs{Path: "keep.txt"})
	mustOK(t, a, "read inside the root")
}

// A root holds the user's own files: checkpoints, restore, fork and a
// template are refused on it (RFC GW §4.5), and there is no disk quota.
func TestRoots_CheckpointForkAndTemplateAreRefused(t *testing.T) {
	w, _, _ := newPersonalWorker(t)
	mustOK(t, openRoot(t, w, "ws1", "work"), "open")
	for _, try := range []struct {
		op   string
		args any
	}{
		{workspace.OpCheckpoint, workspace.CheckpointArgs{}},
		{workspace.OpCheckpoints, struct{}{}},
		{workspace.OpRestore, workspace.CheckpointRef{Checkpoint: "ck_0123456789abcdef"}},
		{workspace.OpDeleteCheckpoint, workspace.CheckpointRef{Checkpoint: "ck_0123456789abcdef"}},
	} {
		if a := fop(t, w, "ws1", try.op, try.args); a.class != workspace.ClassNotOnRoot {
			t.Errorf("%s on a root = %q %q, want %s", try.op, a.class, a.detail, workspace.ClassNotOnRoot)
		}
	}
	if a := fop(t, w, "ws9", workspace.OpFork, workspace.ForkArgs{From: "ws1"}); a.class != workspace.ClassNotOnRoot {
		t.Errorf("a fork of a root = %q %q, want %s", a.class, a.detail, workspace.ClassNotOnRoot)
	}
	if _, err := os.Lstat(filepath.Join(w.opt.Root, "ws9")); err == nil {
		t.Error("a refused fork made a directory")
	}
	if a := w.templatePackOp(context.Background(), "ws1", w.wsDir("ws1"), w.wsDir("ws1"), []byte(`{"checkpoint":"ck_0123456789abcdef"}`), nil); a.class != workspace.ClassNotOnRoot {
		t.Errorf("a template pack of a root = %q %q, want %s", a.class, a.detail, workspace.ClassNotOnRoot)
	}
	// No quota reaches a root: not an open without the root (refused, so
	// the root's folder is not taken for the id's own), and not a limit
	// set on a usage already made by an earlier op.
	if a := fop(t, w, "ws1", workspace.OpOpen, workspace.OpenArgs{DiskQuotaBytes: 1024}); a.class != workspace.ClassExists {
		t.Errorf("an open without the root of a bound id = %q %q, want %s", a.class, a.detail, workspace.ClassExists)
	}
	mustOK(t, fop(t, w, "ws1", workspace.OpWrite, workspace.WriteArgs{Path: "small.txt", Content: "x"}), "a first write")
	if err := w.setLimit("ws1", 1024); err != nil {
		t.Fatal(err)
	}
	a := fop(t, w, "ws1", workspace.OpWrite, workspace.WriteArgs{Path: "big.txt", Content: strings.Repeat("x", 4096)})
	mustOK(t, a, "a write past any quota on a root")
}

// One live workspace per root: a second open of a bound root is busy; a
// repeated open of the same id is harmless; after release it binds again.
func TestRoots_OneBindingPerRoot(t *testing.T) {
	w, _, _ := newPersonalWorker(t)
	mustOK(t, openRoot(t, w, "ws1", "work"), "open")
	mustOK(t, openRoot(t, w, "ws1", "work"), "a repeated open")
	if a := openRoot(t, w, "ws2", "work"); a.class != workspace.ClassRootBusy {
		t.Errorf("a second binding of the root = %q %q, want %s", a.class, a.detail, workspace.ClassRootBusy)
	}
	if a := openRoot(t, w, "ws2", "elsewhere"); a.class != workspace.ClassInvalid {
		t.Errorf("an undeclared root = %q, want %s", a.class, workspace.ClassInvalid)
	}
	// An id with a directory of its own is not bound over it.
	mustOK(t, fop(t, w, "ws3", workspace.OpOpen, workspace.OpenArgs{}), "a plain open")
	if a := openRoot(t, w, "ws3", "work"); a.class != workspace.ClassExists {
		t.Errorf("a root open of an id with its own directory = %q, want %s", a.class, workspace.ClassExists)
	}
	mustOK(t, w.run(context.Background(), &workspacepb.OpStart{WorkspaceId: "ws1", Op: workspace.OpRemove}, nil), "remove")
	mustOK(t, openRoot(t, w, "ws2", "work"), "bound again after release")
	// A binding survives the worker's restart.
	w2 := &worker{opt: w.opt, engine: &engine.Host{}, log: w.log}
	if err := w2.loadBindings(); err != nil {
		t.Fatal(err)
	}
	if held, _ := w2.held(); !slices.Contains(held, "ws2") {
		t.Errorf("after a restart held = %v, want ws2", held)
	}
	// A worker that offers no roots takes no root open.
	plain, _ := newTestWorker(t)
	if a := fop(t, plain, "ws5", workspace.OpOpen, workspace.OpenArgs{Root: "work"}); a.class != workspace.ClassInvalid {
		t.Errorf("a root open on a worker with no roots = %q, want %s", a.class, workspace.ClassInvalid)
	}
}

// D-P13-7: a root must exist and be a folder, roots may not nest, and none
// may be, hold or sit inside the worker's own state folder.
func TestRoots_BadDeclarationsRefusedAtStart(t *testing.T) {
	state := t.TempDir()
	user := t.TempDir()
	writeFile(t, filepath.Join(user, "file.txt"), 1)
	if err := os.Mkdir(filepath.Join(user, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, roots := range map[string][]Root{
		"missing":          {{Name: "a", Path: filepath.Join(user, "nope")}},
		"a file":           {{Name: "a", Path: filepath.Join(user, "file.txt")}},
		"nested":           {{Name: "a", Path: user}, {Name: "b", Path: filepath.Join(user, "inner")}},
		"the state folder": {{Name: "a", Path: state}},
		"inside the state": {{Name: "a", Path: filepath.Join(state, "x")}},
		"holds the state":  {{Name: "a", Path: filepath.Dir(state)}},
		"a bad name":       {{Name: "../a", Path: user}},
		"a name twice":     {{Name: "a", Path: user}, {Name: "a", Path: t.TempDir()}},
		"relative":         {{Name: "a", Path: "relative/dir"}},
	} {
		if name == "inside the state" {
			if err := os.MkdirAll(filepath.Join(state, "x"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := checkRoots(state, roots); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	got, err := checkRoots(state, []Root{{Name: "a", Path: user}, {Name: "b", Path: t.TempDir()}})
	if err != nil || len(got) != 2 {
		t.Errorf("two good roots = %+v, %v", got, err)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatal(err)
	}
}

// Run refuses a personal worker it cannot run safely before it dials: no
// roots, roots on a worker that is not personal, the container engine, and
// a declaration checkRoots refuses.
func TestRoots_RunRefusesABadPersonalStart(t *testing.T) {
	user := t.TempDir()
	if err := os.Mkdir(filepath.Join(user, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Options{
		"personal, no roots":     {Personal: true},
		"roots, not personal":    {Roots: []Root{{Name: "a", Path: user}}},
		"personal, container":    {Personal: true, Engine: workspace.EngineContainer, Roots: []Root{{Name: "a", Path: user}}},
		"personal, nested roots": {Personal: true, Roots: []Root{{Name: "a", Path: user}, {Name: "b", Path: filepath.Join(user, "inner")}}},
	} {
		o.Token, o.Target, o.Root = "t", "unix:///nonexistent/loomcycle.sock", t.TempDir()
		o.Logger = slog.New(slog.DiscardHandler)
		// Bounded: a start that got past the checks would keep dialing.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := Run(ctx, o)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "root") && !strings.Contains(err.Error(), "personal") {
			t.Errorf("%s: Run = %v, want a refusal of the roots", name, err)
		}
	}
}

// Removes of a bound id racing each other (and a rebind) never delete a
// file of the root: the first unbinds, and a later one that no longer sees
// the binding must not take the root's folder for the id's own.
func TestRoots_RacingRemovesNeverDeleteTheRoot(t *testing.T) {
	w, _, user := newPersonalWorker(t)
	for i := 0; i < 20; i++ {
		mustOK(t, openRoot(t, w, "ws1", "work"), "open")
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = w.run(context.Background(), &workspacepb.OpStart{WorkspaceId: "ws1", Op: workspace.OpRemove}, nil)
				_ = openRoot(t, w, "ws1", "work")
			}()
		}
		wg.Wait()
		if _, err := os.Stat(filepath.Join(user, "keep.txt")); err != nil {
			t.Fatalf("round %d: the root's file: %v", i, err)
		}
		_ = w.run(context.Background(), &workspacepb.OpStart{WorkspaceId: "ws1", Op: workspace.OpRemove}, nil)
	}
}

// The last guard before a workspace's folder is deleted: never a declared
// root, nothing inside one or holding one, nothing outside the state folder.
func TestRoots_RemovingAPathOfARootIsRefused(t *testing.T) {
	w, state, user := newPersonalWorker(t)
	if err := os.Mkdir(filepath.Join(user, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(user, filepath.Join(state, "ws7")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{user, filepath.Join(user, "sub"), filepath.Dir(user), filepath.Join(state, "ws7"), t.TempDir(), state} {
		if err := w.removable(p); err == nil {
			t.Errorf("removable(%q) = nil, want refused", p)
		}
	}
	if err := w.removable(filepath.Join(state, "ws8")); err != nil {
		t.Errorf("a workspace's own folder: %v", err)
	}
	// A link in the state folder to the root: the remove fails closed.
	if a := fop(t, w, "ws7", workspace.OpRemove, struct{}{}); a.class != workspace.ClassFailed {
		t.Errorf("a remove of a link to the root = %q %q, want %s", a.class, a.detail, workspace.ClassFailed)
	}
	if _, err := os.Stat(filepath.Join(user, "keep.txt")); err != nil {
		t.Fatalf("the root's file: %v", err)
	}
}

// D-P13-18: the start's clean-up of the worker's work folder skips a root.
// A user's own .loomcycle-work in their folder, with a file in it, is still
// there after the worker restarts.
func TestRoots_RestartLeavesTheUsersWorkFolderInARoot(t *testing.T) {
	w, _, user := newPersonalWorker(t)
	mine := filepath.Join(user, workDir, "mine.txt")
	if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, mine, 3)
	mustOK(t, openRoot(t, w, "ws1", "work"), "open")
	w2 := &worker{opt: w.opt, engine: &engine.Host{}, log: w.log}
	if err := w2.loadBindings(); err != nil {
		t.Fatal(err)
	}
	w2.cleanWork()
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("the user's file after a restart: %v", err)
	}
}
