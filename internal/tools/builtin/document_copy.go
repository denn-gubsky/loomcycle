package builtin

// document_copy.go — RFC FT P0: copy_documents, a set of documents copied into
// another scope (or into the same scope under new names) on the server.
//
// WHY ROWS AND NOT export_md/import_md. An export carries the current bodies and
// the edges inside one document; an import mints ids. A copy that went through
// them would drop every relation between two copied documents, the body history,
// the image bytes and the derived units. Copying the rows themselves keeps all
// of it, and each table is read with SELECT * so a column a later migration adds
// is carried without this file knowing its name.
//
// WHAT A DOCUMENT OWNS is the list delete_document clears: its row, its chunks,
// their tags, revisions, layout and image assets, the document's tags and types,
// each chunk's body and derived units in the Memory plane, its Path names, and
// the statements touching its chunks. That list is the census this file copies;
// a table delete_document learns about belongs here too.
//
// THE ORDER leaves something recoverable at every step, because the SQL rows,
// the Memory bodies and the Path names are separate stores with no shared
// transaction: one document's rows go in one target transaction, then its
// bodies and units, then its names; the statements between copied documents go
// last, when every chunk they join exists. A document whose rows fail is
// reported and leaves nothing behind; one whose bodies or names fail is reported
// with what is missing.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	// copyMaxDocuments bounds one call's set (RFC FT §2).
	copyMaxDocuments = 500
	// copyPage is the rows asked for per query. The operator's SQL Memory row
	// cap may be lower; copyRows reads on from what a page actually returned.
	copyPage = 1000
	// copyBatch is the ids in one IN list.
	copyBatch = 200
	// copyMaxListed bounds each list in the answer (refused, dangling); the
	// counts beside them stay exact.
	copyMaxListed = 200
)

// copyRefusal codes, one per reason a document of the set is not copied.
const (
	copyNotFound      = "not_found"
	copyIsOntology    = "is_ontology"
	copyIsSubject     = "is_subject"
	copyHoldsFacts    = "holds_facts"
	copyForbidden     = "forbidden"
	copyPathCollision = "path_collision"
	copyKeyCollision  = "key_collision"
	copyWriteFailed   = "write_failed"
)

// copyDoc is one document of the set, as planned.
type copyDoc struct {
	id      string
	title   string
	names   []string // its names in the source scope
	targets []string // the names it gets in the target scope
	newID   string
	chunks  []string // its chunk ids, source side
}

// copyPlan is a call's set after every check.
type copyPlan struct {
	src, dst         sqlmem.ScopeKey
	srcMem, dstMem   store.MemoryScope
	docs             []*copyDoc // the members: what will be copied
	refused          []map[string]any
	dangling         []map[string]any
	danglingCount    int
	statements       []string          // statements between members, carried
	chunkDoc         map[string]string // source chunk id → member document id
	counts           map[string]int
	chunkMap         map[string]string // source chunk id → new chunk id (filled on write)
	unitsCopied      int
	bodyFailures     int
	nameFailures     []map[string]any
	statementsFailed int
}

func (p *copyPlan) refuse(docID, code, detail string) {
	r := map[string]any{"document_id": docID, "code": code}
	if detail != "" {
		r["detail"] = detail
	}
	p.refused = append(p.refused, r)
}

