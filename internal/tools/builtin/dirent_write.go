package builtin

// dirent_write.go — the one place this package writes the dirent plane.
//
// WHY A CHOKEPOINT. The reserved tenant names (memory.IsReservedTenantPath) were first
// enforced op by op, in Path and in the Document ops that take a `path`. A Memory set
// with a path registers a name too — an upsert — and it was not one of those ops, so an
// agent could re-point /memory/ontology at a memory entry; the name then resolved to no
// document and, in a tenant with no recorded ontology yet, the real ontology was open
// to delete (RFC FY Part C, review of P2). Every create, delete and move of a dirent now
// passes through here, and TestDirentWrites_GoThroughTheReservedNameChokepoint holds the
// package to it. The per-op refusals stay: they answer with the op's own words, and they
// run before a write that this would refuse half-way through.
//
// Outside this package, the retention sweeper (agent scope) and erasure (user scope)
// delete dirents directly; neither reaches the tenant scope where the names are reserved.

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// reservedNameError is a dirent write the chokepoint refused.
type reservedNameError struct {
	name string
	// why replaces the reason when the name is reserved for something other
	// than a tenant-wide document (a folder's annotation).
	why string
}

func (e *reservedNameError) Error() string {
	if e.why != "" {
		return e.name + ": " + e.why
	}
	return e.name + " is a reserved name in the tenant scope: the runtime finds a tenant-wide " +
		"document by it, so only an operator may create, rename or remove it"
}

// reservedDirentErr is the chokepoint's question for a delete or move: may ctx remove
// or rename the given names (each canonical, with everything below it) in scope?
func reservedDirentErr(ctx context.Context, scope string, canonical ...string) error {
	if scope != "tenant" {
		return nil
	}
	for _, c := range canonical {
		for _, r := range reservedAtOrUnder(c) {
			if reservedNameRefusal(ctx, "", scope, r) != nil {
				return &reservedNameError{name: r}
			}
		}
	}
	return nil
}

// createDirent names a resource at row's path (an upsert). A create takes only its own
// name, so only that exact name is judged: creating /memory takes nothing the runtime
// finds a document by, where removing or moving /memory would.
func createDirent(ctx context.Context, st store.Store, row store.DirentRow) (store.DirentRow, error) {
	if row.Scope == "tenant" && reservedNameRefusal(ctx, "", row.Scope, row.ParentPath+row.Name) != nil {
		return store.DirentRow{}, &reservedNameError{name: row.ParentPath + row.Name}
	}
	if err := annotationNameErr(ctx, row.Scope, row.ParentPath+row.Name); err != nil {
		return store.DirentRow{}, err
	}
	return st.DirentCreate(ctx, row)
}

// deleteDirent removes one name.
func deleteDirent(ctx context.Context, st store.Store, tenantID, scope, scopeID, parent, name string) (bool, error) {
	if err := reservedDirentErr(ctx, scope, parent+name); err != nil {
		return false, err
	}
	return st.DirentDelete(ctx, tenantID, scope, scopeID, parent, name)
}

// deleteDirentsUnder removes every name below prefix (a directory prefix ending "/").
func deleteDirentsUnder(ctx context.Context, st store.Store, tenantID, scope, scopeID, prefix string) (int, error) {
	dir := strings.TrimSuffix(prefix, "/")
	if dir == "" {
		dir = "/"
	}
	if err := reservedDirentErr(ctx, scope, dir); err != nil {
		return 0, err
	}
	return st.DirentDeleteUnder(ctx, tenantID, scope, scopeID, prefix)
}

// moveDirent renames a name and everything below it.
func moveDirent(ctx context.Context, st store.Store, tenantID, scope, scopeID, fromParent, fromName, toParent, toName string) (bool, error) {
	if err := reservedDirentErr(ctx, scope, fromParent+fromName, toParent+toName); err != nil {
		return false, err
	}
	if err := annotationNameErr(ctx, scope, fromParent+fromName, toParent+toName); err != nil {
		return false, err
	}
	return st.DirentMove(ctx, tenantID, scope, scopeID, fromParent, fromName, toParent, toName)
}
