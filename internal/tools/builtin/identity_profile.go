package builtin

import (
	"context"
	"encoding/json"
	"errors"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// readUserRootOf exports subject's User Profile in tenant as clean Markdown,
// "" when they have none (or there is no SQL Memory to keep one in).
//
// This is a scope-crossing read on purpose, and the narrowest one there is:
// it resolves one fixed path in one named user's scope of the caller's own
// tenant and returns the text to the Identity tool, which hands on the Public
// section and the Identity names and drops the rest. It is not the Document
// tool's Execute and does not ask the caller's access rules about the
// document — the Identity tool's grant is what authorizes reading what a
// person declared public — so it must never be reachable with a model-chosen
// path or scope.
//
// Read-only: a profile that does not exist is not provisioned (that happens
// when the person's own agent first references it), so a lookup leaves no
// trace in the other person's scope.
func readUserRootOf(ctx context.Context, st store.Store, sm *sqlmem.Manager, tenant, subject string) (string, error) {
	if st == nil || sm == nil || subject == "" {
		return "", nil
	}
	parent, name, _ := splitPath(memrank.UserRootPath)
	row, err := st.DirentGet(ctx, tenant, string(store.MemoryScopeUser), subject, parent, name)
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}
	if row.Kind != "document" {
		return "", nil
	}
	var ref struct {
		DocumentID string `json:"document_id"`
	}
	if err := json.Unmarshal(row.ResourceRef, &ref); err != nil || ref.DocumentID == "" {
		return "", nil
	}
	// The document's own scope: its tenant and owner, as that person's own
	// run would resolve it, and nothing else of the caller's.
	dctx := tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: tenant, UserID: subject})
	key := sqlmem.ScopeKey{Tenant: sqlmem.ScopeTenant(tenant), Scope: string(store.MemoryScopeUser), ScopeID: subject}
	d := &Document{Store: st, SqlMem: sm}
	if err := d.ensureSchema(dctx, key); err != nil {
		return "", err
	}
	clean := false
	res, err := d.exportMD(dctx, key, store.MemoryScopeUser, docInput{DocumentID: ref.DocumentID, IncludeMetadata: &clean})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", errors.New(res.Text)
	}
	var out struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		return "", err
	}
	return out.Markdown, nil
}
