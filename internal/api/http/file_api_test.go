package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
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

// mkTwinVolume records a dynamic volume of tenant and makes its directory.
func (f *fileTwinFixture) mkVolume(t *testing.T, tenant, name, mode string) string {
	t.Helper()
	dir, _, err := dynvol.Provision(f.dynRoot, tenant, name)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"path": dir, "mode": mode})
	if _, err := f.st.VolumeDefCreate(context.Background(), store.VolumeDefRow{TenantID: tenant, Name: name, Definition: body}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// keep copies into a read-write dynamic volume of the CALLER's tenant, at the
// path the runtime derives for it. A read-only one, a configured one and
// another tenant's are refused.
func TestFileTwin_KeepCopiesIntoTheCallersDynamicVolume(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "minutes.txt", "http", []byte("the minutes\n"), true)
	docs := f.mkVolume(t, "acme", "docs", "rw")
	f.mkVolume(t, "acme", "archive", "ro")
	f.mkVolume(t, "other", "theirs", "rw")

	if err := os.MkdirAll(filepath.Join(docs, "board"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "keep", "from": rec.Ref, "to": "volume:docs:board/"})
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
		if _, text, isErr := f.call(t, twinOwner, map[string]any{"run_id": runID, "op": "keep", "from": rec.Ref, "to": to}); !isErr || !strings.Contains(text, want) {
			t.Errorf("keep to %s = %s, want %q", to, text, want)
		}
	}
}