// copyDocuments is op=copy_documents.
func (d *Document) copyDocuments(ctx context.Context, src sqlmem.ScopeKey, srcMem store.MemoryScope, in docInput) (tools.Result, error) {
	const op = "copy_documents"
	if strings.TrimSpace(in.ToScope) == "" {
		return errValidation(op+": missing required field: to_scope", "Pass `to_scope`: agent, user or tenant — the scope the copies go to."), nil
	}
	// The target goes through the same gate as any call's scope: the tenant
	// grants, an isolated member's confinement. A scope the caller may not write
	// does not resolve.
	dst, dstMem, err := d.resolveScope(ctx, in.ToScope)
	if err != nil {
		return docFail(op+": to_scope: ", err), nil
	}
	if err := d.ensureSchema(ctx, dst); err != nil {
		return errFrom(op+": target schema init: "+err.Error(), err), nil
	}
	pathMap, perr := normalizePathMap(in.PathMap)
	if perr != nil {
		return errValidation(op+": path_map: "+perr.Error(), "Map absolute Path prefixes to absolute Path prefixes, e.g. {\"/loomcycle\": \"/kb/loomcycle\"}."), nil
	}
	if src == dst && len(pathMap) == 0 {
		return errValidation(op+": a copy into the same scope needs a path_map",
			"Every copy would take a name its original already holds. Pass path_map, e.g. {\"/docs\": \"/docs-copy\"}."), nil
	}
	ids, refused := d.copySet(ctx, src, op, in)
	if refused != nil {
		return *refused, nil
	}
	plan := &copyPlan{src: src, dst: dst, srcMem: srcMem, dstMem: dstMem, chunkDoc: map[string]string{},
		counts: map[string]int{}, chunkMap: map[string]string{}}
	if err := d.planCopy(ctx, plan, op, ids, pathMap); err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if err := d.planStatements(ctx, plan); err != nil {
		return errFrom(op+": statements: "+err.Error(), err), nil
	}
	dryRun := in.DryRun == nil || *in.DryRun
	if !dryRun {
		d.writeCopy(ctx, plan)
	}
	return jsonResult(copyResult(plan, dryRun))
}

// copySet resolves the call's set: document_ids, under_path and under_paths,
// their union, each document once, in the order named.
func (d *Document) copySet(ctx context.Context, src sqlmem.ScopeKey, op string, in docInput) ([]string, *tools.Result) {
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, id := range in.DocumentIDs {
		add(id)
	}
	under := append([]string{}, in.UnderPaths...)
	if in.UnderPath != "" {
		under = append([]string{in.UnderPath}, under...)
	}
	for _, p := range under {
		got, err := d.documentsUnderPath(ctx, src, p)
		if err != nil {
			r := docFail(op+": under_path "+p+": ", err)
			return nil, &r
		}
		sort.Strings(got)
		for _, id := range got {
			add(id)
		}
	}
	if len(ids) == 0 {
		r := errValidation(op+": no documents named: pass document_ids, under_path or under_paths (a path with no documents under it names none)",
			"List the scope's documents with op=query_documents or the Path tool's ls, then pass their ids or the subtree.")
		return nil, &r
	}
	if len(ids) > copyMaxDocuments {
		r := errValidation(fmt.Sprintf("%s: the set holds %d documents, at most %d per call", op, len(ids), copyMaxDocuments),
			"Split the set: pass a narrower under_path, or document_ids in batches.")
		return nil, &r
	}
	return ids, nil
}

