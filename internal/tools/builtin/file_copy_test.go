package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// fcFixture is a run of alice with an inbox, a read-write volume "docs", a
// read-only volume "ref", the File tool on a real workspace worker, and the
// workspace grant read and write.
type fcFixture struct {
	ws    *wsFixture
	file  *File
	ctx   context.Context
	box   *inboxtest.Run
	docs  string
	runID string
}

func newFCFixture(t *testing.T, svc inbox.Service, l lifecycle.Limits) *fcFixture {
	t.Helper()
	return newFCFixtureWith(t, svc, l, worker.Options{Capacity: 10})
}

func newFCFixtureWith(t *testing.T, svc inbox.Service, l lifecycle.Limits, opt worker.Options) *fcFixture {
	t.Helper()
	ws := newWSFixtureWith(t, l, opt)
	box, ctx, docs := fileRun(t, svc, false)
	f := &fcFixture{ws: ws, box: box, docs: docs, file: &File{Workspaces: ws.tool.Manager}}
	f.ctx, f.runID = f.runOf(t, ctx, "alice")
	return f
}

// runOf makes ctx a store run of user, with File, Read, Write and Workspace
// and the full workspace grant.
func (f *fcFixture) runOf(t *testing.T, ctx context.Context, user string) (context.Context, string) {
	t.Helper()
	r, err := f.ws.st.CreateRun(context.Background(), f.ws.sess, store.RunIdentity{AgentID: "a_" + user, UserID: user, TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: user, AgentID: "a_" + user, TenantID: "acme", RootRunID: "root-1"})
	ctx = tools.WithRunID(ctx, r.ID)
	ctx = tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) {
		p.Tools = []string{"File", "Read", "Write", "Workspace"}
		p.Workspace = wsFull
	})
	return ctx, r.ID
}

func (f *fcFixture) exec(t *testing.T, ctx context.Context, in map[string]any) (map[string]any, tools.Result) {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := f.file.Execute(ctx, raw)
	if err != nil {
		t.Fatalf("Execute(%s): %v", raw, err)
	}
	if res.IsError {
		return nil, res
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode %q: %v", res.Text, err)
	}
	return out, res
}

func (f *fcFixture) mustCopy(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	in["op"] = "copy"
	out, res := f.exec(t, f.ctx, in)
	if res.IsError {
		t.Fatalf("copy %v: %s", in, res.Text)
	}
	return out
}

func (f *fcFixture) open(t *testing.T, name string) {
	t.Helper()
	f.ws.mustOK(t, f.ctx, `{"op":"open","name":"`+name+`"}`)
}

func (f *fcFixture) owner() lifecycle.Owner {
	ident := tools.RunIdentity(f.ctx)
	return lifecycle.Owner{TenantID: "acme", UserID: ident.UserID, RunID: f.runID, AgentID: ident.AgentID, RootRunID: ident.RootRunID}
}

// wsPut writes a file into the run's workspace name.
func (f *fcFixture) wsPut(t *testing.T, name, p, content string) {
	t.Helper()
	if _, err := f.ws.tool.Manager.PutFile(context.Background(), f.owner(), name, workspace.PutArgs{Path: p, Size: int64(len(content))}, strings.NewReader(content)); err != nil {
		t.Fatalf("put %s:%s: %v", name, p, err)
	}
}

// wsGet reads a file of the run's workspace; ok is false when it is not there.
func (f *fcFixture) wsGet(t *testing.T, name, p string) (string, bool) {
	t.Helper()
	var b bytes.Buffer
	if _, err := f.ws.tool.Manager.GetFile(context.Background(), f.owner(), name, workspace.GetArgs{Path: p}, &b, 0); err != nil {
		return "", false
	}
	return b.String(), true
}

func (f *fcFixture) wsHas(t *testing.T, name, p string) bool {
	t.Helper()
	_, err := f.ws.tool.Manager.Stat(context.Background(), f.owner(), name, workspace.FStatArgs{Path: p})
	return err == nil
}

