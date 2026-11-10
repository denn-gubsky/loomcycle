package builtin

// document_annotation_summary.go — annotations attached to what the read ops
// return.
//
// with_summary: true on get_document, get_chunk, search, related, graph_recall,
// documents_summary and query_documents adds each row's annotation: the nearest
// record at or above the row's chunk (its own branch record, else the closest
// annotated ancestor's, else the document's). The abstract rides only on the rows
// a reader is most likely to need it for — the op's own target, the first five
// documents of a search page, the seeds of a walk — and within summary_chars, so
// the closest rows keep theirs and the rest keep a brief.
//
// It runs AFTER the access-control filter on the op's answer, so it only reads
// records of chunks the caller is already shown, and an ancestor of a shown
// chunk is in the same document. A stale record is shown marked stale; a missing
// one is absent, never made up from the body.
//
// Without with_summary nothing here changes the answer, with one exception the
// design asks for: documents_summary and graph_snapshot carry a document's brief
// as plain metadata whenever it has one, so a tree or a map can label documents
// without a second call. A scope with no annotations is byte-identical.

import (
	"bytes"
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	defaultSummaryChars = 2000
	maxSummaryChars     = 8000
	// summaryAbstractDocuments is how many documents of a search or related page
	// get their abstract.
	summaryAbstractDocuments = 5
	// maxSummaryStaleChecks bounds the records one answer recomputes the inputs
	// of; past it a summary carries no stale mark (unknown, not current).
	maxSummaryStaleChecks = 25
	// maxSummaryAncestorDepth bounds the walk up from a row to its annotation.
	maxSummaryAncestorDepth = 64
)

// summaryOps are the ops with_summary applies to.
var summaryOps = map[string]bool{
	"get_document": true, "get_chunk": true, "search": true, "related": true,
	"graph_recall": true, "documents_summary": true, "query_documents": true,
}

// validateSummaryInput refuses a with_summary the op does not take and a bad
// summary_chars, before the op runs.
func validateSummaryInput(in docInput) *tools.Result {
	if in.WithSummary && !summaryOps[in.Op] {
		r := errValidation(in.Op+": with_summary is taken by get_document, get_chunk, search, related, graph_recall, documents_summary and query_documents",
			"Omit with_summary on this op.")
		return &r
	}
	if in.SummaryChars < 0 {
		r := errValidation(in.Op+": summary_chars must be 0 or more", "")
		return &r
	}
	return nil
}

// summaryRow is one place in an answer a summary goes.
type summaryRow struct {
	m        map[string]any
	chunkID  string
	abstract bool
	field    string // the key the summary is written under
}

// attachAnnotations adds what the call asked for (with_summary), and the plain
// brief metadata of documents_summary and graph_snapshot.
func (d *Document) attachAnnotations(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, res tools.Result) tools.Result {
	if res.IsError {
		return res
	}
	plain := in.Op == "documents_summary" || in.Op == "graph_snapshot"
	if !plain && !(in.WithSummary && summaryOps[in.Op]) {
		return res
	}
	// One cheap probe first: a scope with no annotations is left exactly as it was.
	probe, err := d.query(ctx, key, `SELECT 1 FROM chunk_annotations LIMIT 1`)
	if err != nil || len(probe.Rows) == 0 {
		return res
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(res.Text)))
	dec.UseNumber() // nanosecond timestamps do not survive a float64
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return res
	}
	changed := false
	if in.Op == "graph_snapshot" {
		changed = d.snapshotBriefs(ctx, key, body)
	} else {
		if in.Op == "documents_summary" {
			changed = d.documentBriefs(ctx, key, body)
		}
		if in.WithSummary {
			changed = d.attachSummaries(ctx, key, mscope, in, body) || changed
		}
	}
	if !changed {
		return res
	}
	out, err := json.Marshal(body)
	if err != nil {
		return res
	}
	res.Text = string(out)
	return res
}

