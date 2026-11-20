package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/engine"
)

// newTestWorker is a worker whose root holds one workspace, ws1, and a file
// outside it, secret.txt, beside the workspace in the root.
func newTestWorker(t *testing.T, commands ...string) (*worker, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "ws1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &worker{opt: Options{Root: root, Capacity: 4}, engine: &engine.Host{Commands: commands}}, root
}

func runOp(t *testing.T, w *worker, op string, args any) answer {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return w.inWorkspace(context.Background(), filepath.Join(w.opt.Root, "ws1"), op, raw, nil)
}

func fileText(t *testing.T, a answer) string {
	t.Helper()
	if a.class != "" {
		t.Fatalf("op refused: %s: %s", a.class, a.detail)
	}
	var fa workspace.FileAnswer
	if err := json.Unmarshal(a.output, &fa); err != nil {
		t.Fatalf("answer %q: %v", a.output, err)
	}
	return fa.Text
}

// A path out of the workspace is refused on the worker, whatever form it
// takes: `..`, an absolute path elsewhere, a symlink that leads out.
func TestWorkspaceOps_RefusePathsOutOfTheWorkspace(t *testing.T) {
	w, root := newTestWorker(t)
	ws := filepath.Join(root, "ws1")
	if err := os.Symlink(filepath.Join(root, "secret.txt"), filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(ws, "up")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../secret.txt", filepath.Join(root, "secret.txt"), "link", "up/secret.txt"} {
		if a := runOp(t, w, workspace.OpRead, workspace.ReadArgs{Path: p}); a.class == "" || strings.Contains(string(a.output), "outside") {
			t.Errorf("read %q was not refused: %q", p, a.output)
		}
		// Write replaces a symlink at the target itself, as the Write tool
		// does, so "link" is a write inside the workspace; the others are
		// refused. Either way nothing outside changes (checked below).
		if a := runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: p, Content: "x"}); a.class == "" && p != "link" {
			t.Errorf("write %q was not refused", p)
		}
		if a := runOp(t, w, workspace.OpEdit, workspace.EditArgs{Path: p, OldString: "outside", NewString: "x"}); a.class == "" {
			t.Errorf("edit %q was not refused", p)
		}
		// A symlinked file out of the workspace is skipped, not followed.
		if a := runOp(t, w, workspace.OpGrep, workspace.GrepArgs{Pattern: "outside", Path: p, OutputMode: "content"}); a.class == "" && strings.Contains(string(a.output), "outside") {
			t.Errorf("grep %q read the file outside: %s", p, a.output)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "secret.txt")); string(b) != "outside" {
		t.Errorf("the file outside the workspace changed: %q", b)
	}
	if a := runOp(t, w, workspace.OpExec, workspace.ExecArgs{Command: "pwd", Cwd: "..", TimeoutMS: 5000}); a.class != workspace.ClassInvalid {
		t.Errorf("exec with cwd .. = %q %s, want invalid", a.class, a.output)
	}
}

