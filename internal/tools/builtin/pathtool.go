package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Path is the RFC AL Path-tree tool: a Unix-like VFS over the dirents
// substrate, addressing Documents / Volume mounts / Memory entries by
// human-readable, tenant-rooted, scope-aware paths. Resources keep their
// native ids; a dirent is just the name (Linux inode/dirent separation).
//
// Gated by allowed_tools:[Path]. Tenant + scope isolation is enforced at the
// store boundary — every op scopes by (tenant, scope, scope_id), and a crafted
// path can't escape its tree because normalizePath rejects "..". A dirent is a
// NAME, not an authority grant: resolving one returns a resource_ref, but
// using the resource still goes through that resource's own tool + gates.
type Path struct {
	Store store.Store
}

func (p *Path) Name() string { return "Path" }

func (p *Path) Description() string {
	return "A Unix-like filesystem over your Memory, Volumes, and Documents. Address resources by human-readable paths (e.g. /docs/launch). Ops: resolve, ls, stat, mkdir (no-op — dirs are implicit), mv, rm. Paths are scoped (agent/user/tenant, default agent) and tenant-isolated; segments are [a-zA-Z0-9._-], no \"..\"."
}

// pathInputSchema is a package const so the LoomCycle MCP server can source
// the wrapper's advertised inputSchema verbatim (via MCPWrapperInputSchema)
// rather than restating it — the same pattern as memoryInputSchema.
const pathInputSchema = `{
	"type": "object",
	"properties": {
		"op":            {"type": "string", "enum": ["resolve","ls","stat","mkdir","mv","rm"], "description": "The operation."},
		"path":          {"type": "string", "description": "Absolute path, e.g. /docs/launch. Segments are [a-zA-Z0-9._-]; no \"..\"."},
		"to":            {"type": "string", "description": "Destination path (mv only)."},
		"scope":         {"type": "string", "enum": ["agent","user","tenant"], "description": "Which tree (default agent). user requires a user_id on the run; tenant is shared across the tenant."},
		"recursive":     {"type": "boolean", "description": "ls: list all descendants. rm: required to remove a path that has descendants."},
		"kind_filter":   {"type": "string", "description": "ls: only entries of this kind (document/volume_mount/memory_entry/directory)."},
		"resource_too":  {"type": "boolean", "description": "rm: also delete the backing resource. NOT supported in v1 (dirent-only removal)."}
	},
	"required": ["op"]
}`

func (p *Path) InputSchema() json.RawMessage { return json.RawMessage(pathInputSchema) }

type pathInput struct {
	Op          string `json:"op"`
	Path        string `json:"path"`
	To          string `json:"to"`
	Scope       string `json:"scope"`
	Recursive   bool   `json:"recursive"`
	KindFilter  string `json:"kind_filter"`
	ResourceToo bool   `json:"resource_too"`
}

func (p *Path) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if p.Store == nil {
		return errResult("Path tool: not configured (no Store backend)"), nil
	}
	var in pathInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid input JSON: " + err.Error()), nil
	}
	tenantID, scope, scopeID, err := p.resolveScope(ctx, in.Scope)
	if err != nil {
		return errResult(err.Error()), nil
	}

	switch in.Op {
	case "resolve":
		return p.resolve(ctx, tenantID, scope, scopeID, in)
	case "ls":
		return p.ls(ctx, tenantID, scope, scopeID, in)
	case "stat":
		return p.stat(ctx, tenantID, scope, scopeID, in)
	case "mkdir":
		// Directories are implicit (S3-style) in v1 — mkdir is a no-op kept
		// for forward-compat + ergonomic parity with a real shell.
		if _, err := normalizePath(in.Path); err != nil {
			return errResult(err.Error()), nil
		}
		return jsonResult(map[string]any{"ok": true, "note": "directories are implicit in v1; mkdir is a no-op"})
	case "mv":
		return p.mv(ctx, tenantID, scope, scopeID, in)
	case "rm":
		return p.rm(ctx, tenantID, scope, scopeID, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: resolve, ls, stat, mkdir, mv, rm)", in.Op)), nil
	}
}

