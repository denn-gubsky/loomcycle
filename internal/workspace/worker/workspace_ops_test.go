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

// A file's own text is returned as it is, even where it holds the
// workspace's path: only the runtime's own lines are rewritten.
func TestWorkspaceOps_AFilesTextIsNotRewritten(t *testing.T) {
	w, root := newTestWorker(t)
	dir, _ := filepath.EvalSymlinks(filepath.Join(root, "ws1"))
	body := "cwd was " + dir + "/sub\n"
	if err := os.WriteFile(filepath.Join(root, "ws1", "note.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := fileText(t, runOp(t, w, workspace.OpRead, workspace.ReadArgs{Path: "note.txt"})); got != body {
		t.Errorf("read = %q, want the file's text %q", got, body)
	}
}

// Ruling Q2: the Workspace tool's write and edit are writes. In a workspace
// marked full both are refused with workspace_over_quota while read still
// works; a write or an edit that would pass the quota is refused with
// nothing written.
func TestWorkspaceOps_WriteAndEditHonourTheQuota(t *testing.T) {
	w, root := newTestWorker(t)
	ws := filepath.Join(root, "ws1")
	if err := w.setLimit("ws1", 1000); err != nil {
		t.Fatal(err)
	}
	if a := runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: "a.txt", Content: "hello"}); a.class != "" {
		t.Fatalf("a write within the quota: %s %s", a.class, a.detail)
	}
	if a := runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: "big.txt", Content: strings.Repeat("x", 2000)}); a.class != workspace.ClassOverQuota {
		t.Errorf("a write past the quota = %q %s, want workspace_over_quota", a.class, a.detail)
	}
	if _, err := os.Lstat(filepath.Join(ws, "big.txt")); err == nil {
		t.Error("the refused write left its file")
	}
	if a := runOp(t, w, workspace.OpEdit, workspace.EditArgs{Path: "a.txt", OldString: "hello", NewString: strings.Repeat("y", 2000)}); a.class != workspace.ClassOverQuota {
		t.Errorf("an edit growing past the quota = %q %s, want workspace_over_quota", a.class, a.detail)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "a.txt")); string(b) != "hello" {
		t.Errorf("the refused edit changed the file: %q", b)
	}
	// A command writes past the quota; the workspace is then full.
	if a := runOp(t, w, workspace.OpExec, workspace.ExecArgs{Command: "head -c 3000 /dev/zero > past", TimeoutMS: 5000}); a.class != "" {
		t.Fatalf("exec: %s %s", a.class, a.detail)
	}
	if a := runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: "b.txt", Content: "x"}); a.class != workspace.ClassOverQuota {
		t.Errorf("a write in a full workspace = %q, want workspace_over_quota", a.class)
	}
	if a := runOp(t, w, workspace.OpEdit, workspace.EditArgs{Path: "a.txt", OldString: "hello", NewString: "h"}); a.class != workspace.ClassOverQuota {
		t.Errorf("an edit in a full workspace = %q, want workspace_over_quota", a.class)
	}
	if got := fileText(t, runOp(t, w, workspace.OpRead, workspace.ReadArgs{Path: "a.txt"})); !strings.Contains(got, "hello") {
		t.Errorf("a read in a full workspace = %q", got)
	}
}

// A link cannot make the count drop: write and edit never go through a link
// (refused), and the size they measure is what they change, so after the
// attempt a put that would pass the quota is still refused.
func TestWorkspaceOps_ALinkCannotLowerTheQuotaCount(t *testing.T) {
	w, root := newTestWorker(t)
	ws := filepath.Join(root, "ws1")
	if err := w.setLimit("ws1", 1000); err != nil {
		t.Fatal(err)
	}
	if a := runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: "big.bin", Content: strings.Repeat("b", 900)}); a.class != "" {
		t.Fatalf("write big.bin: %s %s", a.class, a.detail)
	}
	if err := os.Symlink("big.bin", filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(ws, "dlink")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		op   string
		args any
	}{
		{workspace.OpWrite, workspace.WriteArgs{Path: "link", Content: "x"}},
		{workspace.OpEdit, workspace.EditArgs{Path: "link", OldString: strings.Repeat("b", 900), NewString: "x"}},
		{workspace.OpWrite, workspace.WriteArgs{Path: "dlink/f", Content: "x"}},
	} {
		if a := runOp(t, w, c.op, c.args); a.class == "" {
			t.Errorf("%s through a link succeeded", c.op)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "big.bin")); len(b) != 900 {
		t.Errorf("big.bin changed through the link: %d bytes", len(b))
	}
	if a := runOp(t, w, workspace.OpWrite, workspace.WriteArgs{Path: "more", Content: strings.Repeat("m", 200)}); a.class != workspace.ClassOverQuota {
		t.Errorf("a write past the quota after the link tricks = %q %s, want workspace_over_quota", a.class, a.detail)
	}
}
