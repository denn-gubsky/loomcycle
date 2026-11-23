package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"slices"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The Document side of File op=checkout and op=commit (RFC GW P7): the
// export a checkout writes and a commit compares, and the listing a checkout
// takes its documents from. Both are asked through Document's own dispatch
// (executeInput), so the scope's grants, the access-control lists and a read
// mode hold for them as for any export_md or query_documents; the requests
// are unexported, set only in process by the File tool.

// checkoutExport asks export_md for a checkout's form of a document: the
// round-trip Markdown of the parts in force only, its content fingerprint
// and the document's Path names.
type checkoutExport struct{}

// checkoutListRequest asks query_documents for the documents a checkout
// takes: those named under in.UnderPath, or ids. With paths it asks instead
// which of those paths name a document at all.
type checkoutListRequest struct {
	ids []string
	// paths, when set, are canonical paths to look up: the answer says which
	// name a document, readable or not (a new file of a commit must not take
	// another document's name, and the store would let it).
	paths []string
	// max bounds the documents listed: past it the listing is refused.
	max int
}

// checkoutMaxListed bounds what one checkout listing reads: an under_path
// over a subject-homed fact store names as many documents as the tenant has
// entities.
const checkoutMaxListed = 100_000

// inForceTree keeps the parts a reimport reconciles against (currentParts):
// the root, and each part whose invalid_at is unset or after now, a closed
// part's subtree left out with it. A checkout that wrote a closed part would
// have a commit bring it back as a new one.
func inForceTree(roots []chunkRow, byParent map[string][]chunkRow, now int64) ([]chunkRow, map[string][]chunkRow) {
	kept := map[string][]chunkRow{}
	var walk func(id string)
	walk = func(id string) {
		for _, c := range byParent[id] {
			if c.InvalidAt != nil && *c.InvalidAt <= now {
				continue
			}
			kept[id] = append(kept[id], c)
			walk(c.ID)
		}
	}
	for _, r := range roots {
		walk(r.ID)
	}
	return roots, kept
}

// checkoutFingerprint is a checkout's revision of a document: the sha256 of
// its export before the relations trailer. Relations written since (a
// linker's, a reviewer's) do not change it, since a reimport keeps an
// unchanged part's relations and carries a superseded part's.
func checkoutFingerprint(exportBeforeEdges string) string {
	sum := sha256.Sum256([]byte(exportBeforeEdges))
	return hex.EncodeToString(sum[:])
}

// documentPathNames is every Path name of a document in the scope, sorted.
func (d *Document) documentPathNames(ctx context.Context, key sqlmem.ScopeKey, docID string) ([]string, error) {
	rows, err := d.Store.DirentListByDocuments(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), []string{docID})
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, r := range rows {
		if r.Kind == "document" {
			out = append(out, path.Join(r.ParentPath, r.Name))
		}
	}
	slices.Sort(out)
	return out, nil
}

// checkoutListing answers a checkoutListRequest: {documents: [{document_id,
// title, paths}], missing: [ids]} for a listing, {named: [paths]} for a
// path lookup. A document the caller may not read is left out of a listing,
// and an id named but not readable is reported missing, as one that does
// not exist is.
func (d *Document) checkoutListing(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	req := in.checkoutList
	if req.paths != nil {
		return d.checkoutPathsNamed(ctx, key, req.paths)
	}
	ids := req.ids
	if in.UnderPath != "" {
		var err error
		if ids, err = d.documentsUnderPath(ctx, key, in.UnderPath); err != nil {
			return docFail("query_documents: ", err), nil
		}
	}
	ids = uniqueStrings(ids)
	if len(ids) > req.max {
		return errValidation(fmt.Sprintf("query_documents: %d documents are named there, more than a checkout lists (%d)", len(ids), req.max),
			"Check out a narrower under_path."), nil
	}
	acl := d.newDocACL(ctx, key)
	titles := map[string]string{}
	for start := 0; start < len(ids); start += 500 {
		batch := ids[start:min(start+500, len(ids))]
		ph, args := inPlaceholders(batch)
		res, err := d.query(ctx, key, `SELECT id, title FROM documents WHERE `+notMoving+` AND id IN (`+ph+`)`, args...)
		if err != nil {
			return errFrom("query_documents: "+err.Error(), err), nil
		}
		for _, r := range res.Rows {
			titles[asStr(r[0])] = asStr(r[1])
		}
	}
	docs := []map[string]any{}
	missing := []string{}
	for _, id := range ids {
		title, ok := titles[id]
		if ok && acl != nil {
			readable, err := acl.readable(ctx, id)
			if err != nil {
				return errFrom("query_documents: "+err.Error(), err), nil
			}
			ok = readable
		}
		if !ok {
			missing = append(missing, id)
			continue
		}
		paths, err := d.documentPathNames(ctx, key, id)
		if err != nil {
			return errFrom("query_documents: "+err.Error(), err), nil
		}
		docs = append(docs, map[string]any{"document_id": id, "title": title, "paths": paths})
	}
	return jsonResult(map[string]any{"documents": docs, "missing": missing})
}

// checkoutPathsNamed says which of paths name a document in the scope. It
// does not say which document, nor whether the caller may read it: only
// that the name is taken.
func (d *Document) checkoutPathsNamed(ctx context.Context, key sqlmem.ScopeKey, paths []string) (tools.Result, error) {
	named := []string{}
	for _, p := range paths {
		canonical, err := normalizePath(p)
		if err != nil {
			continue
		}
		parent, name, isRoot := splitPath(canonical)
		if isRoot {
			continue
		}
		row, err := d.Store.DirentGet(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), parent, name)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return errFrom("query_documents: "+err.Error(), err), nil
		}
		if row.Kind != "" {
			named = append(named, canonical)
		}
	}
	return jsonResult(map[string]any{"named": named})
}

// uniqueStrings is ss without repeats, in first-seen order.
func uniqueStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// checkoutDocCall is the input of a Document call File makes for a checkout
// or a commit, with the raw form the hooks and the dispatch see.
func checkoutDocCall(in docInput) (docInput, json.RawMessage) {
	call := map[string]any{"op": in.Op, "scope": in.Scope}
	if in.DocumentID != "" {
		call["document_id"] = in.DocumentID
	}
	if in.UnderPath != "" {
		call["under_path"] = in.UnderPath
	}
	raw, _ := json.Marshal(call)
	return in, raw
}