// documentBriefs sets brief on each documents_summary row whose document has a
// document-level annotation.
func (d *Document) documentBriefs(ctx context.Context, key sqlmem.ScopeKey, body map[string]any) bool {
	rows, _ := body["documents"].([]any)
	var roots []string
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok {
			if root, _ := m["root_chunk_id"].(string); root != "" {
				roots = append(roots, root)
			}
		}
	}
	if len(roots) == 0 {
		return false
	}
	recs, err := d.annotationRecordsFor(ctx, key, roots, annotationLevelDocument)
	if err != nil {
		return false
	}
	changed := false
	for _, r := range rows {
		m, _ := r.(map[string]any)
		root, _ := m["root_chunk_id"].(string)
		if rec, ok := recs[root][annotationLevelDocument]; ok {
			m["brief"] = rec.Brief
			changed = true
		}
	}
	return changed
}

// snapshotBriefs adds a briefs column to graph_snapshot's columnar documents,
// aligned with documents.ids (null where a document has none).
func (d *Document) snapshotBriefs(ctx context.Context, key sqlmem.ScopeKey, body map[string]any) bool {
	docs, _ := body["documents"].(map[string]any)
	ids, _ := docs["ids"].([]any)
	if len(ids) == 0 {
		return false
	}
	res, err := d.query(ctx, key, `SELECT d.id, a.brief FROM documents d
		JOIN chunk_annotations a ON a.chunk_id = d.root_chunk_id AND a.level = ?`, annotationLevelDocument)
	if err != nil || len(res.Rows) == 0 {
		return false
	}
	byDoc := make(map[string]string, len(res.Rows))
	for _, r := range res.Rows {
		byDoc[asStr(r[0])] = asStr(r[1])
	}
	briefs := make([]any, len(ids))
	found := false
	for i, id := range ids {
		if b, ok := byDoc[asStr(id)]; ok {
			briefs[i] = b
			found = true
		}
	}
	if !found {
		return false
	}
	docs["briefs"] = briefs
	return true
}

// summaryRows finds where in an op's answer the summaries go.
func summaryRows(op string, body map[string]any) []summaryRow {
	var rows []summaryRow
	list := func(field, idField string, abstract func(i int, m map[string]any) bool) {
		entries, _ := body[field].([]any)
		for i, e := range entries {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m[idField].(string)
			if id == "" {
				continue
			}
			rows = append(rows, summaryRow{m: m, chunkID: id, abstract: abstract(i, m), field: "summary"})
		}
	}
	never := func(int, map[string]any) bool { return false }
	firstDocs := func() func(int, map[string]any) bool {
		seen := map[string]bool{}
		return func(_ int, m map[string]any) bool {
			doc, _ := m["document_id"].(string)
			if seen[doc] {
				return true
			}
			if len(seen) >= summaryAbstractDocuments {
				return false
			}
			seen[doc] = true
			return true
		}
	}
	switch op {
	case "get_document":
		if root, _ := body["root_chunk_id"].(string); root != "" {
			rows = append(rows, summaryRow{m: body, chunkID: root, abstract: true, field: "summary"})
		}
	case "get_chunk":
		if id, _ := body["id"].(string); id != "" {
			rows = append(rows, summaryRow{m: body, chunkID: id, abstract: true, field: "summary"})
		}
	case "documents_summary", "query_documents":
		list("documents", "root_chunk_id", never)
	case "search":
		list("chunks", "chunk_id", firstDocs())
	case "related":
		list("related", "chunk_id", firstDocs())
	case "graph_recall":
		list("chunks", "id", func(_ int, m map[string]any) bool {
			return asInt(m["hop"]) == 0
		})
	}
	return rows
}