func (f *fcFixture) inboxRoot(t *testing.T) string {
	t.Helper()
	ref, ok := f.box.Set.Get(inbox.Name)
	if !ok {
		t.Fatal("no inbox")
	}
	return ref.Root
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func wantFileRefusal(t *testing.T, what string, res tools.Result, contains string) {
	t.Helper()
	if !res.IsError || !strings.Contains(res.Text, contains) {
		t.Fatalf("%s = %+v, want a refusal containing %q", what, res, contains)
	}
}

var folderFiles = map[string]string{"a.txt": "alpha", "sub/b.txt": "bravo"}

// Every direction of the RFC's copy table moves a file and a folder, the
// bytes arriving intact: into a workspace from the inbox and from a volume,
// out of a workspace into a volume and into the inbox, and between two
// workspaces of the run (here on one worker, which copies them itself).
func TestFileCopy_EveryDirectionCopiesAFileAndAFolder(t *testing.T) {
	f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{})
	f.open(t, "w")
	f.open(t, "w2")
	// Sources: a file and a folder in each kind of place.
	kept := inbox.Capture(f.box.Ctx, inbox.Item{Name: "note.txt", Dir: "src", Origin: "o"}, []byte("from inbox"))
	inbox.Capture(f.box.Ctx, inbox.Item{Name: "b.txt", Dir: "src/sub", Origin: "o"}, []byte("bravo"))
	inbox.Capture(f.box.Ctx, inbox.Item{Name: "a.txt", Dir: "src", Origin: "o"}, []byte("alpha"))
	writeFiles(t, f.docs, map[string]string{"one.txt": "from volume", "src/a.txt": "alpha", "src/sub/b.txt": "bravo"})
	f.wsPut(t, "w", "one.txt", "from workspace")
	f.wsPut(t, "w", "src/a.txt", "alpha")
	f.wsPut(t, "w", "src/sub/b.txt", "bravo")

	// inbox -> workspace
	f.mustCopy(t, map[string]any{"from": kept.Record.Ref, "to": "workspace:w2:in/"})
	if got, _ := f.wsGet(t, "w2", "in/note.txt"); got != "from inbox" {
		t.Errorf("inbox->workspace file: %q", got)
	}
	f.mustCopy(t, map[string]any{"from": "volume:inbox:src", "to": "workspace:w2:fromInbox"})
	if a, _ := f.wsGet(t, "w2", "fromInbox/a.txt"); a != "alpha" {
		t.Errorf("inbox->workspace folder a.txt: %q", a)
	}
	if b, _ := f.wsGet(t, "w2", "fromInbox/sub/b.txt"); b != "bravo" {
		t.Errorf("inbox->workspace folder sub/b.txt: %q", b)
	}
	// volume -> workspace
	f.mustCopy(t, map[string]any{"from": "volume:docs:one.txt", "to": "workspace:w2:vol.txt"})
	if got, _ := f.wsGet(t, "w2", "vol.txt"); got != "from volume" {
		t.Errorf("volume->workspace file: %q", got)
	}
	f.mustCopy(t, map[string]any{"from": "volume:docs:src", "to": "workspace:w2:fromVol"})
	if b, _ := f.wsGet(t, "w2", "fromVol/sub/b.txt"); b != "bravo" {
		t.Errorf("volume->workspace folder: %q", b)
	}
	// workspace -> volume
	out := f.mustCopy(t, map[string]any{"from": "workspace:w:one.txt", "to": "volume:docs:out.txt"})
	if readFile(t, filepath.Join(f.docs, "out.txt")) != "from workspace" || out["ref"] != "volume:docs:out.txt" {
		t.Errorf("workspace->volume file: %v", out)
	}
	f.mustCopy(t, map[string]any{"from": "workspace:w:src", "to": "volume:docs:fromWS"})
	if readFile(t, filepath.Join(f.docs, "fromWS", "a.txt")) != "alpha" || readFile(t, filepath.Join(f.docs, "fromWS", "sub", "b.txt")) != "bravo" {
		t.Error("workspace->volume folder: wrong content")
	}
	// workspace -> inbox: captured, so recorded with its origin.
	out = f.mustCopy(t, map[string]any{"from": "workspace:w:one.txt", "to": "volume:inbox:got/"})
	if out["ref"] != "volume:inbox:got/one.txt" || readFile(t, filepath.Join(f.inboxRoot(t), "got", "one.txt")) != "from workspace" {
		t.Errorf("workspace->inbox file: %v", out)
	}
	if rec, ok := inbox.Lookup(f.box.Ctx, "got/one.txt"); !ok || rec.Origin != "copy:workspace:w" {
		t.Errorf("record = %+v %v, want origin copy:workspace:w", rec, ok)
	}
	f.mustCopy(t, map[string]any{"from": "workspace:w:src", "to": "volume:inbox:fromWS"})
	if readFile(t, filepath.Join(f.inboxRoot(t), "fromWS", "sub", "b.txt")) != "bravo" {
		t.Error("workspace->inbox folder: wrong content")
	}
	// workspace -> workspace, one worker
	out = f.mustCopy(t, map[string]any{"from": "workspace:w:one.txt", "to": "workspace:w2:same.txt"})
	if got, _ := f.wsGet(t, "w2", "same.txt"); got != "from workspace" || out["same_worker"] != true {
		t.Errorf("workspace->workspace file: %q %v", got, out)
	}
	f.mustCopy(t, map[string]any{"from": "workspace:w:src", "to": "workspace:w2:fromW"})
	if b, _ := f.wsGet(t, "w2", "fromW/sub/b.txt"); b != "bravo" {
		t.Errorf("workspace->workspace folder: %q", b)
	}
	// stat and ls see what arrived.
	st, res := f.exec(t, f.ctx, map[string]any{"op": "stat", "path": "workspace:w2:fromW/a.txt"})
	if res.IsError || st["size"] != float64(5) || st["type"] != workspace.EntryFile || st["sha256"] == "" {
		t.Errorf("stat = %v %s", st, res.Text)
	}
	ls, res := f.exec(t, f.ctx, map[string]any{"op": "ls", "path": "workspace:w2:fromW", "recursive": true})
	if res.IsError || !strings.Contains(res.Text, `"workspace:w2:fromW/sub/b.txt"`) {
		t.Errorf("ls = %v %s", ls, res.Text)
	}
}

