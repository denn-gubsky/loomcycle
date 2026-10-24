package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC EZ §1.5 seam 5 — the Path tool's per-call INSTANCE check. A Path-tree
// entry is a NAME, and the name is asked about as the thing it names:
//
//   - a document, a directory (mkdir'd or implied) and a missing name are the
//     document namespace: `document:<scope>:<path>`, the verb as asked —
//     resolve/stat/ls read, mkdir/mv write, rm delete. A deny on
//     `document:tenant:/kb/secret/**` therefore also covers renaming a
//     document out of /kb/secret, moving one into it, and a directory above
//     it (every descendant a move or a recursive rm reaches is asked);
//   - a memory entry is `memory:<scope>` (the catalog names memory by scope,
//     not by key), and a volume mount is `volume:<name>`. Neither type has a
//     delete verb, so rm of such a name asks write.
type pathACL struct {
	eff     *authz.Effective
	scope   string
	scopeID string
}

func newPathACL(ctx context.Context, scope, scopeID string) *pathACL {
	eff := tools.CallerACL(ctx)
	if eff == nil {
		return nil
	}
	return &pathACL{eff: eff, scope: scope, scopeID: scopeID}
}

// resource names what a Path-tree entry is governed by, and the verb asked.
func (a *pathACL) resource(kind, fullPath string, ref json.RawMessage, verb string) (authz.Resource, string) {
	switch kind {
	case "memory_entry":
		var r struct {
			Scope   string `json:"scope"`
			ScopeID string `json:"scope_id"`
		}
		_ = json.Unmarshal(ref, &r)
		scope, scopeID := r.Scope, r.ScopeID
		if scope == "" {
			scope, scopeID = a.scope, a.scopeID
		}
		res := authz.Resource{Type: "memory", Name: scope}
		if scope == "user" {
			res.Owner = scopeID
		}
		if verb != "read" {
			verb = "write"
		}
		return res, verb
	case "volume_mount":
		var r struct {
			VolumeName string `json:"volume_name"`
		}
		_ = json.Unmarshal(ref, &r)
		if verb != "read" {
			verb = "write"
		}
		return authz.Resource{Type: "volume", Name: r.VolumeName}, verb
	}
	name := a.scope
	if fullPath != "" && fullPath != "/" {
		name += ":" + fullPath
	}
	res := authz.Resource{Type: "document", Name: name}
	if a.scope == "user" {
		res.Owner = a.scopeID
	}
	return res, verb
}

// allows asks one entry's question and returns the refusal, or nil.
func (a *pathACL) allows(ctx context.Context, op, kind, fullPath string, ref json.RawMessage, verb string) *tools.Result {
	if a == nil {
		return nil
	}
	res, v := a.resource(kind, fullPath, ref, verb)
	if tools.ACLAllowsInstance(ctx, a.eff, v, res) {
		return nil
	}
	r := errPermission(fmt.Sprintf("Path %s: the caller's access-control rules do not allow %s on %s:%s", op, v, res.Type, res.Name),
		"This name is outside what the caller may "+v+"; do not retry it.")
	return &r
}

// readable is a listing's question: no log line per hidden entry.
func (a *pathACL) readable(ctx context.Context, e pathEntry) bool {
	if a == nil {
		return true
	}
	res, v := a.resource(e.Kind, e.FullPath, e.ResourceRef, "read")
	return tools.ACLFilterAllows(ctx, a.eff, v, res)
}

// filter drops the entries the caller may not read: a listing is a search
// (RFC EZ §1.6), so what is not readable is absent.
func (a *pathACL) filter(ctx context.Context, entries []pathEntry) []pathEntry {
	if a == nil {
		return entries
	}
	kept := entries[:0]
	for _, e := range entries {
		if a.readable(ctx, e) {
			kept = append(kept, e)
		}
	}
	return kept
}

// aclSubtree asks verb about the entry at from and every entry under it — a
// move or a recursive remove reaches all of them — and, for a move (to set),
// about the name each one would get under to.
func (p *Path) aclSubtree(ctx context.Context, a *pathACL, op, verb, tenantID, scope, scopeID, from, to string) (*tools.Result, error) {
	kind, ref := "directory", json.RawMessage(nil) // implied by descendants, or missing
	parent, name, _ := splitPath(from)
	row, err := p.Store.DirentGet(ctx, tenantID, scope, scopeID, parent, name)
	var nf *store.ErrNotFound
	switch {
	case err == nil:
		kind, ref = row.Kind, row.ResourceRef
	case !asNotFound(err, &nf):
		return nil, err
	}
	ask := func(k, path string, rf json.RawMessage) *tools.Result {
		if r := a.allows(ctx, op, k, path, rf, verb); r != nil {
			return r
		}
		if to != "" {
			return a.allows(ctx, op, k, to+path[len(from):], rf, verb)
		}
		return nil
	}
	if r := ask(kind, from, ref); r != nil {
		return r, nil
	}
	kids, err := p.Store.DirentListUnder(ctx, tenantID, scope, scopeID, dirPrefix(from))
	if err != nil {
		return nil, err
	}
	for _, k := range kids {
		if r := ask(k.Kind, k.ParentPath+k.Name, k.ResourceRef); r != nil {
			return r, nil
		}
	}
	return nil, nil
}
