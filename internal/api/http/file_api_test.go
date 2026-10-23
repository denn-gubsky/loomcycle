package http

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/dynvol"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// fileTwinFixture is a server with a dynamic root, a static "work" volume,
// and the File and Document tools on a real store and SQL Memory.
type fileTwinFixture struct {
	srv     *Server
	st      store.Store
	dynRoot string
	docs    *builtin.Document
}

func newFileTwinFixture(t *testing.T) *fileTwinFixture {
	t.Helper()
	dynRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Volumes: map[string]config.Volume{
			"pool": {Path: dynRoot, Mode: "rw", DynamicRoot: true},
			"work": {Path: work, Mode: "rw"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "file_twin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	docs := &builtin.Document{Store: st, SqlMem: mgr}
	file := &builtin.File{Docs: docs}
	srv := New(cfg, &stubResolver{p: &scriptedProvider{}}, []tools.Tool{file, docs}, concurrency.New(4, 4, time.Second), st)
	srv.sqlMem = mgr
	return &fileTwinFixture{srv: srv, st: st, dynRoot: dynRoot, docs: docs}
}

// run creates a top-level run of user in tenant, or a sub-run of parent.
func (f *fileTwinFixture) run(t *testing.T, tenant, user, parent string) string {
	t.Helper()
	ctx := context.Background()
	sess, err := f.st.CreateSession(ctx, tenant, "agent", user)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.st.CreateRun(ctx, sess.ID, store.RunIdentity{TenantID: tenant, UserID: user, ParentRunID: parent})
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// capture keeps one file in the inbox of the tree rooted at root, as a run of
// that tree does: through the server's tree state. live leaves the tree
// registered in this process, as a running tree is.
func (f *fileTwinFixture) capture(t *testing.T, root, tenant, name, origin string, data []byte, live bool) inbox.Record {
	t.Helper()
	set, release := f.srv.treeSets.acquire(root)
	if !live {
		defer release()
	}
	ctx := tools.WithEphemeralVolumes(context.Background(), set)
	ctx = inbox.With(ctx, f.srv.inboxService(), f.srv.treeSets.inbox(root))
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{RootRunID: root, TenantID: tenant})
	ctx = tools.WithRunID(ctx, root)
	res := inbox.Capture(ctx, inbox.Item{Name: name, Origin: origin}, data)
	if !res.Kept {
		t.Fatalf("capture: %s", res.Stub)
	}
	return res.Record
}

// sessionCtx is what an MCP session's call carries: the principal, and the
// identity and grants the session stamps for it.
func sessionCtx(p auth.Principal) context.Context {
	ctx := auth.WithPrincipal(context.Background(), p)
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: p.Subject, TenantID: p.TenantID, AgentID: "a_mcp-operator"})
	ctx = tools.WithAgentName(ctx, "mcp-operator")
	return tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Tools = []string{"*"}
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"agent", "user", "tenant"}}
	})
}

var (
	twinOwner    = auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}}
	twinOperator = auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeTenant}}
	twinOutsider = auth.Principal{TenantID: "evil", Subject: "mallory", Scopes: []string{auth.ScopeTenant}}
	twinAdmin    = auth.Principal{TenantID: "", Subject: "root", Scopes: []string{auth.ScopeAdmin}}
)

func (f *fileTwinFixture) call(t *testing.T, p auth.Principal, in map[string]any) (map[string]any, string, bool) {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := f.srv.File(sessionCtx(p), raw)
	if err != nil {
		t.Fatalf("File(%s): %v", raw, err)
	}
	var out map[string]any
	if !res.IsError {
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("decode %q: %v", res.Text, err)
		}
	}
	return out, res.Text, res.IsError
}

// The run's reader lists and stats its inbox; while the tree runs here, stat
// reports what the runtime recorded, origin included.
func TestFileTwin_ReaderListsAndStatsTheRunsInbox(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "report.pdf", "mcp:ms365/get-attachment", []byte("%PDF-1.7\nhello"), true)

	out, text, isErr := f.call(t, twinOperator, map[string]any{"run_id": runID, "op": "ls", "path": "volume:inbox:documents"})
	if isErr {
		t.Fatalf("ls: %s", text)
	}
	if !strings.Contains(text, rec.Ref) {
		t.Fatalf("ls does not list %s: %v", rec.Ref, out)
	}
	out, text, isErr = f.call(t, twinOperator, map[string]any{"run_id": runID, "op": "stat", "path": rec.Ref})
	if isErr || out["origin"] != "mcp:ms365/get-attachment" || out["sha256"] != rec.SHA256 {
		t.Fatalf("stat = %v %s", out, text)
	}
}

