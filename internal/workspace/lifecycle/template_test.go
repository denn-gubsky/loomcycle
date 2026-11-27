package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// templateFixture is a fixture whose manager keeps templates, with alice's
// workspace "work" holding notes.txt and a checkpoint of it.
type templateFixture struct {
	*fixture
	dir   string
	alice Owner
	src   store.WorkspaceRow
	ck    string
	log   *auditLog
}

func newTemplateFixture(t *testing.T, l Limits) *templateFixture {
	t.Helper()
	f := newFixture(t, l, worker.Options{})
	dir := filepath.Join(t.TempDir(), "templates")
	f.m.SetTemplateDir(dir)
	log := &auditLog{}
	f.m.SetAudit(log)
	alice := f.run(t, "alice")
	src := f.open(t, alice, "work")
	if err := os.WriteFile(filepath.Join(f.h.Dir(src.ID), "notes.txt"), []byte("prepared"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.h.Dir(src.ID), "venv"), 0o700); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(f.h.Dir(src.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.h.Dir(src.ID), "venv", "pyvenv.cfg"), []byte("home = "+real+"/venv/bin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ck, err := f.m.Checkpoint(context.Background(), src, "ready")
	if err != nil {
		t.Fatal(err)
	}
	return &templateFixture{fixture: f, dir: dir, alice: alice, src: src, ck: ck.ID, log: log}
}

func (f *templateFixture) archives(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(f.dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func (f *templateFixture) read(t *testing.T, o Owner, name, path string) string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := f.m.GetFile(context.Background(), o, name, workspace.GetArgs{Path: path}, &buf, 1<<20); err != nil {
		t.Fatalf("read %s in %s: %v", path, name, err)
	}
	return buf.String()
}

// RFC GW §11 "Templates": a workspace opened from a template on a worker
// that never held it has the template's files. The archive rests in the
// core's folder, the row names its digest, and the second worker is sent
// the archive once.
func TestTemplate_OpensOnAWorkerThatNeverHeldIt(t *testing.T) {
	f := newTemplateFixture(t, Limits{MaxPerRun: 4})
	ctx := context.Background()
	tpl, err := f.m.SaveTemplate(ctx, f.alice, f.src, f.ck, "prepared", store.WorkspaceTemplateUser)
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Scope != store.WorkspaceTemplateUser || tpl.ScopeID != "alice" || tpl.TenantID != "acme" ||
		!workspace.ValidTemplateDigest(tpl.Digest) || tpl.Bytes <= 0 || len(tpl.Toolchains) == 0 {
		t.Errorf("the template row = %+v", tpl)
	}
	if got := f.archives(t); len(got) != 1 || got[0] != tpl.ID+".tar.gz" {
		t.Errorf("archives = %v", got)
	}

	// The first worker goes; the second, which never held the template, is
	// the only place left.
	f.h.AddWorker("w2", 4)
	f.h.StopWorker()
	later := f.run(t, "alice")
	found, err := f.m.FindTemplate(ctx, later, "prepared")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := f.m.Open(ctx, later, OpenRequest{Name: "copy", Template: &found})
	if err != nil {
		t.Fatalf("open from the template: %v", err)
	}
	if ws.WorkerID == f.src.WorkerID {
		t.Fatalf("opened on the worker that packed it")
	}
	if got := f.read(t, later, "copy", "notes.txt"); got != "prepared" {
		t.Errorf("the opened workspace's file = %q", got)
	}
	// The host engine moves a Python environment's paths to the new
	// workspace's folder (RFC GW §4.3), from whichever worker made it.
	cfg := f.read(t, later, "copy", "venv/pyvenv.cfg")
	if strings.Contains(cfg, f.src.ID) || strings.Contains(cfg, ".loomcycle-template-root") || !strings.Contains(cfg, ws.ID+"/venv") {
		t.Errorf("the opened workspace's pyvenv.cfg = %q", cfg)
	}
	// A second open is served from the worker's cache.
	if _, err := f.m.Open(ctx, later, OpenRequest{Name: "again", Template: &found}); err != nil {
		t.Fatalf("a second open: %v", err)
	}
	if got := f.read(t, later, "again", "notes.txt"); got != "prepared" {
		t.Errorf("the second workspace's file = %q", got)
	}
}

// D-P6-24: a user template is found only for its author's runs (or its
// subject's principal), a tenant one for every run of the tenant, never in
// another tenant; the delete lookup is held to the same, and a row of another
// tenant cannot be deleted.
func TestTemplate_NeverCrossesATenantOrAnotherUser(t *testing.T) {
	f := newTemplateFixture(t, Limits{MaxPerRun: 4})
	ctx := context.Background()
	mine, err := f.m.SaveTemplate(ctx, f.alice, f.src, f.ck, "mine", store.WorkspaceTemplateUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.SaveTemplate(ctx, f.alice, f.src, f.ck, "shared", store.WorkspaceTemplateTenant); err != nil {
		t.Fatal(err)
	}
	bob := f.run(t, "bob")
	if _, err := f.m.FindTemplate(ctx, bob, "mine"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("bob finding alice's user template = %v, want ErrTemplateNotFound", err)
	}
	if got, err := f.m.FindTemplate(ctx, bob, "shared"); err != nil || got.Scope != store.WorkspaceTemplateTenant {
		t.Errorf("bob finding the tenant template = %+v, %v", got, err)
	}
	if _, err := f.m.GetTemplate(ctx, bob, store.WorkspaceTemplateUser, "mine"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("bob's delete lookup of alice's template = %v, want ErrTemplateNotFound", err)
	}
	if list, err := f.m.Templates(ctx, bob); err != nil || len(list) != 1 || list[0].Name != "shared" {
		t.Errorf("bob's list = %+v, %v", list, err)
	}
	globex := f.alice
	globex.TenantID = "globex"
	for _, name := range []string{"mine", "shared"} {
		if _, err := f.m.FindTemplate(ctx, globex, name); !errors.Is(err, ErrTemplateNotFound) {
			t.Errorf("another tenant finding %q = %v, want ErrTemplateNotFound", name, err)
		}
	}
	if list, _ := f.m.Templates(ctx, globex); len(list) != 0 {
		t.Errorf("another tenant's list = %+v", list)
	}
	if err := f.m.DeleteTemplate(ctx, globex, mine); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("another tenant deleting the row = %v, want ErrTemplateNotFound", err)
	}
	// A principal (ruled 2026-11-25) is its token's tenant and subject: it
	// finds the tenant's templates and its subject's own, never another's.
	p := PrincipalOwner("acme", "bob", "tok")
	if _, err := f.m.FindTemplate(ctx, p, "mine"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("another subject's principal finding alice's template = %v, want ErrTemplateNotFound", err)
	}
	if got, err := f.m.FindTemplate(ctx, p, "shared"); err != nil || got.Scope != store.WorkspaceTemplateTenant {
		t.Errorf("a principal finding its tenant's template = %+v, %v", got, err)
	}
	if _, err := f.m.FindTemplate(ctx, PrincipalOwner("globex", "alice", "tok"), "shared"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("another tenant's principal finding the template = %v, want ErrTemplateNotFound", err)
	}
	noUser := f.run(t, "")
	if _, err := f.m.FindTemplate(ctx, noUser, "mine"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("a run with no user finding a user template = %v", err)
	}
	// An open is refused a template row of another tenant, however it was
	// read.
	if _, err := f.m.Open(ctx, globex, OpenRequest{Name: "x", Template: &mine}); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("an open of another tenant's template = %v, want ErrTemplateNotFound", err)
	}
}

// A save takes only the caller's own workspace, never overwrites a held
// name, a user template needs a user, and the tenant's bound holds; a
// refused save keeps no archive.
func TestTemplate_SaveRefusals(t *testing.T) {
	f := newTemplateFixture(t, Limits{MaxPerRun: 4, MaxTemplates: 1})
	ctx := context.Background()
	if _, err := f.m.SaveTemplate(ctx, f.run(t, "bob"), f.src, f.ck, "stolen", store.WorkspaceTemplateUser); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob saving alice's workspace = %v, want ErrNotFound", err)
	}
	noUser := f.alice
	noUser.UserID = ""
	if _, err := f.m.SaveTemplate(ctx, noUser, f.src, f.ck, "x", store.WorkspaceTemplateUser); !errors.Is(err, ErrTemplateScope) {
		t.Errorf("a user template with no user = %v, want ErrTemplateScope", err)
	}
	if _, err := f.m.SaveTemplate(ctx, f.alice, f.src, f.ck, "x", "everyone"); !errors.Is(err, ErrTemplateScope) {
		t.Errorf("an unknown scope = %v, want ErrTemplateScope", err)
	}
	if _, err := f.m.SaveTemplate(ctx, f.alice, f.src, f.ck, "one", store.WorkspaceTemplateTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.SaveTemplate(ctx, f.alice, f.src, f.ck, "one", store.WorkspaceTemplateTenant); !errors.Is(err, ErrTemplateExists) {
		t.Errorf("a held name = %v, want ErrTemplateExists", err)
	}
	if _, err := f.m.SaveTemplate(ctx, f.alice, f.src, f.ck, "two", store.WorkspaceTemplateUser); !errors.Is(err, ErrTemplateLimit) {
		t.Errorf("past the tenant's bound = %v, want ErrTemplateLimit", err)
	}
	if got := f.archives(t); len(got) != 1 {
		t.Errorf("archives after refused saves = %v", got)
	}
}

// An archive past workspace.template.max_mb is refused and nothing is kept.
func TestTemplate_TooLargeKeepsNothing(t *testing.T) {
	f := newTemplateFixture(t, Limits{MaxPerRun: 4, TemplateMaxBytes: 16})
	if _, err := f.m.SaveTemplate(context.Background(), f.alice, f.src, f.ck, "big", store.WorkspaceTemplateUser); !errors.Is(err, ErrTemplateTooLarge) {
		t.Fatalf("a template past its bound = %v, want ErrTemplateTooLarge", err)
	}
	if got := f.archives(t); len(got) != 0 {
		t.Errorf("archives = %v", got)
	}
	if n, _ := f.st.WorkspaceTemplateCount(context.Background(), "acme"); n != 0 {
		t.Errorf("rows = %d", n)
	}
}

// Delete removes the row and the archive; a save and a delete each write
// one audit line naming the template and the run (D-P6-35).
func TestTemplate_DeleteRemovesRowAndArchiveAndBothAreAudited(t *testing.T) {
	f := newTemplateFixture(t, Limits{MaxPerRun: 4})
	ctx := context.Background()
	if _, err := f.m.SaveTemplate(ctx, f.alice, f.src, f.ck, "py", store.WorkspaceTemplateTenant); err != nil {
		t.Fatal(err)
	}
	bob := f.run(t, "bob")
	r, err := f.m.GetTemplate(ctx, bob, store.WorkspaceTemplateTenant, "py")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.DeleteTemplate(ctx, bob, r); err != nil {
		t.Fatal(err)
	}
	if got := f.archives(t); len(got) != 0 {
		t.Errorf("archives after delete = %v", got)
	}
	if _, err := f.m.FindTemplate(ctx, f.alice, "py"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("found after delete: %v", err)
	}
	if acts := strings.Join(f.log.actions(), ","); acts != "workspace_template_save,workspace_template_delete" {
		t.Fatalf("audit = %s", acts)
	}
	save, del := f.log.evs[0], f.log.evs[1]
	if save.WorkspaceOwnerRunID != f.alice.RunID || save.WorkspaceID != f.src.ID || save.TargetName != "workspace_template:py" ||
		save.WorkspaceTemplateDigest != r.Digest || save.TargetTenant != "acme" {
		t.Errorf("save line = %+v", save)
	}
	if del.WorkspaceOwnerRunID != bob.RunID || del.WorkspaceTemplateID != r.ID {
		t.Errorf("delete line = %+v", del)
	}
}

// A user holds at most MaxTemplatesPerUser user templates, so one member
// cannot fill the tenant's bound; a principal and a run of one subject share
// the count; another user can still save; the tenant bound still holds.
func TestTemplate_AUserHoldsAtMostItsOwnShare(t *testing.T) {
	f := newTemplateFixture(t, Limits{MaxPerRun: 4, MaxTemplates: 4, MaxTemplatesPerUser: 2})
	ctx := context.Background()
	if _, err := f.m.SaveTemplate(ctx, f.alice, f.src, f.ck, "a1", store.WorkspaceTemplateUser); err != nil {
		t.Fatal(err)
	}
	// The same subject over the MCP server: its own workspace and checkpoint.
	p := PrincipalOwner("acme", "alice", "tok")
	psrc := f.open(t, p, "pw")
	pck, err := f.m.Checkpoint(ctx, psrc, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.SaveTemplate(ctx, p, psrc, pck.ID, "a2", store.WorkspaceTemplateUser); err != nil {
		t.Fatal(err)
	}
	for _, try := range []struct {
		who string
		o   Owner
		src store.WorkspaceRow
		ck  string
	}{{"the run", f.alice, f.src, f.ck}, {"the principal", p, psrc, pck.ID}} {
		_, err := f.m.SaveTemplate(ctx, try.o, try.src, try.ck, "a3", store.WorkspaceTemplateUser)
		if !errors.Is(err, ErrTemplateLimit) || !strings.Contains(err.Error(), "max_per_user") {
			t.Errorf("a third user template by %s = %v, want ErrTemplateLimit naming max_per_user", try.who, err)
		}
	}
	// Tenant templates are not a user's share.
	if _, err := f.m.SaveTemplate(ctx, f.alice, f.src, f.ck, "t1", store.WorkspaceTemplateTenant); err != nil {
		t.Errorf("a tenant template past alice's user share: %v", err)
	}
	bob := f.run(t, "bob")
	bsrc := f.open(t, bob, "work")
	bck, err := f.m.Checkpoint(ctx, bsrc, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.SaveTemplate(ctx, bob, bsrc, bck.ID, "b1", store.WorkspaceTemplateUser); err != nil {
		t.Errorf("another user's save: %v", err)
	}
	// Four held: the tenant bound refuses bob's second.
	if _, err := f.m.SaveTemplate(ctx, bob, bsrc, bck.ID, "b2", store.WorkspaceTemplateUser); !errors.Is(err, ErrTemplateLimit) || !strings.Contains(err.Error(), "max_per_tenant") {
		t.Errorf("past the tenant bound = %v, want ErrTemplateLimit naming max_per_tenant", err)
	}
}