// Two workspaces of one run on two workers: the copy is not the worker's to
// do, so the bytes stream through the core, a file and a folder.
func TestFileCopy_BetweenWorkspacesOnTwoWorkersStreamsThroughTheCore(t *testing.T) {
	f := newFCFixtureWith(t, inbox.Service{}, lifecycle.Limits{}, worker.Options{Capacity: 1})
	f.ws.h.AddWorker("w2", 1)
	f.open(t, "a")
	f.open(t, "b")
	ra, _ := f.ws.tool.Manager.Get(context.Background(), f.owner(), "a")
	rb, _ := f.ws.tool.Manager.Get(context.Background(), f.owner(), "b")
	if ra.WorkerID == rb.WorkerID {
		t.Fatalf("both workspaces on %s", ra.WorkerID)
	}
	f.wsPut(t, "a", "x.bin", "\x00\x01binary\xff")
	f.wsPut(t, "a", "dir/one.txt", "one")
	f.wsPut(t, "a", "dir/deep/two.txt", "two")

	out := f.mustCopy(t, map[string]any{"from": "workspace:a:x.bin", "to": "workspace:b:x.bin"})
	if got, _ := f.wsGet(t, "b", "x.bin"); got != "\x00\x01binary\xff" || out["same_worker"] != nil {
		t.Errorf("file: %q %v", got, out)
	}
	out = f.mustCopy(t, map[string]any{"from": "workspace:a:dir", "to": "workspace:b:copied"})
	if got, _ := f.wsGet(t, "b", "copied/deep/two.txt"); got != "two" || out["files"] != float64(2) {
		t.Errorf("folder: %q %v", got, out)
	}
}