// attachSummaries writes each row's nearest annotation into it.
func (d *Document) attachSummaries(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, body map[string]any) bool {
	rows := summaryRows(in.Op, body)
	if len(rows) == 0 {
		return false
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.chunkID)
	}
	tree, err := d.ancestry(ctx, key, ids)
	if err != nil {
		return false
	}
	all := make([]string, 0, len(tree))
	for id := range tree {
		all = append(all, id)
	}
	recs, err := d.annotationRecordsFor(ctx, key, all, annotationLevelBranch, annotationLevelDocument)
	if err != nil || len(recs) == 0 {
		return false
	}
	budget := in.SummaryChars
	if budget == 0 {
		budget = defaultSummaryChars
	}
	budget = min(budget, maxSummaryChars)
	s := d.newAnnotationScope(key, mscope)
	staleMemo := map[string]*bool{}
	checks := 0
	staleOf := func(rec annotationRecord) *bool {
		k := rec.ChunkID + "/" + rec.Level
		if v, ok := staleMemo[k]; ok {
			return v
		}
		var v *bool
		if checks < maxSummaryStaleChecks {
			checks++
			if stale, err := s.stale(ctx, rec); err == nil {
				v = &stale
			}
		}
		staleMemo[k] = v
		return v
	}
	summaryOf := func(rec annotationRecord, rowChunk string, withAbstract bool) map[string]any {
		sm := map[string]any{"level": rec.Level, "brief": rec.Brief}
		if rec.ChunkID != rowChunk {
			sm["chunk_id"] = rec.ChunkID
		}
		if withAbstract {
			if n := utf8.RuneCountInString(rec.Abstract); n <= budget {
				sm["abstract"] = rec.Abstract
				budget -= n
			}
		}
		if st := staleOf(rec); st != nil && *st {
			sm["stale"] = true
		}
		return sm
	}
	changed := false
	for _, r := range rows {
		rec, ok := nearestAnnotation(tree, recs, r.chunkID)
		if !ok {
			continue
		}
		r.m[r.field] = summaryOf(rec, r.chunkID, r.abstract)
		changed = true
		// get_chunk on a section: the document's own annotation too, so a reader
		// placed deep in a document knows what the whole is.
		if in.Op == "get_chunk" && rec.Level != annotationLevelDocument {
			if root := rootOf(tree, r.chunkID); root != "" {
				if drec, ok := recs[root][annotationLevelDocument]; ok {
					ds := summaryOf(drec, root, true)
					delete(ds, "chunk_id")
					delete(ds, "level")
					r.m["document_summary"] = ds
				}
			}
		}
	}
	return changed
}

// ancestryNode is one chunk of the walk up: its parent ("" at a root).
type ancestryNode struct{ parent string }

// ancestry loads every chunk from ids up to their roots, one query per level.
func (d *Document) ancestry(ctx context.Context, key sqlmem.ScopeKey, ids []string) (map[string]ancestryNode, error) {
	tree := map[string]ancestryNode{}
	frontier := dedupeStrings(ids)
	for depth := 0; len(frontier) > 0 && depth < maxSummaryAncestorDepth; depth++ {
		var next []string
		for start := 0; start < len(frontier); start += 500 {
			end := min(start+500, len(frontier))
			ph, args := inPlaceholders(frontier[start:end])
			res, err := d.query(ctx, key, `SELECT id, parent_id FROM chunks WHERE id IN (`+ph+`)`, args...)
			if err != nil {
				return nil, err
			}
			for _, r := range res.Rows {
				id, parent := asStr(r[0]), asStr(r[1])
				tree[id] = ancestryNode{parent: parent}
				if parent != "" {
					if _, seen := tree[parent]; !seen {
						next = append(next, parent)
					}
				}
			}
		}
		frontier = dedupeStrings(next)
	}
	return tree, nil
}

// nearestAnnotation is the record a row's summary comes from: the chunk's own
// branch record, else the closest ancestor's, else the root's document record.
func nearestAnnotation(tree map[string]ancestryNode, recs map[string]map[string]annotationRecord, chunkID string) (annotationRecord, bool) {
	cur := chunkID
	for i := 0; cur != "" && i <= maxSummaryAncestorDepth; i++ {
		node, ok := tree[cur]
		if !ok {
			return annotationRecord{}, false
		}
		if node.parent == "" {
			rec, ok := recs[cur][annotationLevelDocument]
			return rec, ok
		}
		if rec, ok := recs[cur][annotationLevelBranch]; ok {
			return rec, true
		}
		cur = node.parent
	}
	return annotationRecord{}, false
}

func rootOf(tree map[string]ancestryNode, chunkID string) string {
	cur := chunkID
	for i := 0; i <= maxSummaryAncestorDepth; i++ {
		node, ok := tree[cur]
		if !ok {
			return ""
		}
		if node.parent == "" {
			return cur
		}
		cur = node.parent
	}
	return ""
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
