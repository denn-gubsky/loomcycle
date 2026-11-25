package store

import (
	"context"
	"time"
)

// The scopes of a workspace template (RFC GW §5.5).
const (
	WorkspaceTemplateUser   = "user"
	WorkspaceTemplateTenant = "tenant"
)

// WorkspaceTemplateRow is one saved workspace template. Its archive is a
// file under the core's data directory named by ID; the row holds only what
// finds and checks it.
type WorkspaceTemplateRow struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id,omitempty"`
	// Scope is WorkspaceTemplateUser or WorkspaceTemplateTenant; ScopeID the
	// author's user id for a user template, "" for a tenant one.
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id,omitempty"`
	Name    string `json:"name"`
	// Toolchains and Engine are the worker's the template was saved on: an
	// open is placed only on a worker carrying the toolchains.
	Toolchains []string `json:"toolchains"`
	Engine     string   `json:"engine,omitempty"`
	// Digest is the SHA-256 (hex) of the archive, Bytes its size.
	Digest     string    `json:"digest"`
	Bytes      int64     `json:"bytes"`
	AuthorUser string    `json:"author_user,omitempty"`
	AuthorRun  string    `json:"author_run,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// WorkspaceTemplateStore persists workspace templates (RFC GW P6). Every
// read takes the tenant, so another tenant's row is absent.
type WorkspaceTemplateStore interface {
	// WorkspaceTemplateCreate inserts a template, stamping CreatedAt. A
	// (tenant, scope, scope_id, name) already held, or a held id, is
	// *ErrConflict: a template is never overwritten.
	WorkspaceTemplateCreate(ctx context.Context, row WorkspaceTemplateRow) (WorkspaceTemplateRow, error)
	// WorkspaceTemplateGet returns *ErrNotFound on a miss.
	WorkspaceTemplateGet(ctx context.Context, tenantID, scope, scopeID, name string) (WorkspaceTemplateRow, error)
	// WorkspaceTemplateList returns the tenant's templates of one scope (and
	// scope id), ordered by name.
	WorkspaceTemplateList(ctx context.Context, tenantID, scope, scopeID string) ([]WorkspaceTemplateRow, error)
	// WorkspaceTemplateCount counts the tenant's templates, both scopes.
	WorkspaceTemplateCount(ctx context.Context, tenantID string) (int, error)
	// WorkspaceTemplateDelete removes the row of that id in the tenant;
	// found=false when none existed.
	WorkspaceTemplateDelete(ctx context.Context, tenantID, id string) (bool, error)
}