// A folder over each bound is refused before anything is written, and the
// refusal names the bound: the file count, the byte total, the per-file
// limit, and the inbox's remaining budget.
func TestFileCopy_AFolderOverABoundIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	f := newFCFixture(t, inbox.Service{MaxFileBytes: 60, MaxTreeBytes: 50}, lifecycle.Limits{CopyMaxFiles: 3, CopyMaxBytes: 100})
	f.open(t, "w")
	for i, n := range []string{"1", "2", "3", "4"} {
		f.wsPut(t, "w", "many/"+n+".txt", strings.Repeat("m", i+1))
	}
	f.wsPut(t, "w", "heavy/a.txt", strings.Repeat("h", 55))
	f.wsPut(t, "w", "heavy/b.txt", strings.Repeat("h", 55))
	f.wsPut(t, "w", "big/a.txt", strings.Repeat("b", 70))
	f.wsPut(t, "w", "budget/a.txt", strings.Repeat("i", 30))
	f.wsPut(t, "w", "budget/b.txt", strings.Repeat("i", 30))

	for _, c := range []struct{ from, to, bound string }{
		{"workspace:w:many", "volume:docs:many", "workspace.copy.max_files"},
		{"workspace:w:heavy", "volume:docs:heavy", "workspace.copy.max_bytes"},
		{"workspace:w:big", "volume:docs:big", "LOOMCYCLE_FILE_MAX_BYTES"},
		{"workspace:w:budget", "volume:inbox:budget", "inbox has left"},
	} {
		_, res := f.exec(t, f.ctx, map[string]any{"op": "copy", "from": c.from, "to": c.to})
		wantFileRefusal(t, c.from, res, c.bound)
	}
	for _, p := range []string{"many", "heavy", "big"} {
		if _, err := os.Stat(filepath.Join(f.docs, p)); !os.IsNotExist(err) {
			t.Errorf("volume:docs:%s exists after a refused copy (%v)", p, err)
		}
	}
	if ents, _ := os.ReadDir(f.docs); len(ents) != 0 {
		t.Errorf("the volume holds %d entries after refused copies", len(ents))
	}
	if inbox.Available(f.box.Ctx) && f.box.Set.Has(inbox.Name) {
		if _, err := os.Stat(filepath.Join(f.inboxRoot(t), "budget")); !os.IsNotExist(err) {
			t.Errorf("the inbox holds budget/ after a refused copy (%v)", err)
		}
	}
}

// A copy into a workspace over its disk quota fails and leaves nothing: not
// the file, not the folder, not a stage.
func TestFileCopy_OverTheQuotaLeavesNothing(t *testing.T) {
	f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{DiskQuotaBytes: 1000})
	f.open(t, "w")
	writeFiles(t, f.docs, map[string]string{"big.txt": strings.Repeat("q", 1500), "dir/a.txt": strings.Repeat("a", 600), "dir/b.txt": strings.Repeat("b", 600)})

	_, res := f.exec(t, f.ctx, map[string]any{"op": "copy", "from": "volume:docs:big.txt", "to": "workspace:w:big.txt"})
	wantFileRefusal(t, "file over quota", res, workspace.ClassOverQuota)
	_, res = f.exec(t, f.ctx, map[string]any{"op": "copy", "from": "volume:docs:dir", "to": "workspace:w:dir"})
	wantFileRefusal(t, "folder over quota", res, workspace.ClassOverQuota)
	ls, res := f.exec(t, f.ctx, map[string]any{"op": "ls", "path": "workspace:w:", "recursive": true})
	if res.IsError || len(ls["entries"].([]any)) != 0 {
		t.Errorf("after refused copies the workspace holds %v %s", ls, res.Text)
	}
	if work := filepath.Join(f.wsDir(t, "w"), ".loomcycle-work", "stage"); dirHasEntries(work) {
		t.Errorf("a stage was left in %s", work)
	}
}

func (f *fcFixture) wsDir(t *testing.T, name string) string {
	t.Helper()
	row, err := f.ws.tool.Manager.Get(context.Background(), f.owner(), name)
	if err != nil {
		t.Fatal(err)
	}
	return f.ws.h.Dir(row.ID)
}

func dirHasEntries(p string) bool {
	ents, err := os.ReadDir(p)
	return err == nil && len(ents) > 0
}