// planCopy checks every document of the set and decides its target names. A
// document that cannot be copied is refused by name and the rest go ahead.
func (d *Document) planCopy(ctx context.Context, p *copyPlan, op string, ids []string, pathMap [][2]string) error {
	srcACL := d.newDocACL(ctx, p.src)
	dstACL := d.newDocACL(ctx, p.dst)
	names, err := d.namesOfDocuments(ctx, p.src, ids)
	if err != nil {
		return fmt.Errorf("names: %w", err)
	}
	ontology := ""
	if p.src.Scope == "tenant" {
		ontology = d.ontologyDocumentID(ctx, p.src)
	}
	claimed := map[string]string{} // target name → the member taking it
	for _, id := range ids {
		res, err := d.query(ctx, p.src, `SELECT title FROM documents WHERE id = ? LIMIT 1`, id)
		if err != nil {
			return err
		}
		if len(res.Rows) == 0 {
			p.refuse(id, copyNotFound, "no such document in scope "+p.src.Scope)
			continue
		}
		doc := &copyDoc{id: id, title: asStr(res.Rows[0][0]), names: names[id]}
		if id == ontology {
			p.refuse(id, copyIsOntology, "the tenant ontology is edited in place, never copied or moved")
			continue
		}
		if _, isSubject, err := d.subjectRootOf(ctx, p.src, id, true); err != nil {
			return err
		} else if isSubject {
			p.refuse(id, copyIsSubject, "a subject document is not copied between scopes")
			continue
		}
		facts, err := d.query(ctx, p.src, `SELECT 1 FROM chunk_memory_meta m JOIN chunks c ON c.id = m.chunk_id WHERE c.document_id = ? LIMIT 1`, id)
		if err != nil {
			return err
		}
		if len(facts.Rows) > 0 {
			p.refuse(id, copyHoldsFacts, "a document holding facts is not copied between scopes")
			continue
		}
		if srcACL != nil {
			if r, err := srcACL.document(ctx, op, "read", id); err != nil {
				return err
			} else if r != nil {
				p.refuse(id, copyForbidden, "read on the source document is not allowed")
				continue
			}
		}
		sourceNames := doc.names
		if len(sourceNames) == 0 {
			sourceNames = []string{"/documents/" + docDefaultPathSegment(doc.title, id)}
		}
		ok := true
		for _, n := range sourceNames {
			t := mapPath(n, pathMap)
			canonical, nerr := normalizePath(t)
			if nerr != nil {
				p.refuse(id, copyPathCollision, "target name "+t+" is not a valid path: "+nerr.Error())
				ok = false
				break
			}
			if p.dst.Scope == "tenant" && canonical == memrank.OntologyPath {
				p.refuse(id, copyIsOntology, "the target name is the tenant ontology's")
				ok = false
				break
			}
			if other, taken := claimed[canonical]; taken {
				p.refuse(id, copyPathCollision, "target name "+canonical+" is also the target of document "+other)
				ok = false
				break
			}
			parent, name, _ := splitPath(canonical)
			if _, gerr := d.Store.DirentGet(ctx, direntTenant(ctx), p.dst.Scope, direntScopeID(p.dst), parent, name); gerr == nil {
				p.refuse(id, copyPathCollision, "target name "+canonical+" already names something in scope "+p.dst.Scope)
				ok = false
				break
			} else {
				var nf *store.ErrNotFound
				if !asNotFound(gerr, &nf) {
					return gerr
				}
			}
			if dstACL != nil {
				if r := dstACL.path(ctx, op, "write", canonical); r != nil {
					p.refuse(id, copyForbidden, "write on the target name "+canonical+" is not allowed")
					ok = false
					break
				}
			}
			doc.targets = append(doc.targets, canonical)
		}
		if !ok {
			continue
		}
		if code, detail, err := d.copyKeyCollision(ctx, p, id); err != nil {
			return err
		} else if code != "" {
			p.refuse(id, code, detail)
			continue
		}
		for _, t := range doc.targets {
			claimed[t] = id
		}
		if err := d.copyRows(ctx, p.src, `SELECT id FROM chunks WHERE document_id = ?`, "id", []any{id}, func(_ []string, r []any) error {
			cid := asStr(r[0])
			doc.chunks = append(doc.chunks, cid)
			p.chunkDoc[cid] = id
			return nil
		}); err != nil {
			return err
		}
		p.docs = append(p.docs, doc)
	}
	return d.countCopy(ctx, p)
}

// copyKeyCollision refuses a document whose work key or identity key the target
// already holds: both are unique in a scope, so the copy would fail its insert.
func (d *Document) copyKeyCollision(ctx context.Context, p *copyPlan, docID string) (string, string, error) {
	for _, col := range []string{"work_key", "identity_key"} {
		res, err := d.query(ctx, p.src, `SELECT DISTINCT `+col+` FROM chunks WHERE document_id = ? AND `+col+` IS NOT NULL AND `+col+` <> ''`, docID)
		if err != nil {
			if isNoSuchColumn(err) {
				continue
			}
			return "", "", err
		}
		for _, r := range res.Rows {
			k := asStr(r[0])
			hit, err := d.query(ctx, p.dst, `SELECT 1 FROM chunks WHERE `+col+` = ? LIMIT 1`, k)
			if err != nil {
				if isNoSuchColumn(err) {
					break
				}
				return "", "", err
			}
			if len(hit.Rows) > 0 {
				return copyKeyCollision, fmt.Sprintf("scope %s already holds %s %q", p.dst.Scope, col, k), nil
			}
		}
	}
	return "", "", nil
}

