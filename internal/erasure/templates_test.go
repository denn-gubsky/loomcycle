package erasure_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedTemplates writes alice's two user templates, bob's one, and a tenant
// template alice saved, each with its archive file under dir.
func seedTemplates(t *testing.T, s *erasure.Service, dir string) {
	t.Helper()
	ctx := context.Background()
	for _, r := range []store.WorkspaceTemplateRow{
		{ID: "tpl_aaaaaaaaaaaaaaaaaaaaaaa1", TenantID: "acme", Scope: store.WorkspaceTemplateUser, ScopeID: "alice", Name: "one", AuthorUser: "alice"},
		{ID: "tpl_aaaaaaaaaaaaaaaaaaaaaaa2", TenantID: "acme", Scope: store.WorkspaceTemplateUser, ScopeID: "alice", Name: "two", AuthorUser: "alice"},
		{ID: "tpl_bbbbbbbbbbbbbbbbbbbbbbb1", TenantID: "acme", Scope: store.WorkspaceTemplateUser, ScopeID: "bob", Name: "one", AuthorUser: "bob"},
		{ID: "tpl_ccccccccccccccccccccccc1", TenantID: "acme", Scope: store.WorkspaceTemplateTenant, Name: "shared", AuthorUser: "alice"},
		{ID: "tpl_ddddddddddddddddddddddd1", TenantID: "globex", Scope: store.WorkspaceTemplateUser, ScopeID: "alice", Name: "one", AuthorUser: "alice"},
	} {
		r.Digest, r.Bytes = "d-"+r.ID, 3
		if _, err := s.Store.WorkspaceTemplateCreate(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, r.ID+".tar.gz"), []byte("tgz"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func archives(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, strings.TrimSuffix(e.Name(), ".tar.gz"))
	}
	return out
}

// RFC GW D-P6-38 (ruled 2026-11-25): a subject's erasure reports and deletes
// the subject's user workspace templates, rows and archives, in its tenant
// only; a dry run deletes nothing; a tenant template the subject saved stays
// (tenant content) and is named as retained.
func TestService_ErasesTheSubjectsUserTemplates(t *testing.T) {
	s := newSvc(t)
	dir := t.TempDir()
	s.TemplateArchive = func(id string) (string, error) { return filepath.Join(dir, id+".tar.gz"), nil }
	seedTemplates(t, s, dir)
	ctx := context.Background()

	rep, err := s.Report(ctx, "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Tier1.Counts["workspace_templates"]; got != 2 {
		t.Errorf("report workspace_templates = %d, want 2", got)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "1 tenant workspace template") {
		t.Errorf("report notes do not name the kept tenant template: %q", rep.Notes)
	}

	dry, err := s.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Deleted["workspace_templates"] != 2 {
		t.Errorf("dry run workspace_templates = %d, want 2", dry.Deleted["workspace_templates"])
	}
	if got := archives(t, dir); len(got) != 5 {
		t.Errorf("a dry run removed archives: %v", got)
	}
	if n, _ := s.Store.WorkspaceTemplateCount(ctx, "acme"); n != 4 {
		t.Errorf("a dry run removed rows: acme holds %d", n)
	}

	res, err := s.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", Confirm: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted["workspace_templates"] != 2 {
		t.Errorf("workspace_templates deleted = %d, want 2", res.Deleted["workspace_templates"])
	}
	if _, ok := res.Retained["workspace_templates_tenant"]; !ok {
		t.Errorf("retained does not name the tenant template: %v", res.Retained)
	}
	got := strings.Join(archives(t, dir), ",")
	if strings.Contains(got, "aaaa") || !strings.Contains(got, "bbbb") || !strings.Contains(got, "cccc") || !strings.Contains(got, "dddd") {
		t.Errorf("archives left = %s; want alice's user ones gone, bob's, the tenant one and globex's kept", got)
	}
	if rows, _ := s.Store.WorkspaceTemplateList(ctx, "acme", store.WorkspaceTemplateUser, "alice"); len(rows) != 0 {
		t.Errorf("alice's template rows left: %+v", rows)
	}
	if rows, _ := s.Store.WorkspaceTemplateList(ctx, "globex", store.WorkspaceTemplateUser, "alice"); len(rows) != 1 {
		t.Errorf("the same subject's template in another tenant = %+v, want kept", rows)
	}
}
