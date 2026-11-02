package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// graph_snapshot (RFC FQ P0) exports a scope's documents, chunk trees, relations,
// Path names and fact flags in one columnar answer, paged by WHOLE documents, so a
// viewer can draw the whole scope without a call per document. Columns are
// parallel arrays; an `*_idx` column indexes into another section of the same
// response and a `*_code` column into the `codes` tables, which keeps a page of
// tens of thousands of chunks to a few repeated integers rather than repeated
// strings.
//
// Access is the listing rule (RFC EZ §1.6): a document the caller may not read is
// absent. It is decided per candidate BEFORE the page is cut, as listEdges does,
// so neither the page's size nor its cursor tells the caller a hidden document
// exists; a relation into one is dropped rather than shown with a dangling end.

const (
	snapshotPageDefault       = 200
	snapshotPageMax           = 1000
	snapshotEdgeBudgetDefault = 50000
	snapshotEdgeBudgetMax     = 200000
	// snapshotEdgeBatch is the rows asked for per relation query. Below the SQL
	// Memory row cap's default (10000) so a batch is normally whole; a cut batch
	// is read on from its last row anyway.
	snapshotEdgeBatch = 2000
	// snapshotMaxIDs bounds document_ids and chunk_ids: each becomes a SQL IN list.
	snapshotMaxIDs = 5000
)

// snapshotSectionNames is every `include` value; projection is opt-in.
var snapshotSectionNames = []string{"documents", "chunks", "edges", "dirents", "facts", "projection"}

// snapshotSectionNeeds: a section whose columns index into another needs it.
var snapshotSectionNeeds = map[string]string{
	"chunks":     "documents", // chunks.document_idx
	"edges":      "chunks",    // edges.from_idx / to_idx
	"facts":      "chunks",    // the fact columns sit beside the chunk columns
	"projection": "chunks",    // projection.missing names chunk indexes
}

type snapshotRequest struct {
	include    map[string]bool
	page       int
	edgeBudget int
	after      string // the cursor's document id; "" = the first page
	docIDs     []string
	chunkIDs   []string
	types      []string
	statuses   []string
	tag        string
	tagPrefix  string
	asOf       *int64
}

type snapshotCursor struct {
	After string `json:"d"`
}