// isNoSuchColumn: a column an older scope never got (its migration runs on
// first use) holds no value to collide with.
func isNoSuchColumn(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no such column") || (strings.Contains(s, "column") && strings.Contains(s, "does not exist"))
}

// countCopy fills the counts a dry run reports.
func (d *Document) countCopy(ctx context.Context, p *copyPlan) error {
	p.counts["documents"] = len(p.docs)
	var chunks []string
	for _, doc := range p.docs {
		chunks = append(chunks, doc.chunks...)
	}
	p.counts["chunks"] = len(chunks)
	for _, t := range []struct{ name, table string }{{"revisions", "chunk_revisions"}, {"assets", "chunk_assets"}} {
		n := 0
		for _, batch := range batchesOf(chunks, copyBatch) {
			res, err := d.query(ctx, p.src, `SELECT count(*) FROM `+t.table+` WHERE chunk_id IN (`+placeholders(len(batch))+`)`, anySlice(batch)...)
			if err != nil {
				return err
			}
			if len(res.Rows) > 0 {
				n += asInt(res.Rows[0][0])
			}
		}
		p.counts[t.name] = n
	}
	return nil
}

// planStatements sorts every statement touching a member's chunks: carried when
// both ends and every evidence chunk are members, dangling otherwise.
func (d *Document) planStatements(ctx context.Context, p *copyPlan) error {
	var chunks []string
	for _, doc := range p.docs {
		chunks = append(chunks, doc.chunks...)
	}
	seen := map[string]bool{}
	type stmt struct{ id, kind, from, to string }
	var touching []stmt
	for _, batch := range batchesOf(chunks, copyBatch) {
		ph := placeholders(len(batch))
		args := append(anySlice(batch), anySlice(batch)...)
		if err := d.copyRows(ctx, p.src, `SELECT id, kind, from_id, to_id FROM chunk_edges WHERE from_id IN (`+ph+`) OR to_id IN (`+ph+`)`, "id", args, func(_ []string, r []any) error {
			s := stmt{asStr(r[0]), asStr(r[1]), asStr(r[2]), asStr(r[3])}
			if !seen[s.id] {
				seen[s.id] = true
				touching = append(touching, s)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	// Evidence quoting a chunk outside the set would point nowhere in the target.
	outsideEvidence := map[string]bool{}
	ids := make([]string, 0, len(touching))
	for _, s := range touching {
		ids = append(ids, s.id)
	}
	for _, batch := range batchesOf(ids, copyBatch) {
		if err := d.copyRows(ctx, p.src, `SELECT edge_id, chunk_id FROM chunk_edge_evidence WHERE chunk_id IS NOT NULL AND chunk_id <> '' AND edge_id IN (`+placeholders(len(batch))+`)`, "id", anySlice(batch), func(_ []string, r []any) error {
			if _, member := p.chunkDoc[asStr(r[1])]; !member {
				outsideEvidence[asStr(r[0])] = true
			}
			return nil
		}); err != nil {
			return err
		}
	}
	for _, s := range touching {
		_, fromIn := p.chunkDoc[s.from]
		_, toIn := p.chunkDoc[s.to]
		outside := ""
		switch {
		case !fromIn:
			outside = "from"
		case !toIn:
			outside = "to"
		case outsideEvidence[s.id]:
			outside = "evidence"
		}
		if outside == "" {
			p.statements = append(p.statements, s.id)
			continue
		}
		p.danglingCount++
		if len(p.dangling) < copyMaxListed {
			p.dangling = append(p.dangling, map[string]any{"statement_id": s.id, "kind": s.kind, "from": s.from, "to": s.to, "outside": outside})
		}
	}
	p.counts["statements"] = len(p.statements)
	return nil
}

// writeCopy copies every member, then the statements between them.
func (d *Document) writeCopy(ctx context.Context, p *copyPlan) {
	var done []*copyDoc
	for _, doc := range p.docs {
		if err := d.copyDocumentRows(ctx, p, doc); err != nil {
			slog.WarnContext(ctx, "copy_documents: rows of a document failed", "document_id", doc.id, "err", err)
			p.refuse(doc.id, copyWriteFailed, err.Error())
			for _, c := range doc.chunks {
				delete(p.chunkMap, c)
			}
			continue
		}
		d.copyDocumentBodies(ctx, p, doc)
		for _, t := range doc.targets {
			if _, err := d.registerDocDirent(ctx, p.dst, doc.newID, t); err != nil {
				p.nameFailures = append(p.nameFailures, map[string]any{"document_id": doc.newID, "path": t, "error": err.Error()})
			}
		}
		done = append(done, doc)
	}
	p.docs = done
	p.counts["documents"] = len(done)
	d.copyStatements(ctx, p)
	for _, doc := range done {
		d.publishIngested(ctx, p.dst, p.dstMem, doc.newID, "copied", nil)
	}
}

// copyDocumentRows writes one document's SQL rows into the target in one
// transaction, under new ids.
func (d *Document) copyDocumentRows(ctx context.Context, p *copyPlan, doc *copyDoc) error {
	doc.newID = newDocID()
	local := map[string]string{}
	for _, c := range doc.chunks {
		local[c] = newDocID()
	}
	docRemap := func(v any) any { return remapID(v, map[string]string{doc.id: doc.newID}) }
	chunkRemap := func(v any) any { return remapID(v, local) }
	inChunks := `chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)`
	tables := []copyTable{
		{"documents", `id = ?`, "id", map[string]func(any) any{"id": docRemap, "root_chunk_id": chunkRemap}},
		{"chunks", `document_id = ?`, "id", map[string]func(any) any{"id": chunkRemap, "document_id": docRemap, "parent_id": chunkRemap}},
		{"chunk_types", `document_id = ?`, "name", map[string]func(any) any{"document_id": docRemap}},
		{"document_tags", `document_id = ?`, "tag", map[string]func(any) any{"document_id": docRemap}},
		{"chunk_tags", inChunks, "chunk_id, tag", map[string]func(any) any{"chunk_id": chunkRemap}},
		{"chunk_revisions", inChunks, "chunk_id, revision", map[string]func(any) any{"chunk_id": chunkRemap}},
		{"chunk_layout", inChunks, "chunk_id", map[string]func(any) any{"chunk_id": chunkRemap}},
		{"chunk_assets", inChunks, "chunk_id", map[string]func(any) any{"chunk_id": chunkRemap, "bytes": func(v any) any { return asBytes(v) }}},
	}
	// Every read happens before the target transaction opens: a SQLite scope
	// has one connection, so a read of the target (or, for a same-scope copy,
	// of the source) inside the transaction would wait on the transaction itself.
	var inserts []copyInsert
	written := map[string]int{}
	for _, t := range tables {
		rows, err := d.readTableRows(ctx, p.src, p.dst, t, doc.id)
		if err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
		inserts = append(inserts, rows...)
		written[t.name] = len(rows)
	}
	if err := d.withSqlTxn(ctx, p.dst, func(txnID string) error {
		return execInserts(ctx, d, txnID, inserts)
	}); err != nil {
		return err
	}
	p.counts["written_revisions"] += written["chunk_revisions"]
	p.counts["written_assets"] += written["chunk_assets"]
	for k, v := range local {
		p.chunkMap[k] = v
	}
	return nil
}

// copyTable is one table of what a document owns: which rows are its, how to
// page them, and which columns hold ids to rewrite.
type copyTable struct {
	name    string
	where   string // one ? — the source document id
	orderBy string
	remap   map[string]func(any) any
}

// copyInsert is one row to write into the target, already re-pointed.
type copyInsert struct {
	stmt string
	vals []any
}

// execInserts writes rows inside the open target transaction.
func execInserts(ctx context.Context, d *Document, txnID string, rows []copyInsert) error {
	for _, r := range rows {
		if err := d.execTxn(ctx, txnID, r.stmt, r.vals...); err != nil {
			return err
		}
	}
	return nil
}

// readTableRows reads one table's rows for one document, page by page, and
// turns each into the insert the target gets. Columns come from the rows
// themselves; a source column the target lacks is refused rather than dropped,
// since dropping it would lose data silently.
func (d *Document) readTableRows(ctx context.Context, src, dst sqlmem.ScopeKey, t copyTable, arg string) ([]copyInsert, error) {
	dstCols, err := d.tableColumns(ctx, dst, t.name)
	if err != nil {
		return nil, err
	}
	var out []copyInsert
	err = d.copyRows(ctx, src, `SELECT * FROM `+t.name+` WHERE `+t.where, t.orderBy, []any{arg}, func(columns []string, row []any) error {
		cols := make([]string, 0, len(columns))
		vals := make([]any, 0, len(columns))
		for i, c := range columns {
			v := row[i]
			if !dstCols[c] {
				if v == nil {
					continue
				}
				return fmt.Errorf("the target scope's %s has no column %s", t.name, c)
			}
			if f := t.remap[c]; f != nil {
				v = f(v)
			}
			cols = append(cols, c)
			vals = append(vals, v)
		}
		out = append(out, copyInsert{
			stmt: `INSERT INTO ` + t.name + ` (` + strings.Join(cols, ", ") + `) VALUES (` + placeholders(len(cols)) + `)`,
			vals: vals,
		})
		return nil
	})
	return out, err
}

// copyRows runs a SELECT page by page in a stable order and hands each row to
// fn. It advances by the rows a page actually returned, so a row cap below
// copyPage (the operator's SQL Memory MaxRows) cuts a page short without
// skipping anything.
func (d *Document) copyRows(ctx context.Context, key sqlmem.ScopeKey, stmt, orderBy string, args []any, fn func(cols []string, row []any) error) error {
	for offset := 0; ; {
		res, err := d.query(ctx, key, fmt.Sprintf(`%s ORDER BY %s LIMIT %d OFFSET %d`, stmt, orderBy, copyPage, offset), args...)
		if err != nil {
			return err
		}
		for _, row := range res.Rows {
			if err := fn(res.Columns, row); err != nil {
				return err
			}
		}
		offset += len(res.Rows)
		if len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < copyPage) {
			return nil
		}
	}
}

// copyDocumentBodies writes each chunk's body and derived units into the target
// and indexes them there. The provenance a body carries (an imported document's
// origin and source) goes with it; the root also records where it was copied
// from.
func (d *Document) copyDocumentBodies(ctx context.Context, p *copyPlan, doc *copyDoc) {
	tenant := direntTenant(ctx)
	srcID, dstID := docMemScopeID(p.srcMem, p.src), docMemScopeID(p.dstMem, p.dst)
	root := ""
	if res, err := d.query(ctx, p.src, `SELECT root_chunk_id FROM documents WHERE id = ?`, doc.id); err == nil && len(res.Rows) > 0 {
		root = asStr(res.Rows[0][0])
	}
	types := map[string]string{}
	for _, batch := range batchesOf(doc.chunks, copyBatch) {
		_ = d.copyRows(ctx, p.src, `SELECT id, coalesce(type, '') FROM chunks WHERE id IN (`+placeholders(len(batch))+`)`, "id", anySlice(batch), func(_ []string, r []any) error {
			types[asStr(r[0])] = asStr(r[1])
			return nil
		})
	}
	for _, c := range doc.chunks {
		nc := p.chunkMap[c]
		key := chunkBodyKey(c)
		entry, err := d.Store.MemoryGet(ctx, tenant, p.srcMem, srcID, key)
		var cb chunkBody
		value := entry.Value
		switch {
		case err == nil:
			_ = json.Unmarshal(entry.Value, &cb)
		case asNotFound(err, new(*store.ErrNotFound)):
			value = nil // a chunk with no body row (a bare root) gets none
		default:
			p.bodyFailures++
			slog.WarnContext(ctx, "copy_documents: read body", "chunk_id", c, "err", err)
			continue
		}
		if c == root {
			cb.Fields = setJSONField(cb.Fields, "copied_from", map[string]any{"scope": p.src.Scope, "document_id": doc.id})
			value, _ = json.Marshal(cb)
		}
		if value != nil {
			newKey := chunkBodyKey(nc)
			prov, perr := d.Store.MemoryProvenanceGet(ctx, tenant, p.srcMem, srcID, key)
			if perr == nil && (prov.Origin != "" || prov.Class != "" || prov.SourceSessionID != "" || prov.SourceRunID != "") {
				err = d.Store.MemorySetProvenance(ctx, tenant, p.dstMem, dstID, newKey, value, 0, prov)
			} else {
				err = d.Store.MemorySet(ctx, tenant, p.dstMem, dstID, newKey, value, 0)
			}
			if err != nil {
				p.bodyFailures++
				slog.WarnContext(ctx, "copy_documents: write body", "chunk_id", nc, "err", err)
				continue
			}
			d.embedBody(ctx, tenant, p.dstMem, p.dst, newKey, nc, types[c], cb.Body)
		}
		for _, u := range d.unitsOf(ctx, tenant, p.srcMem, srcID, c) {
			var v memrank.UnitValue
			if json.Unmarshal(u.Value, &v) != nil {
				continue
			}
			v.ChunkID = nc
			uv, _ := json.Marshal(v)
			uk := memrank.UnitKeyPrefixFor(nc) + strings.TrimPrefix(u.Key, memrank.UnitKeyPrefixFor(c))
			if err := d.Store.MemorySet(ctx, tenant, p.dstMem, dstID, uk, uv, 0); err != nil {
				slog.WarnContext(ctx, "copy_documents: write unit", "key", uk, "err", err)
				continue
			}
			if d.Embedder != nil {
				if idx := d.unitIndexText(ctx, p.dst, nc, v.Text); idx != "" {
					d.embedText(ctx, tenant, p.dstMem, dstID, uk, idx)
				}
			}
			p.unitsCopied++
		}
	}
}

// copyStatements writes the statements between copied documents, with their
// evidence, under new ids, once every chunk they join exists in the target.
func (d *Document) copyStatements(ctx context.Context, p *copyPlan) {
	written := 0
	for _, id := range p.statements {
		edgeMap := map[string]string{id: newDocID()}
		edgeRemap := func(v any) any { return remapID(v, edgeMap) }
		chunkRemap := func(v any) any { return remapID(v, p.chunkMap) }
		// Both ends must have been written: a member whose rows failed took its
		// chunks out of chunkMap.
		res, err := d.query(ctx, p.src, `SELECT from_id, to_id FROM chunk_edges WHERE id = ?`, id)
		if err != nil || len(res.Rows) == 0 {
			p.statementsFailed++
			continue
		}
		if p.chunkMap[asStr(res.Rows[0][0])] == "" || p.chunkMap[asStr(res.Rows[0][1])] == "" {
			p.statementsFailed++
			continue
		}
		edge, err := d.readTableRows(ctx, p.src, p.dst, copyTable{"chunk_edges", `id = ?`, "id",
			map[string]func(any) any{"id": edgeRemap, "from_id": chunkRemap, "to_id": chunkRemap}}, id)
		if err == nil {
			var evidence []copyInsert
			evidence, err = d.readTableRows(ctx, p.src, p.dst, copyTable{"chunk_edge_evidence", `edge_id = ?`, "id",
				map[string]func(any) any{"id": func(any) any { return newDocID() }, "edge_id": edgeRemap, "chunk_id": chunkRemap}}, id)
			if err == nil {
				err = d.withSqlTxn(ctx, p.dst, func(txnID string) error {
					return execInserts(ctx, d, txnID, append(edge, evidence...))
				})
			}
		}
		if err != nil {
			slog.WarnContext(ctx, "copy_documents: statement", "statement_id", id, "err", err)
			p.statementsFailed++
			continue
		}
		written++
	}
	p.counts["statements"] = written
}

// copyResult is the answer, the same shape for a dry run and a real one.
func copyResult(p *copyPlan, dryRun bool) map[string]any {
	docs := make([]map[string]any, 0, len(p.docs))
	for _, doc := range p.docs {
		e := map[string]any{"id": doc.id, "paths": doc.targets}
		if doc.newID != "" {
			e["new_id"] = doc.newID
		}
		docs = append(docs, e)
	}
	counts := map[string]any{}
	for k, v := range p.counts {
		counts[k] = v
	}
	if !dryRun {
		counts["units"] = p.unitsCopied
		if n, ok := p.counts["written_revisions"]; ok {
			counts["revisions"] = n
		}
		if n, ok := p.counts["written_assets"]; ok {
			counts["assets"] = n
		}
		delete(counts, "written_revisions")
		delete(counts, "written_assets")
	}
	refused := p.refused
	if refused == nil {
		refused = []map[string]any{}
	}
	dangling := p.dangling
	if dangling == nil {
		dangling = []map[string]any{}
	}
	out := map[string]any{
		"scope": p.src.Scope, "to_scope": p.dst.Scope, "dry_run": dryRun,
		"documents": docs, "counts": counts, "refused": refused,
		"dangling": dangling, "dangling_count": p.danglingCount,
	}
	if len(p.refused) > copyMaxListed {
		out["refused"] = p.refused[:copyMaxListed]
		out["refused_count"] = len(p.refused)
	}
	if p.bodyFailures > 0 {
		out["body_failures"] = p.bodyFailures
	}
	if len(p.nameFailures) > 0 {
		out["name_failures"] = p.nameFailures
	}
	if p.statementsFailed > 0 {
		out["statements_failed"] = p.statementsFailed
	}
	return out
}

// normalizePathMap checks path_map and orders it longest prefix first, so the
// most specific mapping wins.
func normalizePathMap(m map[string]string) ([][2]string, error) {
	out := make([][2]string, 0, len(m))
	for from, to := range m {
		f, err := normalizePath(from)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", from, err)
		}
		t, err := normalizePath(to)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", to, err)
		}
		out = append(out, [2]string{f, t})
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i][0]) != len(out[j][0]) {
			return len(out[i][0]) > len(out[j][0])
		}
		return out[i][0] < out[j][0]
	})
	return out, nil
}