// overwrite is off by default: a copy onto an existing file or folder is
// refused and leaves it. On, a file is replaced and a folder merged into.
func TestFileCopy_OverwriteOffRefusesAndOnReplaces(t *testing.T) {
	f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{})
	f.open(t, "w")
	writeFiles(t, f.docs, map[string]string{"a.txt": "new", "src/x.txt": "new x", "src/y.txt": "new y", "old.txt": "old"})
	f.wsPut(t, "w", "a.txt", "old")
	f.wsPut(t, "w", "src/x.txt", "old x")
	f.wsPut(t, "w", "src/keep.txt", "kept")

	_, res := f.exec(t, f.ctx, map[string]any{"op": "copy", "from": "volume:docs:a.txt", "to": "workspace:w:a.txt"})
	wantFileRefusal(t, "file, overwrite off", res, workspace.ClassExists)
	_, res = f.exec(t, f.ctx, map[string]any{"op": "copy", "from": "volume:docs:src", "to": "workspace:w:."})
	wantFileRefusal(t, "folder, overwrite off", res, workspace.ClassExists)
	if got, _ := f.wsGet(t, "w", "a.txt"); got != "old" {
		t.Errorf("a.txt after a refused copy = %q", got)
	}
	_, res = f.exec(t, f.ctx, map[string]any{"op": "copy", "from": "workspace:w:a.txt", "to": "volume:docs:old.txt"})
	wantFileRefusal(t, "volume file, overwrite off", res, "already exists")

	f.mustCopy(t, map[string]any{"from": "volume:docs:a.txt", "to": "workspace:w:a.txt", "overwrite": true})
	f.mustCopy(t, map[string]any{"from": "volume:docs:src", "to": "workspace:w:.", "overwrite": true})
	f.mustCopy(t, map[string]any{"from": "workspace:w:src", "to": "volume:docs:.", "overwrite": true})
	for p, want := range map[string]string{"a.txt": "new", "src/x.txt": "new x", "src/y.txt": "new y", "src/keep.txt": "kept"} {
		if got, _ := f.wsGet(t, "w", p); got != want {
			t.Errorf("workspace %s = %q, want %q", p, got, want)
		}
	}
	if readFile(t, filepath.Join(f.docs, "src", "keep.txt")) != "kept" || readFile(t, filepath.Join(f.docs, "src", "x.txt")) != "new x" {
		t.Error("the volume folder was not merged")
	}
}

// rm and clear never remove a workspace's root; rm removes a file or a
// folder, clear empties a folder and keeps it.
func TestFileRm_TheWorkspaceRootIsRefused(t *testing.T) {
	f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{})
	f.open(t, "w")
	f.wsPut(t, "w", "a.txt", "a")
	f.wsPut(t, "w", "dir/b.txt", "b")
	f.wsPut(t, "w", "dir/c.txt", "c")
	for _, p := range []string{"workspace:w:", "workspace:w:.", "workspace:w:./"} {
		_, res := f.exec(t, f.ctx, map[string]any{"op": "rm", "path": p})
		wantFileRefusal(t, "rm "+p, res, "root")
	}
	if !f.wsHas(t, "w", "a.txt") {
		t.Fatal("a.txt is gone after a refused rm of the root")
	}
	out, res := f.exec(t, f.ctx, map[string]any{"op": "clear", "path": "workspace:w:dir"})
	if res.IsError || out["files"] != float64(2) || !f.wsHas(t, "w", "dir") || f.wsHas(t, "w", "dir/b.txt") {
		t.Errorf("clear = %v %s", out, res.Text)
	}
	if out, res = f.exec(t, f.ctx, map[string]any{"op": "rm", "path": "workspace:w:a.txt"}); res.IsError || f.wsHas(t, "w", "a.txt") {
		t.Errorf("rm = %v %s", out, res.Text)
	}
}

// Another run's workspace is not found, the same answer as a name nobody
// opened: stat, ls, copy out of it, copy into it, rm.
func TestFileWorkspace_AnotherRunsWorkspaceIsNotFound(t *testing.T) {
	f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{})
	f.open(t, "w")
	f.wsPut(t, "w", "secret.txt", "s")
	writeFiles(t, f.docs, map[string]string{"x.txt": "x"})
	other, _ := f.runOf(t, f.ctx, "bob")
	for _, in := range []map[string]any{
		{"op": "stat", "path": "workspace:w:secret.txt"},
		{"op": "ls", "path": "workspace:w:"},
		{"op": "copy", "from": "workspace:w:secret.txt", "to": "volume:docs:stolen.txt"},
		{"op": "copy", "from": "volume:docs:x.txt", "to": "workspace:w:planted.txt"},
		{"op": "rm", "path": "workspace:w:secret.txt"},
	} {
		_, res := f.exec(t, other, in)
		wantFileRefusal(t, in["op"].(string), res, `no workspace named "w"`)
	}
	if _, err := os.Stat(filepath.Join(f.docs, "stolen.txt")); !os.IsNotExist(err) {
		t.Error("another run copied the file out")
	}
	if !f.wsHas(t, "w", "secret.txt") || f.wsHas(t, "w", "planted.txt") {
		t.Error("another run changed the workspace")
	}
}

