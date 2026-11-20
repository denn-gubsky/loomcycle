package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/workspacepb"
)

// fop runs one of the JSON file ops in workspace id the way the op stream
// does.
func fop(t *testing.T, w *worker, id, op string, args any) answer {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return w.run(context.Background(), &workspacepb.OpStart{WorkspaceId: id, Op: op, Args: raw}, nil)
}

func mustOK(t *testing.T, a answer, what string) {
	t.Helper()
	if a.class != "" {
		t.Fatalf("%s: refused: %s: %s", what, a.class, a.detail)
	}
}

// listRoot is every name directly in the worker's root, for a check that
// nothing was made beside the workspaces.
func listRoot(t *testing.T, root string) string {
	t.Helper()
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return strings.Join(names, ",")
}

// Every raw-byte op refuses, on the worker, a path that is absolute (even
// one inside the workspace), one with `..`, and one through a link, whether
// the link leads out of the workspace or not; nothing outside changes.
func TestFileOps_RefuseAbsoluteDotDotAndLinkedPathsForEveryOp(t *testing.T) {
	w, root := newTestWorker(t)
	ws := filepath.Join(root, "ws1")
	if err := os.Mkdir(filepath.Join(root, "ws2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(ws, "up")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "in"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "in"), filepath.Join(ws, "inlink")); err != nil {
		t.Fatal(err)
	}
	stage := fop(t, w, "ws1", workspace.OpStage, struct{}{})
	mustOK(t, stage, "stage")
	var st workspace.StageAnswer
	_ = json.Unmarshal(stage.output, &st)
	before := listRoot(t, root)
	bad := []string{"../secret.txt", "..", "in/../../secret.txt", filepath.Join(root, "secret.txt"), filepath.Join(ws, "in"), "/in", "up/secret.txt", "up/ws2", "inlink/x", workDir + "/stage"}
	for _, p := range bad {
		for _, c := range []struct {
			op   string
			args any
		}{
			{workspace.OpFStat, workspace.FStatArgs{Path: p, SHA256: true}},
			{workspace.OpFList, workspace.FListArgs{Path: p, Recursive: true}},
			{workspace.OpMkdir, workspace.MkdirArgs{Path: p + "/new"}},
			{workspace.OpFRemove, workspace.FRemoveArgs{Path: p}},
			{workspace.OpCommit, workspace.CommitArgs{Stage: st.Stage, Path: p + "/new"}},
			{workspace.OpCopyLocal, workspace.CopyLocalArgs{FromWorkspace: "ws2", FromPath: p, Path: "copied", MaxFiles: 10, MaxBytes: 1 << 20}},
			{workspace.OpCopyLocal, workspace.CopyLocalArgs{FromWorkspace: "ws2", FromPath: ".", Path: p + "/copied", MaxFiles: 10, MaxBytes: 1 << 20}},
		} {
			a := fop(t, w, "ws1", c.op, c.args)
			if a.class == "" {
				t.Errorf("%s %q was not refused: %s", c.op, p, a.output)
			}
			// The path the op named may be echoed; the worker's own layout
			// may not.
			if (!strings.Contains(p, root) && strings.Contains(a.detail, root)) || strings.Contains(string(a.output), "outside") {
				t.Errorf("%s %q gave away the worker's disk: %q %q", c.op, p, a.detail, a.output)
			}
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "secret.txt")); string(b) != "outside" {
		t.Errorf("the file outside the workspace changed: %q", b)
	}
	if after := listRoot(t, root); after != before {
		t.Errorf("the worker's root changed: %s, was %s", after, before)
	}
	// A stage is a namespace of its own, but a path in it is checked alike.
	for _, p := range []string{"../x", "/x", workDir} {
		if a := fop(t, w, "ws1", workspace.OpMkdir, workspace.MkdirArgs{Path: p, Stage: st.Stage}); a.class == "" {
			t.Errorf("mkdir %q in a stage was not refused", p)
		}
	}
	for _, p := range []string{"ws2/new", "ws2/copied", "in/new"} {
		if _, err := os.Lstat(filepath.Join(root, p)); err == nil {
			t.Errorf("%s was made", p)
		}
	}
	// A link is reported for what it is, not read through.
	a := fop(t, w, "ws1", workspace.OpFStat, workspace.FStatArgs{Path: "up", SHA256: true})
	mustOK(t, a, "fstat of a link")
	var fi workspace.FileInfo
	_ = json.Unmarshal(a.output, &fi)
	if fi.Type != workspace.EntryLink || fi.SHA256 != "" {
		t.Errorf("fstat of a link = %+v, want a link and no digest", fi)
	}
	// Removing a link removes the link, never what it points at.
	mustOK(t, fop(t, w, "ws1", workspace.OpFRemove, workspace.FRemoveArgs{Path: "up"}), "fremove of a link")
	if _, err := os.Stat(filepath.Join(root, "secret.txt")); err != nil {
		t.Errorf("removing a link removed its target: %v", err)
	}
}

// The workspace's root cannot be removed by fremove, in any spelling.
func TestFileOps_FRemoveRefusesTheRoot(t *testing.T) {
	w, root := newTestWorker(t)
	if err := os.WriteFile(filepath.Join(root, "ws1", "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", ".", "/", "./", "a/..", "./."} {
		if a := fop(t, w, "ws1", workspace.OpFRemove, workspace.FRemoveArgs{Path: p}); a.class != workspace.ClassInvalid {
			t.Errorf("fremove %q = %q %s, want invalid", p, a.class, a.detail)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "ws1", "keep")); err != nil {
		t.Errorf("the workspace lost its file: %v", err)
	}
}

// fls lists one level or everything, in lexical order, reports a link as a
// link without descending into it, never lists the worker's own folder,
// and stops at its limit.
func TestFileOps_FListListsWithoutFollowingLinksOrShowingTheWorkFolder(t *testing.T) {
	w, root := newTestWorker(t)
	ws := filepath.Join(root, "ws1")
	for _, d := range []string{"a/b", workDir + "/tmp"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"a/b/c.txt", "z.txt", workDir + "/tmp/x"} {
		if err := os.WriteFile(filepath.Join(ws, f), []byte("12345"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(root, filepath.Join(ws, "l")); err != nil {
		t.Fatal(err)
	}
	list := func(args workspace.FListArgs) workspace.FListAnswer {
		a := fop(t, w, "ws1", workspace.OpFList, args)
		mustOK(t, a, "fls")
		var out workspace.FListAnswer
		if err := json.Unmarshal(a.output, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	names := func(l workspace.FListAnswer) string {
		var s []string
		for _, e := range l.Entries {
			s = append(s, e.Path+":"+e.Type)
		}
		return strings.Join(s, " ")
	}
	if got := names(list(workspace.FListArgs{Path: ""})); got != "a:dir l:link z.txt:file" {
		t.Errorf("one level = %q", got)
	}
	if got := names(list(workspace.FListArgs{Path: ".", Recursive: true})); got != "a:dir a/b:dir a/b/c.txt:file l:link z.txt:file" {
		t.Errorf("recursive = %q", got)
	}
	if got := names(list(workspace.FListArgs{Path: "a", Recursive: true})); got != "a/b:dir a/b/c.txt:file" {
		t.Errorf("recursive from a = %q", got)
	}
	if l := list(workspace.FListArgs{Path: "", Recursive: true, Limit: 2}); len(l.Entries) != 2 || !l.Truncated {
		t.Errorf("limit 2 = %+v, want two entries, truncated", l)
	}
}

// A copy that would pass a bound, or the target's quota, is refused before
// anything is written, and says which.
func TestFileOps_CopyLocalOverABoundOrTheQuotaWritesNothing(t *testing.T) {
	w, root := newTestWorker(t)
	if err := os.Mkdir(filepath.Join(root, "ws2"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i, f := range []string{"d/1", "d/2", "d/3"} {
		p := filepath.Join(root, "ws2", f)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(strings.Repeat("x", 1000*(i+1))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.setLimit("ws1", 5000); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		files int
		bytes int64
		class string
		want  string
	}{
		{2, 1 << 20, workspace.ClassCopyLimit, "max_files"},
		{10, 5999, workspace.ClassCopyLimit, "max_bytes"},
		{10, 1 << 20, workspace.ClassOverQuota, "quota"},
	} {
		a := fop(t, w, "ws1", workspace.OpCopyLocal, workspace.CopyLocalArgs{FromWorkspace: "ws2", FromPath: "d", Path: "d", MaxFiles: c.files, MaxBytes: c.bytes})
		if a.class != c.class || !strings.Contains(a.detail, c.want) {
			t.Errorf("copy with %d files, %d bytes = %q %q, want %s naming %s", c.files, c.bytes, a.class, a.detail, c.class, c.want)
		}
	}
	if got := listRoot(t, filepath.Join(root, "ws1")); got != "" {
		t.Errorf("the target holds %q after refused copies, want nothing", got)
	}
}

// A worker killed mid-put or mid-stage leaves its temporary files and
// stages in the work folder; the next start removes them, and the quota
// files of workspaces no longer there.
func TestCleanWork_RemovesWhatAKilledWorkerLeft(t *testing.T) {
	w, root := newTestWorker(t)
	w.log = discardLogger()
	ws := filepath.Join(root, "ws1")
	for _, f := range []string{tmpDir + "/put-abc", stageDir + "/0123/f"} {
		p := filepath.Join(ws, filepath.FromSlash(f))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.setLimit("ws1", 100); err != nil {
		t.Fatal(err)
	}
	if err := w.setLimit("gone", 100); err != nil {
		t.Fatal(err)
	}
	w.cleanWork()
	if _, err := os.Lstat(filepath.Join(ws, workDir)); err == nil {
		t.Error("the work folder survived the start")
	}
	if w.limitOf("ws1") != 100 {
		t.Error("a held workspace's quota was dropped")
	}
	if _, err := os.Lstat(filepath.Join(root, "gone"+quotaSuffix)); err == nil {
		t.Error("the quota of a workspace no longer there survived")
	}
}

// A workspace opened with no quota (by an older core) has the default; an
// open's quota is kept with the workspace, out of its reach, and dropped
// with it.
func TestOpen_KeepsTheQuotaOutsideTheWorkspace(t *testing.T) {
	w, root := newTestWorker(t)
	if w.limitOf("ws1") != workspace.DefaultDiskQuotaBytes {
		t.Errorf("a workspace with no recorded quota has %d", w.limitOf("ws1"))
	}
	start := &workspacepb.OpStart{WorkspaceId: "ws9", Op: workspace.OpOpen, Args: []byte(`{"disk_quota_bytes":12345}`)}
	mustOK(t, w.run(context.Background(), start, nil), "open")
	if w.limitOf("ws9") != 12345 {
		t.Errorf("the open's quota = %d", w.limitOf("ws9"))
	}
	if _, err := os.Stat(filepath.Join(root, "ws9"+quotaSuffix)); err != nil {
		t.Errorf("the quota is not kept beside the workspace: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(root, "ws9")); len(ents) != 0 {
		t.Errorf("the open put %d entries in the workspace", len(ents))
	}
	for _, raw := range []string{`{"disk_quota_bytes":-1}`, `{"disk_quota_bytes":17592186044417}`, `{"quota":1}`} {
		if a := w.run(context.Background(), &workspacepb.OpStart{WorkspaceId: "ws8", Op: workspace.OpOpen, Args: []byte(raw)}, nil); a.class != workspace.ClassInvalid {
			t.Errorf("open %s = %q, want invalid", raw, a.class)
		}
	}
	mustOK(t, w.run(context.Background(), &workspacepb.OpStart{WorkspaceId: "ws9", Op: workspace.OpRemove}, nil), "remove")
	if _, err := os.Stat(filepath.Join(root, "ws9"+quotaSuffix)); err == nil {
		t.Error("the removed workspace's quota survived")
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
