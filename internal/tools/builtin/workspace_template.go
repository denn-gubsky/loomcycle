package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// Workspace templates (RFC GW §5.5, P6). Saving and deleting need
// workspace.templates; a tenant template also the tenant value in
// memory_scopes, as other tenant-wide writes do. Opening from one needs
// open (Workspace.open). The tenant is always the run's own (D-P6-24).

// tenantTemplatesAllowed reports whether the run may write tenant-wide: the
// tenant value in its memory_scopes, and not an isolated member.
func tenantTemplatesAllowed(ctx context.Context) bool {
	return contains(tools.PolicyOf(ctx).Memory.AllowedScopes, string(store.MemoryScopeTenant)) &&
		tools.ConfineIsolatedScope(ctx, store.MemoryScopeTenant) == nil
}

// checkTemplateScope refuses a missing or unknown scope, and a tenant one the
// run may not write.
func checkTemplateScope(ctx context.Context, scope string) (tools.Result, bool) {
	switch scope {
	case store.WorkspaceTemplateUser:
		return tools.Result{}, true
	case store.WorkspaceTemplateTenant:
		if !tenantTemplatesAllowed(ctx) {
			return workspaceRefusal("not_granted", "a tenant template needs the tenant value in this agent's memory_scopes"), false
		}
		return tools.Result{}, true
	case "":
		return workspaceRefusal("invalid_argument", "scope is required: user or tenant"), false
	}
	return workspaceRefusal("invalid_argument", fmt.Sprintf("scope %q: want user or tenant", scope)), false
}

// saveTemplate saves a checkpoint of the run's own workspace row as a
// template (D-P6-26).
func (w *Workspace) saveTemplate(ctx context.Context, o lifecycle.Owner, row store.WorkspaceRow, in workspaceInput) tools.Result {
	if in.Checkpoint == "" || in.As == "" {
		return workspaceRefusal("invalid_argument", "save_template needs checkpoint (checkpoints lists them) and as, the template's name")
	}
	if r, ok := checkTemplateScope(ctx, in.Scope); !ok {
		return r
	}
	t, err := w.Manager.SaveTemplate(ctx, o, row, in.Checkpoint, in.As, in.Scope)
	if err != nil {
		if errors.Is(err, lifecycle.ErrNotFound) {
			return workspaceError(err, in.Name)
		}
		return templateError(err)
	}
	return workspaceOK(templateView(t))
}

// deleteTemplate deletes a template the run may delete (D-P6-27): its
// user's own, or with the tenant grant, its tenant's.
func (w *Workspace) deleteTemplate(ctx context.Context, o lifecycle.Owner, in workspaceInput) tools.Result {
	if in.Template == "" {
		return workspaceRefusal("invalid_argument", "delete_template needs template, the template's name")
	}
	if r, ok := checkTemplateScope(ctx, in.Scope); !ok {
		return r
	}
	t, err := w.Manager.GetTemplate(ctx, o, in.Scope, in.Template)
	if err != nil {
		return templateError(err)
	}
	if err := w.Manager.DeleteTemplate(ctx, o, t); err != nil {
		return templateError(err)
	}
	return workspaceOK(map[string]any{"template": t.Name, "scope": t.Scope, "deleted": true})
}

// templates lists the templates the run may open (D-P6-28).
func (w *Workspace) templates(ctx context.Context, o lifecycle.Owner) tools.Result {
	list, err := w.Manager.Templates(ctx, o)
	if err != nil {
		return templateError(err)
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		if templateReachable(ctx, t) {
			out = append(out, templateView(t))
		}
	}
	return workspaceOK(map[string]any{"templates": out})
}

// templateReachable reports whether the run may use t: an isolated member
// (substrate:user) reaches no tenant-shared data, so no tenant template
// (D-P6-39).
func templateReachable(ctx context.Context, t store.WorkspaceTemplateRow) bool {
	return t.Scope != store.WorkspaceTemplateTenant || tools.ConfineIsolatedScope(ctx, store.MemoryScopeTenant) == nil
}

// templateView is a template as the model sees it: never its id, digest,
// author or the run that saved it.
func templateView(t store.WorkspaceTemplateRow) map[string]any {
	return map[string]any{"template": t.Name, "scope": t.Scope, "bytes": t.Bytes, "toolchains": t.Toolchains,
		"created_at": t.CreatedAt}
}

// templateError explains a template op's failure; anything else is the
// workspace's.
func templateError(err error) tools.Result {
	switch {
	case errors.Is(err, lifecycle.ErrTemplateNotFound):
		return workspaceRefusal("template_not_found", "no template of that name you may use (templates lists them)")
	case errors.Is(err, lifecycle.ErrTemplateLimit):
		return workspaceRefusal("template_limit", err.Error())
	case errors.Is(err, lifecycle.ErrTemplateTooLarge):
		return workspaceRefusal("template_too_large", err.Error())
	case errors.Is(err, lifecycle.ErrTemplateScope):
		return workspaceRefusal("invalid_argument", err.Error())
	case errors.Is(err, lifecycle.ErrTemplatesOff):
		return workspaceRefusal("templates_off", "this runtime keeps no workspace templates")
	case errors.Is(err, lifecycle.ErrTemplateExists):
		return workspaceRefusal("exists", err.Error()+"; save it as another name, or delete that template first")
	}
	return workspaceError(err, "")
}
