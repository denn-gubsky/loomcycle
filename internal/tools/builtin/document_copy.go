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
	copyReservedName  = "reserved_name"
	copyIsSubject     = "is_subject"
	copyHoldsFacts    = "holds_facts"
	copyForbidden     = "forbidden"
	copyPathCollision = "path_collision"
	copyKeyCollision  = "key_collision"
	copyWriteFailed   = "write_failed"
	// move_documents only.
	moveIDCollision = "id_collision"
)

// copyTargetForbidden is the detail of a refusal that must not say what in the
// target scope refused it.
const copyTargetForbidden = "write on the target is not allowed"

// copyDoc is one document of the set, as planned.
type copyDoc struct {
	id      string
	title   string
	names   []string // its names in the source scope
	targets []string // the names it gets in the target scope
	newID   string
	chunks  []string // its chunk ids, source side
	// resumed: a move's target already holds this document's rows, written by
	// an earlier call of the same move that did not finish.
	resumed bool
}

// copyPlan is a call's set after every check.
type copyPlan struct {
	// move: the plan is move_documents' — ids are kept, the target rows carry
	// the unfinished-move marker, and nothing is named or announced until the
	// source is gone (document_move.go).
	move             bool
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
	statementError   string // the first statement write's error, for the caller
	// failedDocs: a member whose body or statement write failed this call, and
	// why. A move keeps such a document's source (document_move.go).
	failedDocs map[string]string
	// unfinished: a move's members whose target is written but which are not
	// moved yet; the same call again finishes them.
	unfinished []map[string]any

	// hiddenDangling: relations leaving the set whose far end is in a document
	// the caller may not read. They are never listed or counted in the answer,
	// yet a move would delete them with the source, so they block a move.
	hiddenDangling int
	// unlinkable: listed relations leaving the set whose ends the caller may
	// not link; dangling:drop would unlink them, so they block a drop.
	unlinkable int
	// quietFinish: a move's unfinished moves (another call's) the caller may not
	// read in the target; finished, never reported (document_move.go).
	quietFinish map[string]bool
	// unreadable: ids the caller named but may not read (copySet); refused as
	// not_found, before anything else is asked about them.
	unreadable map[string]bool
}

func (p *copyPlan) unfinish(docID, code, detail string) {
	p.unfinished = append(p.unfinished, map[string]any{"document_id": docID, "code": code, "detail": detail})
}

