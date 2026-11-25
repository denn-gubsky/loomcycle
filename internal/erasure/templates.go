package erasure

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Workspace templates (RFC GW §5.5, D-P6-38 ruled 2026-11-25). A subject's
// USER templates are theirs: an erasure deletes each row and its archive
// (TemplateArchive). A TENANT template the subject saved is the tenant's
// content, open to every user of it, and stays; it is counted and named as
// retained, never deleted by a subject's erasure.

const templatesPlane = "workspace_templates"

// userTemplates are the subject's user templates in the tenant.
func (s *Service) userTemplates(ctx context.Context, tenant, subject string) ([]store.WorkspaceTemplateRow, error) {
	return s.Store.WorkspaceTemplateList(ctx, tenant, store.WorkspaceTemplateUser, subject)
}

// tenantTemplatesBy counts the tenant templates the subject saved.
func (s *Service) tenantTemplatesBy(ctx context.Context, tenant, subject string) (int, error) {
	rows, err := s.Store.WorkspaceTemplateList(ctx, tenant, store.WorkspaceTemplateTenant, "")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if r.AuthorUser == subject {
			n++
		}
	}
	return n, nil
}

func keptTenantTemplates(n int) string {
	return fmt.Sprintf("%d tenant workspace template(s) this subject saved are kept: a tenant template is the tenant's content, "+
		"open to every user of it, and only a tenant operator's delete_template removes it.", n)
}

// eraseTemplates deletes the subject's user templates, rows then archives;
// a dry run counts them. Execute's template step.
func (s *Service) eraseTemplates(ctx context.Context, tenant, subject string, dryRun bool, res *Result, fail func(string, error)) {
	if n, err := s.tenantTemplatesBy(ctx, tenant, subject); err != nil {
		fail(templatesPlane, err)
	} else if n > 0 {
		res.Retained["workspace_templates_tenant"] = keptTenantTemplates(n)
	}
	rows, err := s.userTemplates(ctx, tenant, subject)
	if err != nil {
		fail(templatesPlane, err)
		return
	}
	if dryRun {
		res.Deleted[templatesPlane] = len(rows)
		return
	}
	if s.TemplateArchive == nil && len(rows) > 0 {
		res.Retained["workspace_template_archives"] = "this transport was given no template folder, so the archives of the deleted templates were not removed"
	}
	for _, r := range rows {
		ok, err := s.Store.WorkspaceTemplateDelete(ctx, tenant, r.ID)
		if err != nil {
			fail(templatesPlane, err)
			continue
		}
		if ok {
			res.Deleted[templatesPlane]++
		}
		if s.TemplateArchive == nil {
			continue
		}
		path, err := s.TemplateArchive(r.ID)
		if err != nil {
			fail(templatesPlane, err)
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			fail(templatesPlane, err)
		}
	}
}