// With no run of the tree in this process, the twin still reads the inbox
// on disk; what only the live tree recorded (the origin) is unknown.
func TestFileTwin_ReadsAnInboxWithNoLiveTree(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "report.pdf", "mcp:ms365/get-attachment", []byte("%PDF-1.7\nhello"), false)

	out, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "stat", "path": rec.Ref})
	if isErr || out["origin"] != "unknown" || out["sha256"] != rec.SHA256 {
		t.Fatalf("stat = %v %s", out, text)
	}
}

// Another tenant's run, and an id that names no run, get the same answer.
func TestFileTwin_AnotherTenantsRunIsNotFound(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	f.capture(t, runID, "acme", "report.pdf", "http", []byte("%PDF-1.7\nhello"), true)

	_, cross, isErr := f.call(t, twinOutsider, map[string]any{"run_id": runID, "op": "ls"})
	if !isErr {
		t.Fatalf("another tenant listed the inbox: %s", cross)
	}
	_, missing, _ := f.call(t, twinOutsider, map[string]any{"run_id": "run_nosuch", "op": "ls"})
	if strings.Replace(cross, runID, "X", 1) != strings.Replace(missing, "run_nosuch", "X", 1) {
		t.Errorf("cross-tenant %q differs from missing %q", cross, missing)
	}
	// An admin sees every tenant's runs.
	if _, text, isErr := f.call(t, twinAdmin, map[string]any{"run_id": runID, "op": "ls"}); isErr {
		t.Errorf("admin ls: %s", text)
	}
}

// rm, clear and extract change the run's inbox: the run's owner and an admin
// may, a tenant operator who only reads the run may not.
func TestFileTwin_ChangingTheInboxNeedsTheOwnerOrAnAdmin(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	a := f.capture(t, runID, "acme", "a.txt", "http", []byte("first file\n"), true)
	b := f.capture(t, runID, "acme", "b.txt", "http", []byte("second file\n"), true)
	dir := dynvol.DerivedEphemeralPath(f.dynRoot, runID, inbox.Name)

	for _, op := range []map[string]any{
		{"run_id": runID, "op": "rm", "path": a.Ref},
		{"run_id": runID, "op": "clear"},
		{"run_id": runID, "op": "extract", "path": a.Ref},
	} {
		if _, text, isErr := f.call(t, twinOperator, op); !isErr || !strings.Contains(text, "owner or an admin") {
			t.Errorf("%v by a non-owner = %s", op["op"], text)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(a.Path))); err != nil {
		t.Fatalf("a refused rm removed the file: %v", err)
	}

	if _, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "rm", "path": a.Ref}); isErr {
		t.Fatalf("owner rm: %s", text)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(a.Path))); !os.IsNotExist(err) {
		t.Errorf("owner rm left the file: %v", err)
	}
	if _, text, isErr := f.call(t, twinAdmin, map[string]any{"run_id": runID, "op": "clear"}); isErr {
		t.Fatalf("admin clear: %s", text)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(b.Path))); !os.IsNotExist(err) {
		t.Errorf("admin clear left the file: %v", err)
	}
}

// An inbox made read-only by its run stays read-only to its owner here: the
// twin asks the tree's record before it writes.
func TestFileTwin_OwnerCannotRemoveFromAReadOnlyInbox(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	a := f.capture(t, runID, "acme", "a.txt", "http", []byte("first file\n"), true)
	ctx := context.Background()
	rows, err := f.st.EphemeralVolumeListByRun(ctx, runID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v %v", rows, err)
	}
	if _, err := f.st.EphemeralVolumeSetReadOnly(ctx, runID, inbox.Name); err != nil {
		t.Fatal(err)
	}
	if _, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "rm", "path": a.Ref}); !isErr || !strings.Contains(text, "read-only") {
		t.Fatalf("rm from a read-only inbox = %s", text)
	}
}

// A sub-run's inbox is its tree's.
func TestFileTwin_SubRunNamesItsTreesInbox(t *testing.T) {
	f := newFileTwinFixture(t)
	parent := f.run(t, "acme", "alice", "")
	child := f.run(t, "acme", "alice", parent)
	rec := f.capture(t, parent, "acme", "photo.png", "mcp:x/y", fileTwinPNG(), true)
	if _, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": child, "op": "stat", "path": rec.Ref}); isErr {
		t.Fatalf("stat via the sub-run: %s", text)
	}
}

// A finished tree's inbox was deleted with it, and the answer says so; a run
// that kept nothing yet says that instead.
func TestFileTwin_FinishedRunsInboxIsGone(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "a.txt", "http", []byte("first file\n"), true)
	f.srv.purgeEphemeralVolumesForRun(runID)
	if err := f.st.FinishRun(context.Background(), runID, store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "stat", "path": rec.Ref}); !isErr || !strings.Contains(text, "inbox is gone") {
		t.Fatalf("stat after the run ended = %s", text)
	}
	fresh := f.run(t, "acme", "alice", "")
	if _, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": fresh, "op": "ls"}); !isErr || !strings.Contains(text, "nothing has been kept") {
		t.Fatalf("ls of a run that kept nothing = %s", text)
	}
}