// failDoc records why a member is short in the target, keeping the first reason.
func (p *copyPlan) failDoc(docID, why string) {
	if p.failedDocs != nil && p.failedDocs[docID] == "" {
		p.failedDocs[docID] = why
	}
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
	ids, unreadable, refused := d.copySet(ctx, src, op, in, false, copyMaxDocuments)
	if refused != nil {
		return *refused, nil
	}
	plan := &copyPlan{src: src, dst: dst, srcMem: srcMem, dstMem: dstMem, chunkDoc: map[string]string{},
		counts: map[string]int{}, chunkMap: map[string]string{}, unreadable: unreadable}
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
// their union, each document once, in the order named, at most limit of them.
// allowEmpty: a move with unfinished work to complete may name nothing more.
//
// A document the caller may not read is left out of the set here, before
// anything is asked about it, as a listing leaves it out: the plan would
// otherwise answer for it (not_found, is_subject, holds_facts, forbidden, its
// relations) and the set's size would count it, so a dry run over a subtree
// would enumerate what the caller cannot see (security re-audit 2026-11-04,
// D16). Found under a path, such a document is simply absent. Named by id, it
// stays in the set as unreadable, and the plan answers for it exactly as for
// an id that names nothing (not_found), so naming ids cannot tell the two
// apart.
func (d *Document) copySet(ctx context.Context, src sqlmem.ScopeKey, op string, in docInput, allowEmpty bool, limit int) ([]string, map[string]bool, *tools.Result) {
	var ids []string
	var unreadable map[string]bool
	seen := map[string]bool{}
	acl := d.newDocACL(ctx, src)
	var aclErr error
	add := func(id string, named bool) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] || aclErr != nil {
			return
		}
		seen[id] = true
		if readable, err := acl.permits(ctx, "read", id); err != nil || !readable {
			if aclErr = err; aclErr != nil || !named {
				return
			}
			if unreadable == nil {
				unreadable = map[string]bool{}
			}
			unreadable[id] = true
		}
		ids = append(ids, id)
	}
	for _, id := range in.DocumentIDs {
		add(id, true)
	}
	under := append([]string{}, in.UnderPaths...)
	if in.UnderPath != "" {
		under = append([]string{in.UnderPath}, under...)
	}
	for _, p := range under {
		got, err := d.documentsUnderPath(ctx, src, p)
		if err != nil {
			r := docFail(op+": under_path "+p+": ", err)
			return nil, nil, &r
		}
		sort.Strings(got)
		for _, id := range got {
			add(id, false)
		}
	}
	if aclErr != nil {
		r := errFrom(op+": access control: "+aclErr.Error(), aclErr)
		return nil, nil, &r
	}
	if len(ids) == 0 && !allowEmpty {
		r := errValidation(op+": no documents named: pass document_ids, under_path or under_paths (a path with no documents under it names none)",
			"List the scope's documents with op=query_documents or the Path tool's ls, then pass their ids or the subtree.")
		return nil, nil, &r
	}
	if len(ids) > limit {
		r := errValidation(fmt.Sprintf("%s: the set holds %d documents, at most %d per call", op, len(ids), limit),
			"Split the set: pass a narrower under_path, or document_ids in batches.")
		return nil, nil, &r
	}
	return ids, unreadable, nil
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
		// FAILS CLOSED: a plan that cannot tell which document is the ontology must
		// not proceed as if none were, or it would copy or move the ontology away.
		if ontology, err = d.resolveOntologyID(ctx, p.src); err != nil {
			return fmt.Errorf("could not determine which document is the tenant ontology: %w", err)
		}
	}
	claimed := map[string]string{} // target name → the member taking it
	for _, id := range ids {
		if p.unreadable[id] {
			// Named by the caller, who may not read it: answered as a missing id.
			p.refuse(id, copyNotFound, "no such document in scope "+p.src.Scope)
			continue
		}
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
		// A move removes the source's names, so moving the document at a reserved
		// tenant name away vacates that name (RFC FY Part C). A copy keeps them.
		if p.move && p.src.Scope == "tenant" {
			if held := firstReservedTenantPath(names[id]); held != "" && reservedNameRefusal(ctx, op, p.src.Scope, held) != nil {
				p.refuse(id, copyReservedName, "the document holds the reserved name "+held+"; only an operator may move it away")
				continue
			}
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
		// Read was asked when the set was resolved (copySet). A move removes the
		// source, so it needs write and delete there too: without delete, a move
		// would take away a document the caller may not delete.
		if srcACL != nil && p.move {
			if code, detail, err := moveSourceRefusal(ctx, srcACL, op, id); err != nil {
				return err
			} else if code != "" {
				p.refuse(id, code, detail)
				continue
			}
		}
		if p.move {
			resumed, collides, err := d.moveTargetHolds(ctx, p, id)
			if err != nil {
				return err
			}
			if collides {
				p.refuse(id, moveIDCollision, "scope "+p.dst.Scope+" already holds a document with this id")
				continue
			}
			doc.resumed = resumed
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
			// The other reserved names are the operator's to give (RFC FY Part C).
			if reservedNameRefusal(ctx, op, p.dst.Scope, canonical) != nil {
				p.refuse(id, copyReservedName, "the target name "+canonical+" is reserved in the tenant scope; only an operator may give it")
				ok = false
				break
			}
			if other, taken := claimed[canonical]; taken {
				p.refuse(id, copyPathCollision, "target name "+canonical+" is also the target of document "+other)
				ok = false
				break
			}
			// Asked before the target is probed for what holds the name, so a
			// name the caller may not write says nothing about the target scope.
			if dstACL != nil {
				if r := dstACL.path(ctx, op, "write", canonical); r != nil {
					p.refuse(id, copyForbidden, "write on the target name "+canonical+" is not allowed")
					ok = false
					break
				}
			}
			parent, name, _ := splitPath(canonical)
			if row, gerr := d.Store.DirentGet(ctx, direntTenant(ctx), p.dst.Scope, direntScopeID(p.dst), parent, name); gerr == nil {
				// A move that stopped after naming its target already holds the
				// name, for the same document: that is its own work.
				if p.move && doc.resumed && direntDocumentID(row) == id {
					doc.targets = append(doc.targets, canonical)
					continue
				}
				p.refuse(id, copyPathCollision, "target name "+canonical+" already names something in scope "+p.dst.Scope)
				ok = false
				break
			} else {
				var nf *store.ErrNotFound
				if !asNotFound(gerr, &nf) {
					return gerr
				}
			}
			doc.targets = append(doc.targets, canonical)
		}
		if !ok {
			continue
		}
		// A resumed move's own rows hold its keys already.
		if !doc.resumed {
			if code, detail, err := d.copyKeyCollision(ctx, p, dstACL, id); err != nil {
				return err
			} else if code != "" {
				p.refuse(id, code, detail)
				continue
			}
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

// moveSourceRefusal asks what removing a source document needs besides reading
// it: write, and delete. "" when both are allowed.
func moveSourceRefusal(ctx context.Context, acl *docACL, op, docID string) (code, detail string, err error) {
	for _, verb := range []string{"write", "delete"} {
		r, err := acl.document(ctx, op, verb, docID)
		if err != nil {
			return "", "", err
		}
		if r != nil {
			return copyForbidden, verb + " on the source document is not allowed", nil
		}
	}
	return "", "", nil
}

// copyKeyCollision refuses a document whose work key or identity key the target
// already holds: both are unique in a scope, so the copy would fail its insert.
//
// When the holder is a document the caller may not read (dstACL), the refusal is
// a plain write refusal that names neither the key nor the scope: otherwise a dry
// run would answer "is this work held there" for a document the caller cannot
// see, which resolve_anchor deliberately does not.
func (d *Document) copyKeyCollision(ctx context.Context, p *copyPlan, dstACL *docACL, docID string) (string, string, error) {
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
			hit, err := d.query(ctx, p.dst, `SELECT document_id FROM chunks WHERE `+col+` = ? LIMIT 1`, k)
			if err != nil {
				if isNoSuchColumn(err) {
					break
				}
				return "", "", err
			}
			if len(hit.Rows) > 0 {
				if readable, err := dstACL.permits(ctx, "read", asStr(hit.Rows[0][0])); err != nil {
					return "", "", err
				} else if !readable {
					return copyForbidden, copyTargetForbidden, nil
				}
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

// copyStmt is a statement touching a member's chunks, as planStatements reads it.
type copyStmt struct{ id, kind, from, to string }

// planStatements sorts every statement touching a member's chunks: carried when
// both ends and every evidence chunk are members, dangling otherwise.
func (d *Document) planStatements(ctx context.Context, p *copyPlan) error {
	var chunks []string
	for _, doc := range p.docs {
		chunks = append(chunks, doc.chunks...)
	}
	seen := map[string]bool{}
	var touching []copyStmt
	for _, batch := range batchesOf(chunks, copyBatch) {
		ph := placeholders(len(batch))
		args := append(anySlice(batch), anySlice(batch)...)
		if err := d.copyRows(ctx, p.src, `SELECT id, kind, from_id, to_id FROM chunk_edges WHERE from_id IN (`+ph+`) OR to_id IN (`+ph+`)`, "id", args, func(_ []string, r []any) error {
			s := copyStmt{asStr(r[0]), asStr(r[1]), asStr(r[2]), asStr(r[3])}
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
	acl := d.newDocACL(ctx, p.src)
	farDoc, err := d.farEndDocuments(ctx, p, acl, touching)
	if err != nil {
		return err
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
		// The documents of the relation's two ends: an end outside the set is in
		// a document of the scope that is not a member.
		ends := []string{p.chunkDoc[s.from], p.chunkDoc[s.to]}
		if far, isFar := farDoc[s.id]; isFar {
			if outside == "from" {
				ends[0] = far
			} else {
				ends[1] = far
			}
			// A far end the caller may not read is not reported: listing it would
			// name a chunk of a document it cannot see, and counting it would say
			// one is there. A move still has to stop for it (moveDocuments).
			if readable, err := acl.permits(ctx, "read", far); err != nil {
				return err
			} else if !readable {
				p.hiddenDangling++
				continue
			}
		}
		// dangling:drop deletes the relation with the source: an unlink, so
		// it needs link on both ends, as unlink_chunks asks.
		if p.move && acl != nil {
			for _, docID := range ends {
				if linkable, err := acl.permits(ctx, "link", docID); err != nil {
					return err
				} else if !linkable {
					p.unlinkable++
					break
				}
			}
		}
		p.danglingCount++
		if len(p.dangling) < copyMaxListed {
			p.dangling = append(p.dangling, map[string]any{"statement_id": s.id, "kind": s.kind, "from": s.from, "to": s.to, "outside": outside})
		}
	}
	p.counts["statements"] = len(p.statements)
	return nil
}

// farEndDocuments maps each statement leaving the set by an end (not by its
// evidence) to the document holding that outside end. Only asked for a caller
// with a set; a chunk with no row maps to "" (the whole scope is asked).
func (d *Document) farEndDocuments(ctx context.Context, p *copyPlan, acl *docACL, touching []copyStmt) (map[string]string, error) {
	if acl == nil {
		return nil, nil
	}
	farChunk := map[string]string{} // statement id → its outside chunk
	var chunks []string
	for _, s := range touching {
		_, fromIn := p.chunkDoc[s.from]
		_, toIn := p.chunkDoc[s.to]
		switch {
		case !fromIn:
			farChunk[s.id] = s.from
		case !toIn:
			farChunk[s.id] = s.to
		default:
			continue
		}
		chunks = append(chunks, farChunk[s.id])
	}
	docOf := map[string]string{}
	for _, batch := range batchesOf(chunks, copyBatch) {
		meta, err := d.chunkMetaByIDs(ctx, p.src, batch)
		if err != nil {
			return nil, err
		}
		for id, m := range meta {
			docOf[id] = m.documentID
		}
	}
	out := make(map[string]string, len(farChunk))
	for stmt, c := range farChunk {
		out[stmt] = docOf[c]
	}
	return out, nil
}

// writeCopy copies every member, then the statements between them.
func (d *Document) writeCopy(ctx context.Context, p *copyPlan) {
	var done []*copyDoc
	for _, doc := range p.docs {
		if err := d.copyDocumentRows(ctx, p, doc); err != nil {
			// The error goes back to the caller in the refusal's detail, not to
			// the log: it can quote row values.
			slog.WarnContext(ctx, "document copy: rows of a document failed", "document_id", doc.id, "move", p.move)
			p.refuse(doc.id, copyWriteFailed, err.Error())
			for _, c := range doc.chunks {
				delete(p.chunkMap, c)
			}
			continue
		}
		d.copyDocumentBodies(ctx, p, doc)
		// A move names its target only once the source is gone.
		if !p.move {
			for _, t := range doc.targets {
				if _, err := d.registerDocDirent(ctx, p.dst, doc.newID, t); err != nil {
					p.nameFailures = append(p.nameFailures, map[string]any{"document_id": doc.newID, "path": t, "error": err.Error()})
				}
			}
		}
		done = append(done, doc)
	}
	p.docs = done
	p.counts["documents"] = len(done)
	d.copyStatements(ctx, p)
	if !p.move {
		for _, doc := range done {
			d.publishIngested(ctx, p.dst, p.dstMem, doc.newID, "copied", nil)
		}
	}
}

// copyDocumentRows writes one document's SQL rows into the target in one
// transaction, under new ids.
func (d *Document) copyDocumentRows(ctx context.Context, p *copyPlan, doc *copyDoc) error {
	local := map[string]string{}
	if p.move {
		// A move keeps every id (RFC FT D1).
		doc.newID = doc.id
		for _, c := range doc.chunks {
			local[c] = c
		}
	} else {
		doc.newID = newDocID()
		for _, c := range doc.chunks {
			local[c] = newDocID()
		}
	}
	if doc.resumed {
		// The rows went in, with the marker, in one transaction of an earlier
		// call: they are complete.
		for k, v := range local {
			p.chunkMap[k] = v
		}
		return nil
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
		{"chunk_annotations", inChunks, "chunk_id, level", map[string]func(any) any{"chunk_id": chunkRemap}},
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
	if p.move {
		inserts = append(inserts, moveMarkerInsert(p, doc))
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
			p.failDoc(doc.id, "a body could not be read")
			slog.WarnContext(ctx, "copy_documents: read body", "chunk_id", c, "err", err)
			continue
		}
		if c == root && !p.move {
			cb.Fields = setJSONField(cb.Fields, "copied_from", map[string]any{"scope": p.src.Scope, "document_id": doc.id})
			value, _ = json.Marshal(cb)
		}
		if value != nil {
			newKey := chunkBodyKey(nc)
			prov, perr := d.Store.MemoryProvenanceGet(ctx, tenant, p.srcMem, srcID, key)
			if perr == nil && (prov.Origin != "" || prov.Class != "" || prov.SourceSessionID != "" || prov.SourceRunID != "") {
				err = d.Store.MemorySetProvenance(store.WithChangeDocument(ctx, doc.newID), tenant, p.dstMem, dstID, newKey, value, 0, prov)
			} else {
				err = d.Store.MemorySet(store.WithChangeDocument(ctx, doc.newID), tenant, p.dstMem, dstID, newKey, value, 0)
			}
			if err != nil {
				p.bodyFailures++
				p.failDoc(doc.id, "a body could not be written")
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
	// The copied annotations' search rows, written in the target like the units:
	// the records came over with the chunk tables.
	newIDs := make([]string, 0, len(doc.chunks))
	for _, c := range doc.chunks {
		newIDs = append(newIDs, p.chunkMap[c])
	}
	if recs, err := d.annotationRecordsFor(ctx, p.dst, newIDs); err == nil {
		for _, byLevel := range recs {
			for _, rec := range byLevel {
				d.indexAnnotation(ctx, p.dst, p.dstMem, rec)
			}
		}
	}
}

// copyStatements writes the statements between copied documents, with their
// evidence, under new ids, once every chunk they join exists in the target.
func (d *Document) copyStatements(ctx context.Context, p *copyPlan) {
	written := 0
	for _, id := range p.statements {
		newEdge, newEvidence := newDocID(), func(any) any { return newDocID() }
		if p.move {
			// Kept ids; a statement an earlier call of the same move wrote is done.
			newEdge, newEvidence = id, func(v any) any { return v }
			if res, err := d.query(ctx, p.dst, `SELECT 1 FROM chunk_edges WHERE id = ?`, id); err == nil && len(res.Rows) > 0 {
				written++
				continue
			}
		}
		edgeMap := map[string]string{id: newEdge}
		edgeRemap := func(v any) any { return remapID(v, edgeMap) }
		chunkRemap := func(v any) any { return remapID(v, p.chunkMap) }
		// Both ends must have been written: a member whose rows failed took its
		// chunks out of chunkMap.
		res, err := d.query(ctx, p.src, `SELECT from_id, to_id, method, kind, run_id FROM chunk_edges WHERE id = ?`, id)
		if err != nil || len(res.Rows) == 0 {
			p.statementsFailed++
			continue
		}
		from, to := asStr(res.Rows[0][0]), asStr(res.Rows[0][1])
		// A run without the link grant writing into the tenant scope carries a
		// relation as a proposal: what it asserted in a scope of its own is not
		// the tenant's until reviewed there (RFC FZ Part C). Who made a statement
		// is its method, not its kind: the runtime's own (about and derived_from
		// from a fact upsert, [[name]] links) is the documents' structure and
		// travels as it is — and so does a structural kind an old scope's
		// statements migration stamped manual with no run id. A row a run wrote
		// (method agent, or any run id) is demoted. A rejection stays rejected.
		method, kind, runID := asStr(res.Rows[0][2]), asStr(res.Rows[0][3]), asStr(res.Rows[0][4])
		structural := method == edgeMethodRuntime || method == edgeMethodWikilink ||
			(isReservedEdgeKind(kind) && method == edgeMethodManual && runID == "")
		demote := runFilesProposals(ctx, p.dstMem) && !structural
		failEnds := func() {
			p.failDoc(p.chunkDoc[from], "a relation could not be written")
			p.failDoc(p.chunkDoc[to], "a relation could not be written")
		}
		if p.chunkMap[from] == "" || p.chunkMap[to] == "" {
			p.statementsFailed++
			failEnds()
			continue
		}
		edge, err := d.readTableRows(ctx, p.src, p.dst, copyTable{"chunk_edges", `id = ?`, "id",
			map[string]func(any) any{"id": edgeRemap, "from_id": chunkRemap, "to_id": chunkRemap}}, id)
		if err == nil {
			var evidence []copyInsert
			evidence, err = d.readTableRows(ctx, p.src, p.dst, copyTable{"chunk_edge_evidence", `edge_id = ?`, "id",
				map[string]func(any) any{"id": newEvidence, "edge_id": edgeRemap, "chunk_id": chunkRemap}}, id)
			if err == nil {
				err = d.withSqlTxn(ctx, p.dst, func(txnID string) error {
					if err := execInserts(ctx, d, txnID, append(edge, evidence...)); err != nil || !demote {
						return err
					}
					// The verdict and the ruling that made it hold are not carried:
					// the proposal is judged afresh in the tenant.
					return d.execTxn(ctx, txnID, `UPDATE chunk_edges SET status = ?, operator_ruled_at = NULL, ruled_by = NULL,
						judged_at = NULL, judge_reason = NULL, judged_by = NULL, verdict_confidence = NULL
						WHERE id = ? AND status = ?`, edgeStatusProposed, newEdge, edgeStatusAsserted)
				})
			}
		}
		if err != nil {
			slog.WarnContext(ctx, "document copy: statement failed", "statement_id", id, "move", p.move)
			if p.statementError == "" {
				p.statementError = err.Error()
			}
			p.statementsFailed++
			failEnds()
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
		if doc.newID != "" && !p.move {
			e["new_id"] = doc.newID
		}
		if doc.resumed {
			e["resumed"] = true
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
	if p.statementError != "" {
		out["statement_error"] = p.statementError
	}
	if len(p.unfinished) > 0 {
		out["unfinished"] = p.unfinished
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