// keep writes into the tenant's volumes, so a principal that only reads the
// run may not: not even with overwrite, and nothing is written. The owner's
// access rules apply too: a volume its rules let it only read is refused.
func TestFileTwin_KeepNeedsTheOwnerOrAnAdmin(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "minutes.txt", "http", []byte("the minutes\n"), true)
	docs := f.mkVolume(t, "acme", "docs", "rw")
	target := filepath.Join(docs, "minutes.txt")
	if err := os.WriteFile(target, []byte("theirs\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, in := range []map[string]any{
		{"run_id": runID, "op": "keep", "from": rec.Ref, "to": "volume:docs:new.txt"},
		{"run_id": runID, "op": "keep", "from": rec.Ref, "to": "volume:docs:minutes.txt", "overwrite": true},
	} {
		if _, text, isErr := f.call(t, twinOperator, in); !isErr || !strings.Contains(text, "owner or an admin") {
			t.Errorf("keep %v by a reader = %s", in, text)
		}
	}
	if _, err := os.Stat(filepath.Join(docs, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("a refused keep wrote a file: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "theirs\n" {
		t.Errorf("a refused overwrite replaced the file: %q", b)
	}

	// The owner whose rules deny writing to the volume.
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "acme", ID: "alice"},
		Bindings: []authz.Binding{{ACL: "a", SubjectKind: authz.SubjectTenant}},
		ACLs: map[string]authz.ACLVersion{"a": {ACL: authz.ACL{Name: "a", Rules: []authz.Rule{
			{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"volume:docs"}},
		}}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	ruled := twinOwner
	ruled.Effective = eff
	if _, text, isErr := f.call(t, ruled, map[string]any{"run_id": runID, "op": "keep", "from": rec.Ref, "to": "volume:docs:new.txt"}); !isErr || !strings.Contains(text, "access rules") {
		t.Errorf("keep against a write deny = %s", text)
	}
	if _, err := os.Stat(filepath.Join(docs, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("a keep the rules deny wrote a file: %v", err)
	}
	// The admin may.
	if _, text, isErr := f.call(t, twinAdmin, map[string]any{"run_id": runID, "op": "keep", "from": rec.Ref, "to": "volume:shared:new.txt"}); isErr && !strings.Contains(text, "no volume named") {
		t.Errorf("admin keep = %s", text)
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

// The input is decoded strictly, and the File tool is handed the input that
// was authorised, never the caller's bytes: a key that differs from a known
// one only by case, a key given twice, or a key the twin does not take is
// refused, so no call can be authorised as one op and run as another.
func TestFileTwin_DecodesStrictly(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "a.txt", "http", []byte("first file\n"), true)
	dir := dynvol.DerivedEphemeralPath(f.dynRoot, runID, inbox.Name)
	raw := func(p auth.Principal, body string) (string, bool) {
		t.Helper()
		res, err := f.srv.File(sessionCtx(p), json.RawMessage(body))
		if err != nil {
			t.Fatalf("File(%s): %v", body, err)
		}
		return res.Text, res.IsError
	}
	for body, want := range map[string]string{
		`{"run_id":"` + runID + `","op":"rm","OP":"stat","path":"` + rec.Ref + `"}`:   `"OP"`,
		`{"run_id":"` + runID + `","op":"stat","OP":"rm","path":"` + rec.Ref + `"}`:   `"OP"`,
		`{"run_id":"` + runID + `","op":"stat","op":"rm","path":"` + rec.Ref + `"}`:   `"op" more than once`,
		`{"run_id":"` + runID + `","op":"ls","url":"https://example.com/"}`:           `"url"`,
		`{"run_id":"` + runID + `","op":"ls","bogus":1}`:                              `"bogus"`,
		`{"run_id":"` + runID + `","Run_ID":"other","op":"ls"}`:                       `"Run_ID"`,
		`{"run_id":"` + runID + `","op":"stat","path":"` + rec.Ref + `"} {"op":"rm"}`: "trailing",
	} {
		if text, isErr := raw(twinOperator, body); !isErr || !strings.Contains(text, want) {
			t.Errorf("%s = %s, want a refusal naming %s", body, text, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rec.Path))); err != nil {
		t.Fatalf("a refused call removed the file: %v", err)
	}
	if text, isErr := raw(twinOperator, `{"run_id":"`+runID+`","op":"stat","path":"`+rec.Ref+`"}`); isErr || !strings.Contains(text, rec.SHA256) {
		t.Errorf("a valid stat = %s", text)
	}
}

// The keys the strict decoder admits are exactly the ones the struct it
// authorises carries, so a field added to one cannot be dropped by the other.
func TestFileTwin_FieldsAreTheArgs(t *testing.T) {
	var got []string
	ty := reflect.TypeOf(fileTwinArgs{})
	got = append(got, "run_id")
	for i := range ty.NumField() {
		name, _, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ",")
		got = append(got, name)
	}
	if !reflect.DeepEqual(got, connector.FileTwinFields) {
		t.Errorf("fileTwinArgs fields %v, connector.FileTwinFields %v", got, connector.FileTwinFields)
	}
}

// A run the caller may not read is the same not-found as a missing run, for
// the reading ops and for import alike: whether it is another tenant's, or a
// colleague's run seen by an isolated member of the same tenant (who may read
// only its own). The isolated member's own run stays reachable.
func TestFileTwin_UnreadableRunIsNotFoundForEveryOp(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "notes.md", "http", []byte("# Notes\n\nBody.\n"), true)
	isolated := auth.Principal{TenantID: "acme", Subject: "carol", Scopes: []string{auth.ScopeUser, auth.ScopeRunsRead}}

	for _, c := range []struct {
		name string
		p    auth.Principal
	}{{"another tenant", twinOutsider}, {"an isolated member who does not own the run", isolated}} {
		for _, in := range []map[string]any{
			{"op": "ls"},
			{"op": "stat", "path": rec.Ref},
			{"op": "import", "path": rec.Ref},
		} {
			in["run_id"] = runID
			_, got, isErr := f.call(t, c.p, in)
			in["run_id"] = "run_nosuch"
			_, missing, _ := f.call(t, c.p, in)
			if !isErr || strings.Replace(got, runID, "X", 1) != strings.Replace(missing, "run_nosuch", "X", 1) {
				t.Errorf("%s, %v: %q, want the missing run's %q", c.name, in["op"], got, missing)
			}
		}
	}
	own := f.run(t, "acme", "carol", "")
	f.capture(t, own, "acme", "mine.txt", "http", []byte("mine\n"), true)
	if _, text, isErr := f.call(t, isolated, map[string]any{"run_id": own, "op": "ls"}); isErr {
		t.Errorf("the isolated member's own run: %s", text)
	}
}

// A run's inbox is listed on the Volumes page like any ephemeral volume, to
// its tenant and not to another: the row the first capture writes carries the
// run's tenant, which is what the listing filters on.
func TestFileTwin_InboxIsListedAsTheTenantsEphemeralVolume(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	f.capture(t, runID, "acme", "a.txt", "http", []byte("first file\n"), true)
	list := func(tenant string) []ephemeralVolumeEntry {
		rec := httptest.NewRecorder()
		f.srv.handleListEphemeralVolumes(rec, reqWithTenant("/v1/_volumes/ephemeral", tenant))
		var resp ephemeralVolumesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		return resp.Entries
	}
	got := list("acme")
	if len(got) != 1 || got[0].Name != inbox.Name || got[0].RootRunID != runID || got[0].Mode != "rw" {
		t.Errorf("acme's ephemeral volumes = %+v, want the run's inbox", got)
	}
	if other := list("evil"); len(other) != 0 {
		t.Errorf("another tenant sees %+v", other)
	}
}

// An isolated user is confined to its own scope, and a dynamic volume is
// shared by its tenant. Inside a run the agent definition picks the volumes
// it writes; off-run keep would let the user pick any of them, so an
// isolated owner may not keep, and nothing is written.
func TestFileTwin_KeepRefusesAnIsolatedOwner(t *testing.T) {
	f := newFileTwinFixture(t)
	runID := f.run(t, "acme", "alice", "")
	rec := f.capture(t, runID, "acme", "minutes.txt", "http", []byte("the minutes\n"), true)
	docs := f.mkVolume(t, "acme", "docs", "rw")

	isolated := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeUser, auth.ScopeRunsCreate, auth.ScopeRunsRead}}
	if _, text, isErr := f.call(t, isolated, map[string]any{"run_id": runID, "op": "keep", "from": rec.Ref, "to": "volume:docs:new.txt"}); !isErr || !strings.Contains(text, "isolated") {
		t.Errorf("keep by an isolated owner = %s", text)
	}
	if _, err := os.Stat(filepath.Join(docs, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("a refused keep wrote a file: %v", err)
	}
}