// keep copies into a read-write dynamic volume of the CALLER's tenant, at the
// path the runtime derives for it. A read-only one, a configured one and
// another tenant's are refused.
func TestFileTwin_KeepCopiesIntoTheCallersDynamicVolume(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "minutes.txt", "http", []byte("the minutes\n"), true)
	ctx := context.Background()
	mkVol := func(tenant, name, mode string) string {
		dir, _, err := dynvol.Provision(f.dynRoot, tenant, name)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]string{"path": dir, "mode": mode})
		if _, err := f.st.VolumeDefCreate(ctx, store.VolumeDefRow{TenantID: tenant, Name: name, Definition: body}); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	docs := mkVol("acme", "docs", "rw")
	mkVol("acme", "archive", "ro")
	mkVol("other", "theirs", "rw")

	if err := os.MkdirAll(filepath.Join(docs, "board"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, text, isErr := f.call(t, twinOperator, map[string]any{"run_id": runID, "op": "keep", "from": rec.Ref, "to": "volume:docs:board/"})
	if isErr {
		t.Fatalf("keep: %s", text)
	}
	if b, err := os.ReadFile(filepath.Join(docs, "board", "minutes.txt")); err != nil || string(b) != "the minutes\n" {
		t.Fatalf("kept file = %q %v (result %v)", b, err, out)
	}
	for to, want := range map[string]string{
		"volume:archive:x.txt": "read-only",
		"volume:work:x.txt":    "configured volume",
		"volume:theirs:x.txt":  "no volume named",
		"volume:inbox:x.txt":   "deleted with the run",
		"volume:x.txt":         "must name a volume",
	} {
		if _, text, isErr := f.call(t, twinOperator, map[string]any{"run_id": runID, "op": "keep", "from": rec.Ref, "to": to}); !isErr || !strings.Contains(text, want) {
			t.Errorf("keep to %s = %s, want %q", to, text, want)
		}
	}
}

// import builds the Document in the CALLER's own user scope, not the run's
// user's: here a tenant operator imports what the run's owner extracted.
func TestFileTwin_ImportBuildsTheDocumentInTheCallersScope(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "notes.md", "mcp:ms365/get-attachment", []byte("# Notes\n\n## Decision\n\nApprove the budget.\n"), true)

	ex, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "extract", "path": rec.Ref})
	if isErr {
		t.Fatalf("extract: %s", text)
	}
	out, text, isErr := f.call(t, twinOperator, map[string]any{"run_id": runID, "op": "import", "path": ex["ref"], "doc_path": "/board/notes"})
	if isErr {
		t.Fatalf("import: %s", text)
	}
	docID, _ := out["document_id"].(string)
	if docID == "" || out["source"] != "mcp:ms365/get-attachment" {
		t.Fatalf("import = %v", out)
	}
	get := json.RawMessage(`{"op":"get_document","scope":"user","path":"/board/notes"}`)
	res, err := f.docs.Execute(sessionCtx(twinOperator), get)
	if err != nil || res.IsError || !strings.Contains(res.Text, docID) {
		t.Fatalf("the importer's get_document = %+v %v", res, err)
	}
	if res, err := f.docs.Execute(sessionCtx(twinOwner), get); err != nil || !res.IsError {
		t.Errorf("the run owner's user scope holds the importer's document: %+v %v", res, err)
	}
}

// download and upload, and paths outside the inbox, are refused before the
// run is read.
func TestFileTwin_RefusesTransfersAndPathsOutsideTheInbox(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	f.capture(t, runID, "acme", "a.txt", "http", []byte("first file\n"), true)
	for _, in := range []map[string]any{
		{"run_id": runID, "op": "download", "url": "https://example.com/a.pdf"},
		{"run_id": runID, "op": "upload", "from": "volume:inbox:documents/a.txt", "url": "https://example.com/u"},
		{"run_id": runID, "op": "stat", "path": "volume:work:secret.txt"},
		{"run_id": runID, "op": "ls", "path": "volume:"},
		{"run_id": runID, "op": "keep", "from": "volume:work:secret.txt", "to": "volume:docs:x"},
		{"run_id": "../etc", "op": "ls"},
	} {
		if _, text, isErr := f.call(t, twinAdmin, in); !isErr {
			t.Errorf("%v was not refused: %s", in, text)
		}
	}
}

// fileTwinPNG is a valid 1x1 PNG.
func fileTwinPNG() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
}
