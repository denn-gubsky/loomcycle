package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// hitListKeys names, per op, the result lists whose rows are hits a reader
// goes on from: search's chunks (expanded rows included) and related's
// neighbours by meaning and by relation.
var hitListKeys = map[string][]string{
	"search":  {"chunks"},
	"related": {"related", "linked"},
}

// attachHitNavigation adds to each hit of search and related what the next
// call needs (RFC GK D3, D4): `path`, the document's first Path name in sort
// order (the name list_statements groups it by), and `has_body: false` on a
// section whose own body is empty and whose text is in its children.
//
// It runs after the access-control filter, on the rows the caller is shown,
// and costs one name lookup and one child probe per page, not per hit.
//
// Decoration, like the annotations attached after it: a lookup fault leaves
// the answer as the op gave it, with a log line, rather than failing a search
// that found something.
func (d *Document) attachHitNavigation(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, acl *docACL, op string, res tools.Result) tools.Result {
	lists := hitListKeys[op]
	if lists == nil || res.IsError {
		return res
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(res.Text)))
	dec.UseNumber() // nanosecond validity times do not survive a float64
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return res
	}
	var rows []map[string]any
	for _, k := range lists {
		arr, _ := body[k].([]any)
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok {
				rows = append(rows, m)
			}
		}
	}
	if len(rows) == 0 {
		return res
	}
	var docIDs, chunkIDs []string
	seenDoc, seenChunk := map[string]bool{}, map[string]bool{}
	for _, m := range rows {
		if id, _ := m["document_id"].(string); id != "" && !seenDoc[id] {
			seenDoc[id] = true
			docIDs = append(docIDs, id)
		}
		if id, _ := m["chunk_id"].(string); id != "" && !seenChunk[id] {
			seenChunk[id] = true
			chunkIDs = append(chunkIDs, id)
		}
	}
	names, err := d.namesOfDocuments(ctx, key, docIDs)
	if err != nil {
		slog.WarnContext(ctx, "document: hit paths lookup failed", "op", op, "err", err)
		names = nil
	}
	bodiless, err := d.bodilessSections(ctx, key, mscope, chunkIDs)
	if err != nil {
		slog.WarnContext(ctx, "document: hit body probe failed", "op", op, "err", err)
		bodiless = nil
	}
	for _, m := range rows {
		doc, _ := m["document_id"].(string)
		if p := firstPathName(names[doc]); p != "" && acl.nameReadable(ctx, p) {
			m["path"] = p
		}
		if cid, _ := m["chunk_id"].(string); bodiless[cid] {
			m["has_body"] = false
		}
	}
	out, err := json.Marshal(body)
	if err != nil {
		return res
	}
	res.Text = string(out)
	return res
}

// nameReadable reports whether the caller may read the document named p. A
// hit reaches here only from a document the caller may read, and a document is
// readable only at every one of its names, so this cannot refuse today; it is
// asked anyway so a name is never shown on the strength of a gate upstream that
// a later change could loosen. Nil-safe: no set reads everything.
func (a *docACL) nameReadable(ctx context.Context, p string) bool {
	if a == nil || !a.pathSensitive {
		return true
	}
	return tools.ACLFilterAllows(ctx, a.eff, "read", a.resource(p))
}

// bodilessSections returns which of ids are sections: a chunk with children
// and an empty body of its own, so its text is in the children. One query
// finds the chunks with children; only those have their body read, and a page
// holds at most a few dozen hits.
func (d *Document) bodilessSections(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	res, err := d.query(ctx, key, `SELECT DISTINCT parent_id FROM chunks WHERE parent_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range res.Rows {
		id := asStr(r[0])
		cb, err := d.readBody(ctx, mscope, key, id)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(cb.Body) == "" {
			out[id] = true
		}
	}
	return out, nil
}

// chunkOutlineCap bounds the children get_chunk lists for a section with no
// body of its own: enough for any real outline level, small enough that a
// chunk with thousands of children (a fact store's subject) stays one page.
const chunkOutlineCap = 50

// chunkOutline is get_chunk's answer for a chunk whose own body is empty: its
// children, one level, in document order, so the next read is obvious (RFC GK
// D3). Nil when the chunk has none. The children are in the chunk's document,
// which the caller was allowed to read to get here.
func (d *Document) chunkOutline(ctx context.Context, key sqlmem.ScopeKey, chunkID string) ([]map[string]any, bool, error) {
	res, err := d.query(ctx, key, `SELECT id, title FROM chunks WHERE parent_id = ? ORDER BY position, id LIMIT ?`, chunkID, chunkOutlineCap+1)
	if err != nil {
		return nil, false, err
	}
	truncated := len(res.Rows) > chunkOutlineCap
	rows := res.Rows
	if truncated {
		rows = rows[:chunkOutlineCap]
	}
	var out []map[string]any
	for _, r := range rows {
		out = append(out, map[string]any{"id": asStr(r[0]), "title": asStr(r[1])})
	}
	return out, truncated, nil
}
