package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// What a snapshot restore needs from the Path and Document tools. The snapshot
// package cannot import this one, so the restore call sites inject these, as
// they inject the other sections' authoring validators.

// direntKinds are the kinds the tools write: a Document names a document, the
// Memory tool a memory_entry, VolumeDef a volume_mount, and Path mkdir a
// directory. Resolution knows no other kind, so a row naming one is unreachable.
var direntKinds = map[string]bool{"document": true, "memory_entry": true, "volume_mount": true, "directory": true}

// ValidateDirentEntry re-checks one restored Path-tree entry against the rules
// the tools apply when they write one, so a hand-edited snapshot cannot bring
// back a name no author could have created here:
//   - scope is agent, user or tenant, keyed the way the Path tool keys it: an
//     empty scope_id for tenant (tenant_id carries the identity), a non-empty
//     one otherwise;
//   - parent_path is canonical and trailing-slashed, and the name is one
//     segment of [a-zA-Z0-9._-], so the full path passes normalizePath (no
//     "..", bounded length and depth);
//   - kind is one the tools write, and resource_ref is a JSON object carrying
//     what that kind resolves by.
//
// It checks shape only. Whether the thing named exists here is the restore's
// question, asked after every other section has landed.
func ValidateDirentEntry(body json.RawMessage) error {
	var e struct {
		Scope       string          `json:"scope"`
		ScopeID     string          `json:"scope_id"`
		ParentPath  string          `json:"parent_path"`
		Name        string          `json:"name"`
		Kind        string          `json:"kind"`
		ResourceRef json.RawMessage `json:"resource_ref"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return fmt.Errorf("entry does not decode as a path entry: %w", err)
	}
	switch e.Scope {
	case "tenant":
		if e.ScopeID != "" {
			return fmt.Errorf("a tenant-scope entry has an empty scope_id (the tenant carries the identity), got %q", e.ScopeID)
		}
	case "agent", "user":
		if e.ScopeID == "" {
			return fmt.Errorf("a %s-scope entry needs the %s it belongs to (scope_id is empty)", e.Scope, e.Scope)
		}
	default:
		return fmt.Errorf("unknown scope %q (agent | user | tenant)", e.Scope)
	}
	parent, err := normalizePath(e.ParentPath)
	if err != nil {
		return fmt.Errorf("parent_path: %w", err)
	}
	if dirPrefix(parent) != e.ParentPath {
		return fmt.Errorf("parent_path %q is not in canonical form (want %q)", e.ParentPath, dirPrefix(parent))
	}
	if e.Name == "" || e.Name == "." || e.Name == ".." || len(e.Name) > maxSegmentLen || !pathSegmentRe.MatchString(e.Name) {
		return fmt.Errorf("invalid name %q (one segment of letters, digits, . _ -; at most %d chars)", e.Name, maxSegmentLen)
	}
	if _, err := normalizePath(e.ParentPath + e.Name); err != nil {
		return err
	}
	if !direntKinds[e.Kind] {
		return fmt.Errorf("unknown kind %q (document | memory_entry | volume_mount | directory)", e.Kind)
	}
	var ref map[string]any
	if err := json.Unmarshal(e.ResourceRef, &ref); err != nil || ref == nil {
		return fmt.Errorf("resource_ref is not a JSON object")
	}
	need := map[string]string{"document": "document_id", "memory_entry": "key", "volume_mount": "volume_name"}[e.Kind]
	if need != "" {
		if v, _ := ref[need].(string); v == "" {
			return fmt.Errorf("a %s entry's resource_ref needs a non-empty %s", e.Kind, need)
		}
	}
	return nil
}

// SnapshotDocumentExists returns the check a restore uses to decide whether a
// restored `document` name points at a document on this host. Its arguments
// are the dirent's own coordinates; it maps them onto SQL Memory's, which key
// the same logical scope differently:
//   - the tenant: "" on the dirent plane, "default" in SQL Memory
//     (sqlScopeTenantValue);
//   - tenant scope: an empty scope_id on the dirent plane, the tenant again in
//     SQL Memory, whose scope id may not be empty (the inverse of direntScopeID;
//     Document.resolveScope builds the same key).
//
// A scope that was never provisioned holds no document, and is answered
// without provisioning it: a restore asks about every document name it
// carries, and must not leave an empty database behind for each one whose
// document did not arrive.
func SnapshotDocumentExists(sm *sqlmem.Manager) func(ctx context.Context, tenantID, scope, scopeID, documentID string) (bool, error) {
	return func(ctx context.Context, tenantID, scope, scopeID, documentID string) (bool, error) {
		key := sqlmem.ScopeKey{Tenant: sqlScopeTenantValue(tenantID), Scope: scope, ScopeID: scopeID}
		if scope == "tenant" {
			key.ScopeID = key.Tenant
		}
		ok, err := sm.ScopeExists(ctx, key)
		if err != nil || !ok {
			return false, err
		}
		res, err := sm.Query(ctx, key, sm.Rebind(`SELECT 1 FROM documents WHERE id = ? LIMIT 1`), []any{documentID})
		if schemaMissing(err) {
			return false, nil // a SQL Memory scope with no document tables
		}
		if err != nil {
			return false, err
		}
		return len(res.Rows) > 0, nil
	}
}