func TestWorkspaceOps_FileOpsWorkAndHideTheWorkersPaths(t *testing.T) {
	w, root := newTestWorker(t)
	// Write makes the file, not its directory, as the Write tool does.
	if a := runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: "src/a.txt", Content: "x"}); a.class != workspace.ClassFailed {
		t.Errorf("a write into a missing directory = %q, want failed", a.class)
	}
	if err := os.MkdirAll(filepath.Join(root, "ws1", "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := fileText(t, runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: "src/a.txt", Content: "alpha\nbeta\n"}))
	if out != "wrote 11 bytes to src/a.txt" {
		t.Errorf("write = %q", out)
	}
	if got := fileText(t, runOp(t, w, workspace.OpEdit, workspace.EditArgs{Path: "src/a.txt", OldString: "beta", NewString: "gamma"})); got != "edited src/a.txt (1 replacement)" {
		t.Errorf("edit = %q", got)
	}
	one := 2
	if got := fileText(t, runOp(t, w, workspace.OpRead, workspace.ReadArgs{Path: "src/a.txt", Offset: &one})); !strings.HasPrefix(got, "gamma\n") {
		t.Errorf("read from line 2 = %q", got)
	}
	if got := fileText(t, runOp(t, w, workspace.OpGlob, workspace.GlobArgs{Pattern: "**/*.txt"})); got != "src/a.txt\n" {
		t.Errorf("glob = %q", got)
	}
	if got := fileText(t, runOp(t, w, workspace.OpGrep, workspace.GrepArgs{Pattern: "gam", OutputMode: "content"})); !strings.Contains(got, "src/a.txt") || strings.Contains(got, root) {
		t.Errorf("grep = %q", got)
	}
	a := runOp(t, w, workspace.OpRead, workspace.ReadArgs{Path: "missing.txt"})
	if a.class != workspace.ClassFailed || strings.Contains(a.detail, root) {
		t.Errorf("a missing file = %s %q, want failed without the worker's root in it", a.class, a.detail)
	}
}

func TestWorkspaceOps_RefuseUnknownArgumentsAndBadLineRanges(t *testing.T) {
	w, _ := newTestWorker(t)
	for name, raw := range map[string]string{
		"volume":     `{"path":"a","volume":"v"}`,
		"misspelt":   `{"path":"a","ofset":2}`,
		"offset 0":   `{"path":"a","offset":0}`,
		"limit 0":    `{"path":"a","limit":0}`,
		"no args":    ``,
		"not object": `[1]`,
	} {
		a := w.inWorkspace(context.Background(), filepath.Join(w.opt.Root, "ws1"), workspace.OpRead, []byte(raw), nil)
		if a.class != workspace.ClassInvalid {
			t.Errorf("%s: class %q (%s), want invalid", name, a.class, a.detail)
		}
	}
}

func TestWorkspaceOps_ReadOfABinaryFileByLinesIsRefused(t *testing.T) {
	w, root := newTestWorker(t)
	if err := os.WriteFile(filepath.Join(root, "ws1", "bin"), []byte("a\x00b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	one := 1
	if a := runOp(t, w, workspace.OpRead, workspace.ReadArgs{Path: "bin", Offset: &one}); a.class != workspace.ClassInvalid || !strings.Contains(a.detail, "binary") {
		t.Errorf("= %s %q, want invalid naming a binary file", a.class, a.detail)
	}
}

func TestWorkspaceOps_ExecRunsInTheWorkspaceAndReportsTheExitCode(t *testing.T) {
	w, root := newTestWorker(t)
	if err := os.Mkdir(filepath.Join(root, "ws1", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := runOp(t, w, workspace.OpExec, workspace.ExecArgs{Command: "basename \"$(pwd)\"; exit 7", Cwd: "sub", TimeoutMS: 5000})
	if a.class != "" {
		t.Fatalf("refused: %s %s", a.class, a.detail)
	}
	var ea workspace.ExecAnswer
	if err := json.Unmarshal(a.output, &ea); err != nil {
		t.Fatal(err)
	}
	if ea.ExitCode != 7 || ea.Output != "sub\n" || a.exit != 7 {
		t.Errorf("exec = %+v (exit frame %d), want exit 7 and output sub", ea, a.exit)
	}
	if a := runOp(t, w, workspace.OpExec, workspace.ExecArgs{Command: "true"}); a.class != workspace.ClassInvalid {
		t.Errorf("an exec with no timeout = %q, want invalid", a.class)
	}
}

func TestWorkspaceOps_ExecOfAnUnlistedCommandIsRefused(t *testing.T) {
	w, root := newTestWorker(t, "echo")
	a := runOp(t, w, workspace.OpExec, workspace.ExecArgs{Command: "touch made", TimeoutMS: 5000})
	if a.class != workspace.ClassRefused {
		t.Errorf("class = %q, want refused", a.class)
	}
	if _, err := os.Stat(filepath.Join(root, "ws1", "made")); err == nil {
		t.Error("the refused command ran")
	}
}

func TestWorkspaceOps_AMissingWorkspaceIsNotFound(t *testing.T) {
	w, root := newTestWorker(t)
	a := w.inWorkspace(context.Background(), filepath.Join(root, "ws2"), workspace.OpRead, []byte(`{"path":"a"}`), nil)
	if a.class != workspace.ClassNotFound {
		t.Errorf("class = %q, want not_found", a.class)
	}
}