// mapPath rewrites a name by the longest path_map prefix it lies at or under.
func mapPath(name string, m [][2]string) string {
	for _, e := range m {
		from, to := e[0], e[1]
		if from == "/" {
			return strings.TrimSuffix(to, "/") + name
		}
		if name == from {
			return to
		}
		if strings.HasPrefix(name, from+"/") {
			return strings.TrimSuffix(to, "/") + strings.TrimPrefix(name, from)
		}
	}
	return name
}

// remapID rewrites an id through m; anything not in m (a NULL, an empty parent,
// an id outside the set) is returned as it was.
func remapID(v any, m map[string]string) any {
	s, ok := v.(string)
	if !ok {
		if b, isBytes := v.([]byte); isBytes {
			s, ok = string(b), true
		}
	}
	if !ok || s == "" {
		return v
	}
	if n, hit := m[s]; hit {
		return n
	}
	return v
}

// setJSONField sets one key of a JSON object (a chunk's fields), making the
// object when there is none.
func setJSONField(raw json.RawMessage, k string, v any) json.RawMessage {
	m := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
		if m == nil {
			m = map[string]any{}
		}
	}
	m[k] = v
	out, _ := json.Marshal(m)
	return out
}

func batchesOf(ids []string, n int) [][]string {
	var out [][]string
	for len(ids) > 0 {
		k := n
		if len(ids) < k {
			k = len(ids)
		}
		out = append(out, ids[:k])
		ids = ids[k:]
	}
	return out
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