// resolveScope derives the authoritative (tenant, scope, scope_id) from the run
// identity — never the wire (mirrors Memory.resolveScope). scope defaults to
// agent. tenant_id is always the principal's tenant (the isolation axis).
func (p *Path) resolveScope(ctx context.Context, requested string) (tenantID, scope, scopeID string, err error) {
	tenantID = tools.RunIdentity(ctx).TenantID
	if requested == "" {
		requested = "agent"
	}
	switch requested {
	case "agent":
		name := tools.AgentName(ctx)
		if name == "" {
			return "", "", "", fmt.Errorf("Path: scope=agent requires a yaml-declared agent (no agent name on the run)")
		}
		return tenantID, "agent", name, nil
	case "user":
		uid := tools.RunIdentity(ctx).UserID
		if uid == "" {
			return "", "", "", fmt.Errorf("Path: scope=user requires a user_id on the run (caller must supply user_id)")
		}
		return tenantID, "user", uid, nil
	case "tenant":
		return tenantID, "tenant", "", nil
	default:
		return "", "", "", fmt.Errorf("Path: unknown scope %q (agent | user | tenant)", requested)
	}
}

type pathEntry struct {
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	FullPath    string          `json:"full_path"`
	ResourceRef json.RawMessage `json:"resource_ref,omitempty"`
}

func (p *Path) resolve(ctx context.Context, tenantID, scope, scopeID string, in pathInput) (tools.Result, error) {
	canonical, err := normalizePath(in.Path)
	if err != nil {
		return errResult(err.Error()), nil
	}
	parent, name, isRoot := splitPath(canonical)
	if isRoot {
		return jsonResult(map[string]any{"kind": "directory", "full_path": "/"})
	}
	row, err := p.Store.DirentGet(ctx, tenantID, scope, scopeID, parent, name)
	if err != nil {
		var nf *store.ErrNotFound
		if asNotFound(err, &nf) {
			return errResult("no such path: " + canonical), nil
		}
		return errResult("resolve: " + err.Error()), nil
	}
	return jsonResult(pathEntry{Name: name, Kind: row.Kind, FullPath: canonical, ResourceRef: row.ResourceRef})
}

func (p *Path) ls(ctx context.Context, tenantID, scope, scopeID string, in pathInput) (tools.Result, error) {
	canonical, err := normalizePath(in.Path)
	if err != nil {
		return errResult(err.Error()), nil
	}
	prefix := dirPrefix(canonical)
	var rows []store.DirentRow
	if in.Recursive {
		rows, err = p.Store.DirentListUnder(ctx, tenantID, scope, scopeID, prefix)
	} else {
		rows, err = p.Store.DirentList(ctx, tenantID, scope, scopeID, prefix)
	}
	if err != nil {
		return errResult("ls: " + err.Error()), nil
	}
	entries := make([]pathEntry, 0, len(rows))
	for _, r := range rows {
		if in.KindFilter != "" && r.Kind != in.KindFilter {
			continue
		}
		entries = append(entries, pathEntry{Name: r.Name, Kind: r.Kind, FullPath: r.ParentPath + r.Name, ResourceRef: r.ResourceRef})
	}
	return jsonResult(map[string]any{"path": canonical, "entries": entries})
}

func (p *Path) stat(ctx context.Context, tenantID, scope, scopeID string, in pathInput) (tools.Result, error) {
	canonical, err := normalizePath(in.Path)
	if err != nil {
		return errResult(err.Error()), nil
	}
	parent, name, isRoot := splitPath(canonical)
	if isRoot {
		return jsonResult(map[string]any{"kind": "directory", "full_path": "/", "scope": scope})
	}
	row, err := p.Store.DirentGet(ctx, tenantID, scope, scopeID, parent, name)
	if err != nil {
		var nf *store.ErrNotFound
		if asNotFound(err, &nf) {
			return errResult("no such path: " + canonical), nil
		}
		return errResult("stat: " + err.Error()), nil
	}
	return jsonResult(map[string]any{
		"full_path": canonical, "name": row.Name, "kind": row.Kind,
		"scope": scope, "resource_ref": row.ResourceRef,
		"created_at": row.CreatedAt, "updated_at": row.UpdatedAt,
	})
}