// The grant is checked before a byte moves: no workspace grant refuses every
// use; read only refuses a workspace target and rm, and allows reading one.
// Off a run there is no workspace at all.
func TestFileCopy_TheGrantIsCheckedBeforeAnyByteMoves(t *testing.T) {
	f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{})
	f.open(t, "w")
	f.wsPut(t, "w", "a.txt", "a")
	writeFiles(t, f.docs, map[string]string{"x.txt": "x"})
	none := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{} })
	readOnly := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Workspace = tools.WorkspacePolicyValue{Read: true} })

	for _, in := range []map[string]any{
		{"op": "stat", "path": "workspace:w:a.txt"},
		{"op": "ls", "path": "workspace:w:"},
		{"op": "copy", "from": "workspace:w:a.txt", "to": "volume:docs:out.txt"},
		{"op": "copy", "from": "volume:docs:x.txt", "to": "workspace:w:in.txt"},
	} {
		_, res := f.exec(t, none, in)
		wantFileRefusal(t, "no grant: "+in["op"].(string), res, "workspace grant does not allow")
	}
	for _, in := range []map[string]any{
		{"op": "copy", "from": "volume:docs:x.txt", "to": "workspace:w:in.txt"},
		{"op": "rm", "path": "workspace:w:a.txt"},
		{"op": "clear", "path": "workspace:w:"},
	} {
		_, res := f.exec(t, readOnly, in)
		wantFileRefusal(t, "read only: "+in["op"].(string), res, "does not allow write")
	}
	if f.wsHas(t, "w", "in.txt") || !f.wsHas(t, "w", "a.txt") {
		t.Error("a refused call changed the workspace")
	}
	if _, err := os.Stat(filepath.Join(f.docs, "out.txt")); !os.IsNotExist(err) {
		t.Error("a refused call wrote the volume")
	}
	if _, res := f.exec(t, readOnly, map[string]any{"op": "copy", "from": "workspace:w:a.txt", "to": "volume:docs:out.txt"}); res.IsError {
		t.Errorf("read grant, workspace source: %s", res.Text)
	}
	// A run without the File tool in its tools (the dispatcher would not
	// call it, but the reference checks it too), and a call off any run.
	noFile := tools.UpdateRunPolicy(f.ctx, func(p *tools.RunPolicy) { p.Tools = []string{"Workspace", "Read", "Write"} })
	_, res := f.exec(t, noFile, map[string]any{"op": "stat", "path": "workspace:w:a.txt"})
	wantFileRefusal(t, "no File tool", res, "needs the File tool")
	offRun := tools.WithRunPolicy(context.Background(), tools.PolicyOf(f.ctx))
	_, res = f.exec(t, offRun, map[string]any{"op": "stat", "path": "workspace:w:a.txt"})
	wantFileRefusal(t, "off a run", res, "belongs to a run")
}

// The run's Workspace pre-hooks judge a workspace reference as a Workspace
// call {op, name, path, via: File}, on the FINAL path a folder target
// expands to, and a hook's refusal stops the copy before anything is
// written.
func TestFileCopy_AWorkspaceHookSeesTheFinalPathAndCanRefuse(t *testing.T) {
	f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{})
	f.open(t, "w")
	f.wsPut(t, "w", "in/keep.txt", "k")
	f.wsPut(t, "w", "src.txt", "s")
	writeFiles(t, f.docs, map[string]string{"a.txt": "a", "b.txt": "b"})
	type call struct{ Tool, Op, Name, Path, Via string }
	var seen []call
	ctx := tools.WithHookPreCheck(f.ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
		var in struct{ Op, Name, Path, Via string }
		_ = json.Unmarshal(input, &in)
		seen = append(seen, call{name, in.Op, in.Name, in.Path, in.Via})
		if name == "Workspace" && in.Path == "in/b.txt" {
			return tools.Result{Text: "no b here", IsError: true}, false
		}
		return tools.Result{}, true
	})

	if _, res := f.exec(t, ctx, map[string]any{"op": "copy", "from": "volume:docs:a.txt", "to": "workspace:w:in"}); res.IsError {
		t.Fatalf("copy a: %s", res.Text)
	}
	want := call{"Workspace", "write", "w", "in/a.txt", "File"}
	if !containsCall(seen, want) {
		t.Errorf("hooks saw %+v, want %+v", seen, want)
	}
	for _, c := range seen {
		if c.Tool == "Workspace" && c.Path == "in" {
			t.Errorf("a hook was asked about the folder, not the file: %+v", c)
		}
	}
	_, res := f.exec(t, ctx, map[string]any{"op": "copy", "from": "volume:docs:b.txt", "to": "workspace:w:in/"})
	wantFileRefusal(t, "hooked copy", res, "no b here")
	if f.wsHas(t, "w", "in/b.txt") {
		t.Error("the refused copy wrote in/b.txt")
	}
	seen = nil
	f.exec(t, ctx, map[string]any{"op": "copy", "from": "workspace:w:src.txt", "to": "volume:docs:./"})
	f.exec(t, ctx, map[string]any{"op": "rm", "path": "workspace:w:in/keep.txt"})
	for _, w := range []call{{"Workspace", "read", "w", "src.txt", "File"}, {"Workspace", "remove", "w", "in/keep.txt", "File"}} {
		if !containsCall(seen, w) {
			t.Errorf("hooks saw %+v, want %+v", seen, w)
		}
	}
}