func encodeSnapshotCursor(docID string) string {
	b, _ := json.Marshal(snapshotCursor{After: docID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeSnapshotCursor(s string) (string, bool) {
	var c snapshotCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil || c.After == "" {
		return "", false
	}
	return c.After, true
}

// snapshotList trims, drops empties and dedups, keeping first-seen order.
func snapshotList(one string, many []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range append([]string{one}, many...) {
		if v = strings.TrimSpace(v); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func parseSnapshotRequest(in docInput) (snapshotRequest, *tools.Result) {
	const op = "graph_snapshot"
	fail := func(msg, fix string) (snapshotRequest, *tools.Result) {
		r := errValidation(op+": "+msg, fix)
		return snapshotRequest{}, &r
	}
	req := snapshotRequest{include: map[string]bool{}}
	if len(in.Include) == 0 {
		for _, s := range snapshotSectionNames {
			req.include[s] = s != "projection"
		}
	} else {
		known := map[string]bool{}
		for _, s := range snapshotSectionNames {
			known[s] = true
		}
		for _, s := range in.Include {
			if !known[s] {
				return fail(fmt.Sprintf("unknown include %q", s), "Pass include as any of: "+strings.Join(snapshotSectionNames, ", ")+".")
			}
			req.include[s] = true
		}
		for _, s := range snapshotSectionNames {
			if need := snapshotSectionNeeds[s]; req.include[s] && need != "" && !req.include[need] {
				return fail(fmt.Sprintf("include %q needs %q, whose rows its columns index", s, need), "Add "+need+" to include.")
			}
		}
	}
	req.page = in.PageDocuments
	if req.page <= 0 {
		req.page = snapshotPageDefault
	}
	req.page = min(req.page, snapshotPageMax)
	req.edgeBudget = in.EdgeBudget
	if req.edgeBudget <= 0 {
		req.edgeBudget = snapshotEdgeBudgetDefault
	}
	req.edgeBudget = min(req.edgeBudget, snapshotEdgeBudgetMax)
	if in.Cursor != "" {
		after, ok := decodeSnapshotCursor(in.Cursor)
		if !ok {
			return fail("cursor is not a cursor this op returned", "Pass page.next_cursor from the previous page, or omit cursor for the first page.")
		}
		req.after = after
	}
	req.docIDs = snapshotList("", in.DocumentIDs)
	req.chunkIDs = snapshotList("", in.ChunkIDs)
	if len(req.docIDs) > snapshotMaxIDs || len(req.chunkIDs) > snapshotMaxIDs {
		return fail(fmt.Sprintf("document_ids and chunk_ids take at most %d ids each", snapshotMaxIDs), "Split the ids over several calls, or select by under_path.")
	}
	req.types = snapshotList(in.Type, in.Types)
	req.statuses = snapshotList(in.Status, in.Statuses)
	req.tag = strings.TrimSpace(in.Tag)
	req.tagPrefix = strings.TrimSpace(in.TagPrefix)
	if in.AsOf != nil {
		if *in.AsOf <= 0 {
			return fail("as_of must be a positive instant in unix nanoseconds", "Pass as_of as unix nanoseconds, e.g. 1747180800000000000, or omit it.")
		}
		v := *in.AsOf
		req.asOf = &v
	}
	return req, nil
}

// snapshotCodes interns the strings a `*_code` column indexes; "" is -1.
type snapshotCodes struct {
	index map[string]int
	list  []string
}

func (c *snapshotCodes) code(s string) int {
	if s == "" {
		return -1
	}
	if c.index == nil {
		c.index = map[string]int{}
	}
	if i, ok := c.index[s]; ok {
		return i
	}
	c.index[s] = len(c.list)
	c.list = append(c.list, s)
	return len(c.list) - 1
}

func (c *snapshotCodes) values() []string {
	if c.list == nil {
		return []string{}
	}
	return c.list
}

type snapshotDoc struct {
	id, title, root, typ, status string
	createdAt, updatedAt         int64
}

type snapshotChunk struct {
	id, docID, parentID, typ, status, title string
	position                                int
	validAt, invalidAt                      *int64
	updatedAt                               int64
	isFact                                  bool
	factClass                               string
	factConfidence                          *float64
	factRetired                             bool
}

func (d *Document) graphSnapshot(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	const op = "graph_snapshot"
	req, refused := parseSnapshotRequest(in)
	if refused != nil {
		return *refused, nil
	}
	acl := d.newDocACL(ctx, key)
	// A page is a small part of a possibly large scope: ask names per document
	// through the reverse index, primed a batch at a time.
	acl.useIndex()
	prime := func(ids []string) error { return acl.primeNames(ctx, ids) }
	limited, err := acl.limitsReading(ctx)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	readable := map[string]bool{}
	readableDoc := func(docID string) (bool, error) {
		if !limited {
			return true, nil
		}
		ok, seen := readable[docID]
		if !seen {
			var rerr error
			if ok, rerr = acl.readable(ctx, docID); rerr != nil {
				return false, rerr
			}
			readable[docID] = ok
		}
		return ok, nil
	}

	where, args, none, res := d.snapshotCandidates(ctx, key, req, in.UnderPath)
	if res != nil {
		return *res, nil
	}
	var docs []snapshotDoc
	more := false
	if !none {
		docs, more, err = d.snapshotPage(ctx, key, req, where, args, limited, readableDoc, prime)
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
	}

	var codeTypes, codeStatuses, codeKinds, codeClasses, codeMethods snapshotCodes
	clampedTo := 0
	var chunks []snapshotChunk
	if req.include["chunks"] && len(docs) > 0 {
		var cut bool
		chunks, cut, err = d.snapshotChunks(ctx, key, docs, req.include["facts"])
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		if cut {
			// The row cap cut the chunk statement. Rows come ordered by document,
			// in the page's order, so every document before the last one seen is
			// whole: the page keeps those, and the next page starts at the cut one.
			last := chunks[len(chunks)-1].docID
			k := 0
			for k < len(docs) && docs[k].id != last {
				k++
			}
			if k == 0 {
				return errBusiness(fmt.Sprintf("%s: document %s has more chunks than one SQL Memory query may return", op, last),
					"Raise LOOMCYCLE_SQLMEM_MAX_ROWS, or read that document with export_md."), nil
			}
			docs, more, clampedTo = docs[:k], true, k
			kept := chunks[:0]
			for _, c := range chunks {
				if c.docID == last {
					break
				}
				kept = append(kept, c)
			}
			chunks = kept
		}
	}

	page := map[string]any{"documents": len(docs)}
	if more && len(docs) > 0 {
		page["next_cursor"] = encodeSnapshotCursor(docs[len(docs)-1].id)
	}
	if clampedTo > 0 {
		page["clamped_to"] = clampedTo
	}
	out := map[string]any{"page": page}

	docIdx := make(map[string]int, len(docs))
	for i, doc := range docs {
		docIdx[doc.id] = i
	}
	if req.include["documents"] {
		sec, err := d.snapshotDocuments(ctx, key, mscope, docs, &codeTypes, &codeStatuses)
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		out["documents"] = sec
	}
	var chunkIdx map[string]int
	if req.include["chunks"] {
		var sec map[string]any
		sec, chunkIdx = snapshotChunkSection(docs, chunks, docIdx, req.include["facts"], &codeTypes, &codeStatuses, &codeClasses)
		out["chunks"] = sec
	}
	if req.include["edges"] {
		sec, err := d.snapshotEdges(ctx, key, req, docs, chunkIdx, limited, readableDoc, prime, &codeKinds, &codeStatuses, &codeMethods)
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		out["edges"] = sec
	}
	// The Path skeleton is the same on every page, so it rides the first one only.
	if req.include["dirents"] && req.after == "" {
		sec, res, err := d.snapshotDirents(ctx, key, in.UnderPath, docIdx, req.include["documents"], limited, readableDoc)
		if res != nil {
			return *res, nil
		}
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		out["dirents"] = sec
	}
	if req.include["projection"] {
		// P0 ships the block's shape only: no layout is computed yet, so every
		// chunk on the page is missing one.
		missing := make([]int, len(chunks))
		for i := range missing {
			missing[i] = i
		}
		out["projection"] = map[string]any{"model": "", "basis_version": 0, "xyz": []float64{}, "missing": missing}
	}
	out["codes"] = map[string]any{
		"types": codeTypes.values(), "statuses": codeStatuses.values(), "kinds": codeKinds.values(),
		"fact_classes": codeClasses.values(), "methods": codeMethods.values(),
	}
	return jsonResult(out)
}

// snapshotCandidates renders the document filters as a WHERE over `documents`.
// none reports a selection that already resolved to nothing.
func (d *Document) snapshotCandidates(ctx context.Context, key sqlmem.ScopeKey, req snapshotRequest, underPath string) (where string, args []any, none bool, refused *tools.Result) {
	const op = "graph_snapshot"
	where = "1=1"
	ids := req.docIDs
	if len(req.chunkIDs) > 0 {
		// A chunk in a document the caller may not read resolves here like any
		// other, and the page drops that document: the id answers as a missing one.
		ph, pargs := inPlaceholders(req.chunkIDs)
		res, err := d.query(ctx, key, `SELECT DISTINCT document_id FROM chunks WHERE id IN (`+ph+`)`, pargs...)
		if err != nil {
			r := errFrom(op+": "+err.Error(), err)
			return "", nil, false, &r
		}
		resolved := append([]string{}, ids...)
		for _, r := range res.Rows {
			resolved = append(resolved, asStr(r[0]))
		}
		ids = snapshotList("", resolved)
	}
	if len(req.docIDs) > 0 || len(req.chunkIDs) > 0 {
		if len(ids) == 0 {
			return "", nil, true, nil
		}
		ph, pargs := inPlaceholders(ids)
		where += " AND id IN (" + ph + ")"
		args = append(args, pargs...)
	}
	if underPath != "" {
		under, err := d.documentsUnderPath(ctx, key, underPath)
		if err != nil {
			r := docFail(op+": ", err)
			return "", nil, false, &r
		}
		if len(under) == 0 {
			return "", nil, true, nil
		}
		ph, pargs := inPlaceholders(under)
		where += " AND id IN (" + ph + ")"
		args = append(args, pargs...)
	}
	if len(req.types) > 0 {
		var types []string
		seen := map[string]bool{}
		for _, t := range req.types {
			for _, x := range d.expandTypeFilter(ctx, t) {
				if !seen[x] {
					seen[x] = true
					types = append(types, x)
				}
			}
		}
		frag, targs := typeFilterSQL("type", types)
		where += " AND " + frag
		args = append(args, targs...)
	}
	if len(req.statuses) > 0 {
		frag, sargs := typeFilterSQL("status", req.statuses)
		where += " AND " + frag
		args = append(args, sargs...)
	}
	if req.tag != "" {
		where += " AND id IN (SELECT document_id FROM document_tags WHERE tag = ?)"
		args = append(args, req.tag)
	}
	if req.tagPrefix != "" {
		where += " AND id IN (SELECT document_id FROM document_tags WHERE tag = ? OR tag LIKE ?)"
		args = append(args, req.tagPrefix, req.tagPrefix+"/%")
	}
	return where, args, false, nil
}

// snapshotPage reads the page's documents in id order past the cursor, keeping
// only readable ones, until it holds one more than the page (which says another
// page exists) or the candidates run out. The cursor is the last KEPT id, so it
// never names a hidden document.
func (d *Document) snapshotPage(ctx context.Context, key sqlmem.ScopeKey, req snapshotRequest, where string, args []any, limited bool, readableDoc func(string) (bool, error), prime func([]string) error) ([]snapshotDoc, bool, error) {
	batch := req.page + 1
	if limited {
		// A floor, so a small page among many hidden documents is not a query per row.
		batch = max(req.page, 100) + 1
	}
	after := req.after
	var docs []snapshotDoc
	for {
		w, a := where, append([]any{}, args...)
		if after != "" {
			w += " AND id > ?"
			a = append(a, after)
		}
		a = append(a, batch)
		res, err := d.query(ctx, key, `SELECT id, title, root_chunk_id, type, status, created_at, updated_at FROM documents WHERE `+w+` ORDER BY id LIMIT ?`, a...)
		if err != nil {
			return nil, false, err
		}
		if limited {
			batchIDs := make([]string, len(res.Rows))
			for i, r := range res.Rows {
				batchIDs[i] = asStr(r[0])
			}
			if err := prime(batchIDs); err != nil {
				return nil, false, err
			}
		}
		for _, r := range res.Rows {
			id := asStr(r[0])
			ok, err := readableDoc(id)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				continue
			}
			doc := snapshotDoc{id: id, title: asStr(r[1]), root: asStr(r[2]), typ: asStr(r[3]), status: asStr(r[4])}
			doc.createdAt, _ = asInt64(r[5])
			doc.updatedAt, _ = asInt64(r[6])
			docs = append(docs, doc)
			if len(docs) > req.page {
				break
			}
		}
		// The store's row cap can cut a batch short; only an uncut short batch
		// is the end.
		if len(docs) > req.page || len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < batch) {
			break
		}
		after = asStr(res.Rows[len(res.Rows)-1][0])
	}
	if len(docs) > req.page {
		return docs[:req.page], true, nil
	}
	return docs, false, nil
}

// snapshotChunks reads every chunk of the page's documents in one statement,
// ordered by document in the page's order. cut reports the SQL Memory row cap
// stopped it, so the last document read may be partial.
func (d *Document) snapshotChunks(ctx context.Context, key sqlmem.ScopeKey, docs []snapshotDoc, facts bool) ([]snapshotChunk, bool, error) {
	ids := make([]string, len(docs))
	for i, doc := range docs {
		ids[i] = doc.id
	}
	ph, args := inPlaceholders(ids)
	cols := `c.id, c.document_id, c.parent_id, c.position, c.type, c.status, c.title, c.valid_at, c.invalid_at, c.updated_at`
	from := `chunks c`
	if facts {
		cols += `, m.chunk_id, m.class, m.confidence, m.expired_at`
		from += ` LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id`
	}
	res, err := d.query(ctx, key, `SELECT `+cols+` FROM `+from+` WHERE c.document_id IN (`+ph+`) ORDER BY c.document_id, c.position, c.id`, args...)
	if err != nil {
		return nil, false, err
	}
	out := make([]snapshotChunk, 0, len(res.Rows))
	for _, r := range res.Rows {
		c := snapshotChunk{
			id: asStr(r[0]), docID: asStr(r[1]), parentID: asStr(r[2]), position: asInt(r[3]),
			typ: asStr(r[4]), status: asStr(r[5]), title: asStr(r[6]),
			validAt: asInt64Ptr(r[7]), invalidAt: asInt64Ptr(r[8]),
		}
		c.updatedAt, _ = asInt64(r[9])
		if facts {
			c.isFact = asStr(r[10]) != ""
			c.factClass = asStr(r[11])
			c.factConfidence = asFloat64Ptr(r[12])
			c.factRetired = asInt64Ptr(r[13]) != nil
		}
		out = append(out, c)
	}
	return out, res.Truncated && len(out) > 0, nil
}

// snapshotTreeOrder returns a document's chunks in depth-first document order
// (siblings by position) with each one's parent as an index into that order, so
// a parent always comes before its children. rows are the document's chunks in
// (position, id) order. A chunk whose parent is not in the document is a root;
// so is the first chunk of a cycle no root reaches, which keeps every chunk
// listed exactly once whatever the tree's state.
func snapshotTreeOrder(root string, rows []snapshotChunk) (order []int, parent []int) {
	byID := make(map[string]int, len(rows))
	for i, c := range rows {
		byID[c.id] = i
	}
	children := map[int][]int{}
	var roots []int
	for i, c := range rows {
		p, ok := byID[c.parentID]
		if c.parentID == "" || !ok || p == i {
			roots = append(roots, i)
			continue
		}
		children[p] = append(children[p], i)
	}
	if r, ok := byID[root]; ok {
		for j, x := range roots {
			if x == r {
				roots = append(append([]int{r}, roots[:j]...), roots[j+1:]...)
				break
			}
		}
	}
	visited := make([]bool, len(rows))
	type frame struct{ row, parent int }
	walk := func(start int) {
		stack := []frame{{start, -1}}
		for len(stack) > 0 {
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if visited[f.row] {
				continue
			}
			visited[f.row] = true
			at := len(order)
			order = append(order, f.row)
			parent = append(parent, f.parent)
			kids := children[f.row]
			for j := len(kids) - 1; j >= 0; j-- {
				stack = append(stack, frame{kids[j], at})
			}
		}
	}
	for _, r := range roots {
		walk(r)
	}
	for i := range rows {
		if !visited[i] {
			walk(i)
		}
	}
	return order, parent
}

func (d *Document) snapshotDocuments(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, docs []snapshotDoc, types, statuses *snapshotCodes) (map[string]any, error) {
	pageIDs := make([]string, len(docs))
	for i, doc := range docs {
		pageIDs[i] = doc.id
	}
	// The page's names only, through the reverse index: a page costs its own
	// documents, not the scope's whole Path tree.
	names, err := d.namesOfDocuments(ctx, key, pageIDs)
	if err != nil {
		return nil, err
	}
	n := len(docs)
	ids, titles, roots := make([]string, n), make([]string, n), make([]string, n)
	typeCode, statusCode := make([]int, n), make([]int, n)
	created, updated := make([]int64, n), make([]int64, n)
	colorEnabled := make([]bool, n)
	colorScheme := make([]json.RawMessage, n)
	paths := make([][]string, n)
	for i, doc := range docs {
		ids[i], titles[i], roots[i] = doc.id, doc.title, doc.root
		typeCode[i], statusCode[i] = types.code(doc.typ), statuses.code(doc.status)
		created[i], updated[i] = doc.createdAt, doc.updatedAt
		p := append([]string{}, names[doc.id]...)
		sort.Slice(p, func(a, b int) bool {
			if len(p[a]) != len(p[b]) {
				return len(p[a]) < len(p[b])
			}
			return p[a] < p[b]
		})
		paths[i] = p
		if doc.root != "" {
			// Decoration: one unreadable root must not fail the page, as in
			// documents_summary.
			body, _ := d.readBody(ctx, mscope, key, doc.root)
			enabled, scheme := docColorMeta(body)
			colorEnabled[i] = enabled
			if len(scheme) > 0 {
				colorScheme[i] = scheme
			}
		}
	}
	return map[string]any{
		"ids": ids, "titles": titles, "root_chunk_ids": roots, "type_code": typeCode, "status_code": statusCode,
		"created_at": created, "updated_at": updated, "color_enabled": colorEnabled, "color_scheme": colorScheme,
		"paths": paths,
	}, nil
}

// snapshotChunkSection lays the page's chunks out document by document, each
// document's tree in snapshotTreeOrder, and returns each chunk id's index.
func snapshotChunkSection(docs []snapshotDoc, chunks []snapshotChunk, docIdx map[string]int, facts bool, types, statuses, classes *snapshotCodes) (map[string]any, map[string]int) {
	byDoc := map[string][]snapshotChunk{}
	for _, c := range chunks {
		byDoc[c.docID] = append(byDoc[c.docID], c)
	}
	n := len(chunks)
	ids, titles := make([]string, 0, n), make([]string, 0, n)
	docCol, parentCol, position := make([]int, 0, n), make([]int, 0, n), make([]int, 0, n)
	typeCode, statusCode := make([]int, 0, n), make([]int, 0, n)
	validAt, invalidAt := make([]*int64, 0, n), make([]*int64, 0, n)
	updated := make([]int64, 0, n)
	isFact, factClass := make([]bool, 0, n), make([]int, 0, n)
	factConfidence, factRetired := make([]*float64, 0, n), make([]bool, 0, n)
	index := make(map[string]int, n)
	for _, doc := range docs {
		rows := byDoc[doc.id]
		order, parent := snapshotTreeOrder(doc.root, rows)
		base := len(ids)
		for j, ri := range order {
			c := rows[ri]
			index[c.id] = len(ids)
			ids = append(ids, c.id)
			titles = append(titles, c.title)
			docCol = append(docCol, docIdx[doc.id])
			if parent[j] < 0 {
				parentCol = append(parentCol, -1)
			} else {
				parentCol = append(parentCol, base+parent[j])
			}
			position = append(position, c.position)
			typeCode = append(typeCode, types.code(c.typ))
			statusCode = append(statusCode, statuses.code(c.status))
			validAt = append(validAt, c.validAt)
			invalidAt = append(invalidAt, c.invalidAt)
			updated = append(updated, c.updatedAt)
			if facts {
				isFact = append(isFact, c.isFact)
				factClass = append(factClass, classes.code(c.factClass))
				factConfidence = append(factConfidence, c.factConfidence)
				factRetired = append(factRetired, c.factRetired)
			}
		}
	}
	sec := map[string]any{
		"ids": ids, "document_idx": docCol, "parent_idx": parentCol, "position": position,
		"type_code": typeCode, "status_code": statusCode, "titles": titles,
		"valid_at": validAt, "invalid_at": invalidAt, "updated_at": updated,
	}
	if facts {
		sec["is_fact"] = isFact
		sec["fact_class_code"] = factClass
		sec["fact_confidence"] = factConfidence
		sec["fact_retired"] = factRetired
	}
	return sec, index
}

type snapshotExternal struct {
	Idx        int    `json:"idx"`
	ID         string `json:"id"`
	DocumentID string `json:"document_id"`
}

// snapshotEdges lists the live statements with an end on the page — each once —
// under the default visibility, in id order, up to the budget. An end off the
// page is an `external` entry (a negative index) carrying its document, and the
// statement is dropped when that document is unreadable or the chunk is gone.
func (d *Document) snapshotEdges(ctx context.Context, key sqlmem.ScopeKey, req snapshotRequest, docs []snapshotDoc, chunkIdx map[string]int, limited bool, readableDoc func(string) (bool, error), prime func([]string) error, kinds, statuses, methods *snapshotCodes) (map[string]any, error) {
	ids := []string{}
	fromCol, toCol, kindCode := []int{}, []int{}, []int{}
	auto := []bool{}
	created := []int64{}
	statusCode, methodCode := []int{}, []int{}
	confidence := []*float64{}
	validAt, invalidAt := []*int64{}, []*int64{}
	external := []snapshotExternal{}
	extIdx := map[string]int{}
	truncated := false

	if len(docs) > 0 {
		docIDs := make([]string, len(docs))
		for i, doc := range docs {
			docIDs[i] = doc.id
		}
		ph, phArgs := inPlaceholders(docIDs)
		base := "(e.from_id IN (SELECT id FROM chunks WHERE document_id IN (" + ph + ")) OR e.to_id IN (SELECT id FROM chunks WHERE document_id IN (" + ph + ")))"
		vis, visArgs := statementVisibility{asOf: req.asOf}.clause()
		after := ""
	read:
		for {
			w := base + " AND " + vis
			a := append(append(append([]any{}, phArgs...), phArgs...), visArgs...)
			if after != "" {
				w += " AND e.id > ?"
				a = append(a, after)
			}
			a = append(a, snapshotEdgeBatch)
			res, err := d.query(ctx, key, `SELECT e.id, e.from_id, e.to_id, e.kind, e.auto, e.status, e.method, e.confidence,
       e.valid_at, e.invalid_at, e.created_at, cf.document_id, ct.document_id
FROM chunk_edges e
LEFT JOIN chunks cf ON cf.id = e.from_id
LEFT JOIN chunks ct ON ct.id = e.to_id
WHERE `+w+`
ORDER BY e.id
LIMIT ?`, a...)
			if err != nil {
				return nil, err
			}
			if limited {
				// The off-page ends' documents, a batch at a time.
				var endDocs []string
				for _, r := range res.Rows {
					endDocs = append(endDocs, asStr(r[11]), asStr(r[12]))
				}
				if err := prime(endDocs); err != nil {
					return nil, err
				}
			}
			for _, r := range res.Rows {
				ends := [2]struct {
					chunk, doc string
				}{{asStr(r[1]), asStr(r[11])}, {asStr(r[2]), asStr(r[12])}}
				keep := true
				for _, end := range ends {
					if _, on := chunkIdx[end.chunk]; on {
						continue
					}
					if end.doc == "" {
						keep = false // the chunk is gone: nowhere to place this end
						break
					}
					ok, err := readableDoc(end.doc)
					if err != nil {
						return nil, err
					}
					if !ok {
						keep = false
						break
					}
				}
				if !keep {
					continue
				}
				if len(ids) == req.edgeBudget {
					truncated = true
					break read
				}
				var idx [2]int
				for j, end := range ends {
					if i, on := chunkIdx[end.chunk]; on {
						idx[j] = i
						continue
					}
					i, seen := extIdx[end.chunk]
					if !seen {
						i = -(len(external) + 1)
						extIdx[end.chunk] = i
						external = append(external, snapshotExternal{Idx: i, ID: end.chunk, DocumentID: end.doc})
					}
					idx[j] = i
				}
				status := asStr(r[5])
				if status == "" {
					status = edgeStatusAsserted
				}
				ids = append(ids, asStr(r[0]))
				fromCol = append(fromCol, idx[0])
				toCol = append(toCol, idx[1])
				kindCode = append(kindCode, kinds.code(asStr(r[3])))
				auto = append(auto, asInt(r[4]) == 1)
				statusCode = append(statusCode, statuses.code(status))
				methodCode = append(methodCode, methods.code(asStr(r[6])))
				confidence = append(confidence, asFloat64Ptr(r[7]))
				validAt = append(validAt, asInt64Ptr(r[8]))
				invalidAt = append(invalidAt, asInt64Ptr(r[9]))
				c, _ := asInt64(r[10])
				created = append(created, c)
			}
			// The store's row cap can cut a batch short; only an uncut short batch
			// is the end.
			if len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < snapshotEdgeBatch) {
				break
			}
			after = asStr(res.Rows[len(res.Rows)-1][0])
		}
	}
	return map[string]any{
		"ids": ids, "from_idx": fromCol, "to_idx": toCol, "kind_code": kindCode, "auto": auto,
		"created_at": created, "status_code": statusCode, "method_code": methodCode, "confidence": confidence,
		"valid_at": validAt, "invalid_at": invalidAt, "external": external, "truncated": truncated,
	}, nil
}

// snapshotDirents is the Path skeleton at/under underPath: directories and
// document names, by path. A caller with a reading limit sees only readable
// documents and the directories leading to them, so an empty or hidden subtree
// tells it nothing. document_idx points into this page's documents (-1 for a
// directory, or a document on another page); document_ids names the document
// whatever page it is on.
func (d *Document) snapshotDirents(ctx context.Context, key sqlmem.ScopeKey, underPath string, docIdx map[string]int, withDocuments, limited bool, readableDoc func(string) (bool, error)) (map[string]any, *tools.Result, error) {
	prefix := "/"
	if underPath != "" {
		canonical, err := normalizePath(underPath)
		if err != nil {
			r := errValidation("graph_snapshot: "+err.Error(), docPathFix)
			return nil, &r, nil
		}
		prefix = dirPrefix(canonical)
	}
	rows, err := d.Store.DirentListUnder(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), prefix)
	if err != nil {
		return nil, nil, err
	}
	type entry struct{ kind, docID string }
	entries := map[string]entry{}
	var dirs []string
	for _, r := range rows {
		full := r.ParentPath + r.Name
		switch r.Kind {
		case "document":
			var ref struct {
				DocumentID string `json:"document_id"`
			}
			_ = json.Unmarshal(r.ResourceRef, &ref)
			ok, err := readableDoc(ref.DocumentID)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				continue
			}
			entries[full] = entry{kind: "document", docID: ref.DocumentID}
			// The directories leading to it, implied or made, below the prefix.
			for p := r.ParentPath; len(p) > len(prefix) && strings.HasPrefix(p, prefix); {
				dir := strings.TrimSuffix(p, "/")
				if _, seen := entries[dir]; !seen {
					entries[dir] = entry{kind: "directory"}
				}
				p = dir[:strings.LastIndex(dir, "/")+1]
			}
		case "directory":
			dirs = append(dirs, full)
		}
	}
	// A made directory with nothing readable under it is shown only to a caller
	// whose set hides nothing: to anyone else it would describe a hidden subtree.
	if !limited {
		for _, dir := range dirs {
			if _, seen := entries[dir]; !seen {
				entries[dir] = entry{kind: "directory"}
			}
		}
	}
	paths := make([]string, 0, len(entries))
	for p := range entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	kinds, idx, docIDs := make([]string, len(paths)), make([]int, len(paths)), make([]string, len(paths))
	for i, p := range paths {
		e := entries[p]
		kinds[i], docIDs[i], idx[i] = e.kind, e.docID, -1
		if j, on := docIdx[e.docID]; on && withDocuments && e.kind == "document" {
			idx[i] = j
		}
	}
	return map[string]any{"paths": paths, "kinds": kinds, "document_idx": idx, "document_ids": docIDs}, nil, nil
}