func (p *Path) mv(ctx context.Context, tenantID, scope, scopeID string, in pathInput) (tools.Result, error) {
	fromC, err := normalizePath(in.Path)
	if err != nil {
		return errResult("from: " + err.Error()), nil
	}
	toC, err := normalizePath(in.To)
	if err != nil {
		return errResult("to: " + err.Error()), nil
	}
	fromParent, fromName, fromRoot := splitPath(fromC)
	toParent, toName, toRoot := splitPath(toC)
	if fromRoot || toRoot {
		return errResult("cannot move the root path"), nil
	}
	if fromC == toC {
		return errResult("source and destination are the same path"), nil
	}
	// A directory can't be moved into itself or its own subtree — the
	// descendant-rewrite would reparent the moved node beneath itself and
	// orphan the whole subtree (real filesystems reject this, EINVAL). The
	// trailing-slash form avoids a /docs vs /docs2 false positive.
	if strings.HasPrefix(toC+"/", fromC+"/") {
		return errResult("cannot move a path into itself or its own subtree: " + fromC + " -> " + toC), nil
	}
	// No-clobber: the destination must not already exist.
	if _, err := p.Store.DirentGet(ctx, tenantID, scope, scopeID, toParent, toName); err == nil {
		return errResult("destination already exists: " + toC), nil
	}
	moved, err := p.Store.DirentMove(ctx, tenantID, scope, scopeID, fromParent, fromName, toParent, toName)
	if err != nil {
		return errResult("mv: " + err.Error()), nil
	}
	if !moved {
		return errResult("no such path: " + fromC), nil
	}
	return jsonResult(map[string]any{"ok": true, "from": fromC, "to": toC})
}

func (p *Path) rm(ctx context.Context, tenantID, scope, scopeID string, in pathInput) (tools.Result, error) {
	canonical, err := normalizePath(in.Path)
	if err != nil {
		return errResult(err.Error()), nil
	}
	parent, name, isRoot := splitPath(canonical)
	if isRoot {
		return errResult("cannot remove the root path"), nil
	}
	if in.ResourceToo {
		return errResult("resource_too is not supported in v1 — rm removes only the path entry; delete the backing resource via its own tool (Memory/Volume/Document)"), nil
	}
	// Refuse to remove a path with descendants unless recursive (Linux semantics).
	prefix := dirPrefix(canonical)
	descendants, err := p.Store.DirentListUnder(ctx, tenantID, scope, scopeID, prefix)
	if err != nil {
		return errResult("rm: " + err.Error()), nil
	}
	if len(descendants) > 0 && !in.Recursive {
		return errResult(fmt.Sprintf("path %q has %d descendant(s); pass recursive:true to remove them", canonical, len(descendants))), nil
	}
	removed := 0
	if in.Recursive && len(descendants) > 0 {
		n, derr := p.Store.DirentDeleteUnder(ctx, tenantID, scope, scopeID, prefix)
		if derr != nil {
			return errResult("rm: " + derr.Error()), nil
		}
		removed += n
	}
	found, err := p.Store.DirentDelete(ctx, tenantID, scope, scopeID, parent, name)
	if err != nil {
		return errResult("rm: " + err.Error()), nil
	}
	if found {
		removed++
	}
	if !found && removed == 0 {
		return errResult("no such path: " + canonical), nil
	}
	return jsonResult(map[string]any{"ok": true, "removed": canonical, "n_removed": removed})
}

// asNotFound reports whether err is a *store.ErrNotFound (the store's miss
// sentinel), tolerant of wrapping.
func asNotFound(err error, target **store.ErrNotFound) bool {
	return errors.As(err, target)
}

func jsonResult(v any) (tools.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return errResult("internal: marshal result: " + err.Error()), nil
	}
	return tools.Result{Text: string(b)}, nil
}