func containsCall[T comparable](s []T, v T) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// The ops that do not take a workspace reference yet refuse one plainly.
func TestFileWorkspace_OpsWithoutWorkspaceSupportRefuseTheReference(t *testing.T) {
	f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{})
	f.open(t, "w")
	f.wsPut(t, "w", "a.docx", "x")
	for _, in := range []map[string]any{
		{"op": "unpack", "path": "workspace:w:a.docx"},
		{"op": "keep", "from": "workspace:w:a.docx", "to": "volume:docs:a.docx"},
	} {
		_, res := f.exec(t, f.ctx, in)
		if !res.IsError {
			t.Errorf("%v was not refused", in)
		}
	}
	_, res := f.exec(t, f.ctx, map[string]any{"op": "unpack", "path": "workspace:w:a.docx"})
	wantFileRefusal(t, "unpack", res, "workspace reference")
}

func TestParseDataRef_WorkspaceForms(t *testing.T) {
	for in, want := range map[string]dataRef{
		"workspace:w:a/b.txt":  {kind: refWorkspace, workspace: "w", path: "a/b.txt"},
		"workspace:build.1:":   {kind: refWorkspace, workspace: "build.1", path: ""},
		"workspace:w:dir/x:y":  {kind: refWorkspace, workspace: "w", path: "dir/x:y"},
		"workspace:w_2:./out/": {kind: refWorkspace, workspace: "w_2", path: "./out/"},
	} {
		got, err := parseDataRef(in)
		got.raw = ""
		if err != nil || got != want {
			t.Errorf("parseDataRef(%q) = %+v %v, want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"workspace:", "workspace:w", "workspace:-w:a", "workspace:w@agent:a", "workspace:w:/etc/passwd", "workspace:w:a/../../b", "workspace:a b:c"} {
		if _, err := parseDataRef(bad); err == nil {
			t.Errorf("parseDataRef(%q) accepted", bad)
		}
	}
	// A bare path is still the inbox's; workspace: is a reference.
	if r, _ := fileRef("workspace:w:a"); r.kind != refWorkspace {
		t.Errorf("fileRef(workspace:w:a) = %+v", r)
	}
}

// clear names a workspace folder by path and an inbox folder by prefix. A
// path that is not a workspace's is refused, never ignored: ignored, it
// would empty the whole inbox for a call that meant one folder.
func TestFileClear_APathThatIsNotAWorkspaceIsRefusedAndTheInboxStays(t *testing.T) {
	f := newFCFixture(t, inbox.Service{}, lifecycle.Limits{})
	kept := inbox.Capture(f.box.Ctx, inbox.Item{Name: "a.pdf", Origin: "o"}, filePDF)
	for _, in := range []map[string]any{
		{"op": "clear", "path": "volume:inbox:images"},
		{"op": "clear", "path": "images"},
		{"op": "clear", "path": "workspace:w:", "prefix": "images"},
	} {
		_, res := f.exec(t, f.ctx, in)
		wantFileRefusal(t, "clear", res, "path names a folder of a workspace")
	}
	if _, err := os.Stat(filepath.Join(f.inboxRoot(t), kept.Record.Path)); err != nil {
		t.Errorf("the inbox file is gone: %v", err)
	}
}

// A hook that refuses one file inside a folder refuses the whole folder
// copy, before anything is written: each entry is judged on its own source
// and target path, not only the folder's. Volume to workspace (a Read hook
// on the source file, a Workspace write hook on the target file),
// workspace to volume, workspace to workspace on one worker (the worker's
// own copy) and on two, and an overwrite merge.
func TestFileCopy_AHookOnOneFileInsideAFolderRefusesTheWholeCopy(t *testing.T) {
	deny := func(ctx context.Context, tool, p string) context.Context {
		return tools.WithHookPreCheck(ctx, func(_ context.Context, name string, input json.RawMessage) (tools.Result, bool) {
			var in struct{ Path string }
			_ = json.Unmarshal(input, &in)
			if name == tool && in.Path == p {
				return tools.Result{Text: "denied " + p, IsError: true}, false
			}
			return tools.Result{}, true
		})
	}
	setup := func(t *testing.T, opt worker.Options, second bool) *fcFixture {
		f := newFCFixtureWith(t, inbox.Service{}, lifecycle.Limits{}, opt)
		if second {
			f.ws.h.AddWorker("w2", 1)
		}
		f.open(t, "a")
		f.open(t, "b")
		writeFiles(t, f.docs, map[string]string{"src/ok.txt": "ok", "src/secrets/key.txt": "k"})
		f.wsPut(t, "a", "src/ok.txt", "ok")
		f.wsPut(t, "a", "src/secrets/key.txt", "k")
		return f
	}
	for _, c := range []struct {
		name       string
		opt        worker.Options
		second     bool
		from, to   string
		tool, path string
		overwrite  bool
		written    func(f *fcFixture) bool
	}{
		{"volume->workspace, Read hook on the source", worker.Options{Capacity: 10}, false, "volume:docs:src", "workspace:b:dst", "Read", "src/secrets/key.txt", false,
			func(f *fcFixture) bool { return f.wsHas(t, "b", "dst") }},
		{"volume->workspace, Workspace hook on the target", worker.Options{Capacity: 10}, false, "volume:docs:src", "workspace:b:dst", "Workspace", "dst/secrets/key.txt", false,
			func(f *fcFixture) bool { return f.wsHas(t, "b", "dst") }},
		{"workspace->volume, Workspace hook on the source", worker.Options{Capacity: 10}, false, "workspace:a:src", "volume:docs:dst", "Workspace", "src/secrets/key.txt", false,
			func(f *fcFixture) bool { _, err := os.Stat(filepath.Join(f.docs, "dst")); return err == nil }},
		{"workspace->volume, Write hook on the target", worker.Options{Capacity: 10}, false, "workspace:a:src", "volume:docs:dst", "Write", "dst/secrets/key.txt", false,
			func(f *fcFixture) bool { _, err := os.Stat(filepath.Join(f.docs, "dst")); return err == nil }},
		{"workspace->workspace, one worker", worker.Options{Capacity: 10}, false, "workspace:a:src", "workspace:b:dst", "Workspace", "dst/secrets/key.txt", false,
			func(f *fcFixture) bool { return f.wsHas(t, "b", "dst") }},
		{"workspace->workspace, two workers", worker.Options{Capacity: 1}, true, "workspace:a:src", "workspace:b:dst", "Workspace", "src/secrets/key.txt", false,
			func(f *fcFixture) bool { return f.wsHas(t, "b", "dst") }},
		{"overwrite merge into a volume folder", worker.Options{Capacity: 10}, false, "workspace:a:src", "volume:docs:./", "Write", "src/secrets/key.txt", true,
			func(f *fcFixture) bool {
				return readFile(t, filepath.Join(f.docs, "src", "ok.txt")) != "OLD" || readFile(t, filepath.Join(f.docs, "src", "secrets", "key.txt")) != "OLDK"
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t, c.opt, c.second)
			if c.overwrite {
				// The merge target holds other bytes, which a refused merge must leave.
				writeFiles(t, f.docs, map[string]string{"src/ok.txt": "OLD", "src/secrets/key.txt": "OLDK"})
			}
			_, res := f.exec(t, deny(f.ctx, c.tool, c.path), map[string]any{"op": "copy", "from": c.from, "to": c.to, "overwrite": c.overwrite})
			wantFileRefusal(t, c.name, res, "denied "+c.path)
			if c.written(f) {
				t.Errorf("%s: something was written after the refusal", c.name)
			}
		})
	}
}
