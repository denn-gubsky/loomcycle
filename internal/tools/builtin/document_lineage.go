package builtin

// document_lineage.go — Document op=lineage (RFC GL P2): from one chunk up to
// the root of the Path tree in one call — the enclosing sections, the
// document, and each folder above each of the document's Path names, each with
// its annotation brief. A walker reading a knowledge base otherwise spends three
// calls on that move (graph_recall up, get_document, Path ls).
//
// Access: the op is asked `read` on the chunk's document before it runs
// (docACLOps, target docACLChunk), so a chunk the caller may not read answers
// exactly as get_chunk does. Everything below that is either in the same
// document (sections) or a name the caller can already see:
//   - the document's Path names: read was asked at every one of them;
//   - a folder: listed only if Path ls would list it (read on
//     document:<scope>:<folder>, the question pathACL asks of a directory);
//   - a folder's brief: only when the caller may read every document the
//     folder's annotation covers (coversReadable, as get_annotation, ls
//     with_summary and annotationDocumentWithheld use). A withheld brief is
//     left out; the folder entry stays.
//
// The annotation reads reuse what with_summary uses (annotationRecordsFor,
// folderTargetOf/folderRecord, annotationScope.stale); nothing here reads an
// annotation another way. Sections and the document carry a stale mark (within
// the with_summary check budget); folders carry only their brief, see
// folderEntry.

import (
	"context"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	// maxLineageSections bounds the sections walked up from the chunk; a deeper
	// chunk answers with the nearest ones and sections_truncated. The document is
	// still reported: it is known from the chunk's row, not from the walk.
	maxLineageSections = 64
	// maxLineagePaths bounds the Path names reported, in sort order; past it
	// paths_truncated. Each name's folders are bounded by maxPathSegments, since
	// every stored name was validated against it.
	maxLineagePaths = 4
)

func (d *Document) lineage(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	const op = "lineage"
	if in.ID == "" {
		return errValidation(op+": missing required field: id", "Pass `id`, a chunk id from search, get_chunk or graph_recall; a document's root_chunk_id asks about the document itself."), nil
	}
	notFound := func() (tools.Result, error) {
		return errNotFound(op+": no such chunk: "+in.ID, "Find the chunk with op=search or op=query_chunks in the same scope."), nil
	}
	row, ok, err := d.getChunkRow(ctx, key, in.ID)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if !ok {
		return notFound()
	}
	docRes, err := d.query(ctx, key, `SELECT id, title, root_chunk_id, type, status FROM documents WHERE id = ? LIMIT 1`, row.DocumentID)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if len(docRes.Rows) == 0 {
		return notFound()
	}
	dr := docRes.Rows[0]
	docID, rootID := asStr(dr[0]), asStr(dr[2])

	// The sections, nearest first: the chunk itself (unless it is the root) and
	// each parent below the root. A broken tree — a parent that is missing, in
	// another document, or already visited — ends the walk there, reported as
	// sections_truncated (the chain is incomplete); another document's chunk is
	// never reported as a section of this one.
	var sections []chunkRow
	sectionsTruncated := false
	seen := map[string]bool{}
	for cur := row; cur.ID != rootID; {
		if len(sections) == maxLineageSections || seen[cur.ID] {
			sectionsTruncated = true
			break
		}
		seen[cur.ID] = true
		sections = append(sections, cur)
		if cur.ParentID == "" {
			break
		}
		next, ok, err := d.getChunkRow(ctx, key, cur.ParentID)
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		if !ok || next.DocumentID != row.DocumentID {
			sectionsTruncated = true
			break
		}
		cur = next
	}

	names, err := d.namesOfDocuments(ctx, key, []string{docID})
	if err != nil {
		return errFrom(op+": names: "+err.Error(), err), nil
	}
	docNames := names[docID]
	sort.Strings(docNames)
	pathsTruncated := len(docNames) > maxLineagePaths
	if pathsTruncated {
		docNames = docNames[:maxLineagePaths]
	}

	// Annotations: one probe first, so a scope with none costs no record reads.
	// Only a missing table means "none"; any other fault is reported, not taken
	// for an unannotated scope (which would silently drop every brief).
	probe, err := d.query(ctx, key, `SELECT 1 FROM chunk_annotations LIMIT 1`)
	if err != nil && !schemaMissing(err) {
		return errFrom(op+": annotations: "+err.Error(), err), nil
	}
	annotated := err == nil && len(probe.Rows) > 0
	var recs map[string]map[string]annotationRecord
	if annotated {
		ids := []string{rootID}
		for _, s := range sections {
			ids = append(ids, s.ID)
		}
		if recs, err = d.annotationRecordsFor(ctx, key, ids, annotationLevelBranch, annotationLevelDocument); err != nil {
			return errFrom(op+": annotations: "+err.Error(), err), nil
		}
	}
	s := d.newAnnotationScope(key, mscope)
	checks := 0
	mark := func(m map[string]any, rec annotationRecord) {
		m["brief"] = rec.Brief
		if checks >= maxSummaryStaleChecks {
			return // past the budget a record carries no mark: unknown, not current
		}
		checks++
		if stale, err := s.stale(ctx, rec); err == nil && stale {
			m["stale"] = true
		}
	}

	secOut := make([]map[string]any, 0, len(sections))
	for _, c := range sections {
		m := map[string]any{"id": c.ID, "title": c.Title}
		if c.Type != "" {
			m["type"] = c.Type
		}
		if rec, ok := recs[c.ID][annotationLevelBranch]; ok {
			mark(m, rec)
		}
		secOut = append(secOut, m)
	}
	docOut := map[string]any{"document_id": docID, "root_chunk_id": rootID, "title": asStr(dr[1])}
	if t := asStr(dr[3]); t != "" {
		docOut["type"] = t
	}
	if st := asStr(dr[4]); st != "" {
		docOut["status"] = st
	}
	if rec, ok := recs[rootID][annotationLevelDocument]; ok {
		mark(docOut, rec)
	}

	acl := d.newDocACL(ctx, key)
	folders := map[string]map[string]any{} // folder path → its entry; nil = not listed
	folderEntry := func(folder string) (map[string]any, error) {
		if m, done := folders[folder]; done {
			return m, nil
		}
		folders[folder] = nil
		// Listed only where Path ls would list it.
		if acl != nil && !tools.ACLFilterAllows(ctx, acl.eff, "read", acl.resource(folder)) {
			return nil, nil
		}
		m := map[string]any{"path": folder}
		folders[folder] = m
		if !annotated {
			return m, nil
		}
		t, err := d.folderTargetOf(ctx, key, folder)
		if err != nil {
			return nil, err
		}
		rec, err := d.folderRecord(ctx, key, t)
		if err != nil || rec == nil {
			return m, err
		}
		ok, err := coversReadable(ctx, acl, rec.covers)
		if err != nil {
			return nil, err
		}
		// The brief only, never a stale mark: judging a folder's record rebuilds
		// its inputs from its whole subtree (folderInputsFor), a scope-wide read
		// per walker step for a brief on "/", and the answer would also reflect
		// documents the caller may not read.
		if ok {
			m["brief"] = rec.Brief
		}
		return m, nil
	}
	pathsOut := make([]map[string]any, 0, len(docNames))
	for _, name := range docNames {
		chain := []map[string]any{}
		for p, hops := name, 0; p != "/" && hops < maxPathSegments; hops++ {
			parent, _, _ := splitPath(p)
			p = strings.TrimSuffix(parent, "/")
			if p == "" {
				p = "/"
			}
			m, err := folderEntry(p)
			if err != nil {
				return errFrom(op+": folder "+p+": "+err.Error(), err), nil
			}
			if m != nil {
				chain = append(chain, m)
			}
		}
		pathsOut = append(pathsOut, map[string]any{"path": name, "folders": chain})
	}

	out := map[string]any{"id": in.ID, "sections": secOut, "document": docOut, "paths": pathsOut}
	if sectionsTruncated {
		out["sections_truncated"] = true
	}
	if pathsTruncated {
		out["paths_truncated"] = true
	}
	return jsonResult(out)
}
