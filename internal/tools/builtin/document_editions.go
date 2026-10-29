package builtin

// document_editions.go — stable identity for prose, and new editions of a work
// (RFC FN P2).
//
// A law, a standard or a contract is amended, and the operator imports the new
// edition. Imported as a new document, every statement, tag and citation made
// against the old one stays on the old one; imported over it, every chunk is
// recreated and they are all lost. Neither is what an edition is: most parts are
// unchanged and should keep everything attached to them, a few changed and should
// be superseded with dates, a few were removed and should close, a few are new.
//
// That needs each part to be recognisable across editions, which is what an
// ANCHOR is: a key derived from where the part sits (its heading path, or a
// class-declared grammar such as AKN eIds), stored on the chunk. The WORK is
// recognised by its key too — the root chunk's work_key, or the class identity a
// document class's @identity fields produce (RFC FO). A keyed reimport matches the
// incoming edition to the held one part by part on those keys.
//
// WHY COLUMNS AND NOT natural_key. The RFC proposed storing `work#anchor` as the
// chunk's natural_key so sync's reconciliation would apply. natural_key lives in
// chunk_memory_meta, the table that makes a chunk a FACT, so every section of
// every keyed document would have become a fact in list_facts and the memory
// views (owner decision 2026-10-29).
//
// NOT ONE TRANSACTION. Parts are created through createChunk, which writes the
// body, its revision log, its embedding and its [[name]] links — none of which
// can join a SQL transaction. A reimport that fails half way is completed by
// running it again: every part already reconciled reads as unchanged.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// placeholderStatus marks a document filed for a work the scope does not hold
// yet (a citation's target). Its backlinks are the import worklist.
const placeholderStatus = "unresolved"

// migrateAnchors adds chunks.anchor, chunks.stable_id and chunks.work_key.
// Probe-then-ALTER, like every column migration here: ensureSchema runs on every
// op.
//
// WORK KEY IN ITS OWN COLUMN. It was first kept in identity_key, but that column
// is the ontology's: every write re-derives it from the chunk's class, so an
// ordinary edit of a typed (or untyped) root cleared or overwrote the claim and
// the next edition landed in a new document. Unique per scope, like identity_key.
func (d *Document) migrateAnchors(ctx context.Context, key sqlmem.ScopeKey) error {
	for _, col := range []string{"anchor", "stable_id"} {
		if !d.tableHasColumn(ctx, key, "chunks", col) {
			if err := d.exec(ctx, key, `ALTER TABLE chunks ADD COLUMN `+col+` TEXT`); err != nil {
				return err
			}
		}
	}
	if !d.tableHasColumn(ctx, key, "chunks", "work_key") {
		if err := d.exec(ctx, key, `ALTER TABLE chunks ADD COLUMN work_key TEXT`); err != nil {
			return err
		}
		// Carry the keys claimed before the column existed. Only where the claim is
		// recognisable: a placeholder root, or an untyped root — the ontology derives
		// an identity only from a class, so an untyped root's key was a work key.
		// A typed root's key may be its class identity; it stays where it is and
		// documentByWorkKey still finds it there.
		if err := d.exec(ctx, key, `UPDATE chunks SET work_key = identity_key
			WHERE identity_key IS NOT NULL AND id IN (SELECT root_chunk_id FROM documents)
			  AND (COALESCE(type, '') = '' OR status = ?)`, placeholderStatus); err != nil {
			return err
		}
	}
	if err := d.exec(ctx, key, `CREATE UNIQUE INDEX IF NOT EXISTS chunks_work_key ON chunks(work_key)`); err != nil {
		return err
	}
	if err := d.exec(ctx, key, `CREATE INDEX IF NOT EXISTS chunks_doc_anchor ON chunks(document_id, anchor)`); err != nil {
		return err
	}
	return d.exec(ctx, key, `CREATE INDEX IF NOT EXISTS chunks_stable_id ON chunks(stable_id)`)
}

// headingSegment normalises one heading for a heading-path anchor: lower-cased,
// runs of anything but letters and digits collapsed to "-". A heading with no
// letters (a bare "1.2") contributes nothing, so renumbering a list does not
// re-key everything under it.
func headingSegment(title string) string {
	var b strings.Builder
	dash, letters := false, false
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(r):
			letters = true
			fallthrough
		case unicode.IsDigit(r):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	if !letters {
		return ""
	}
	return b.String()
}

// anchorKeyer assigns anchors to parts in document order, keeping them unique
// within the document: a second "Notes" under the same parent is "notes~2".
type anchorKeyer struct {
	d       *Document
	ctx     context.Context
	grammar string
	taken   map[string]bool
}

func (k *anchorKeyer) next(path []string) string {
	a := k.d.anchorFor(k.ctx, k.grammar, path)
	if a == "" {
		segs := make([]string, 0, len(path))
		for _, p := range path {
			if s := headingSegment(p); s != "" {
				segs = append(segs, s)
			}
		}
		a = strings.Join(segs, "/")
	}
	if a == "" {
		a = "part"
	}
	base := a
	for n := 2; k.taken[a]; n++ {
		a = fmt.Sprintf("%s~%d", base, n)
	}
	k.taken[a] = true
	return a
}

// editionPart is a held part of a document.
type editionPart struct {
	id, parentID, title, anchor, stableID, typ, status string
	position                                           int
	body                                               string
}

// currentParts returns the document's parts in force at `at` (the root excluded),
// in tree order, with their bodies.
func (d *Document) currentParts(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, docID, rootID string, at int64) ([]editionPart, error) {
	res, err := d.query(ctx, key, `SELECT id, COALESCE(parent_id, ''), title, COALESCE(anchor, ''), COALESCE(stable_id, ''),
		COALESCE(type, ''), COALESCE(status, ''), position
		FROM chunks WHERE document_id = ? AND id <> ? AND (invalid_at IS NULL OR invalid_at > ?)
		ORDER BY position, created_at, id`, docID, rootID, at)
	if err != nil {
		return nil, err
	}
	byParent := map[string][]editionPart{}
	for _, r := range res.Rows {
		p := editionPart{id: asStr(r[0]), parentID: asStr(r[1]), title: asStr(r[2]), anchor: asStr(r[3]),
			stableID: asStr(r[4]), typ: asStr(r[5]), status: asStr(r[6]), position: asInt(r[7])}
		byParent[p.parentID] = append(byParent[p.parentID], p)
	}
	var out []editionPart
	var walk func(parent string)
	walk = func(parent string) {
		for _, p := range byParent[parent] {
			cb, err := d.readBody(ctx, mscope, key, p.id)
			if err == nil {
				p.body = cb.Body
			}
			out = append(out, p)
			walk(p.id)
		}
	}
	walk(rootID)
	return out, nil
}

// assignAnchors gives every part of a document in force now that has no anchor
// one, and a stable_id where it has none. A part keeps an anchor once it has one,
// so renaming a heading later does not silently re-key what was linked to it.
func (d *Document) assignAnchors(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, docID, rootID string) error {
	parts, err := d.currentParts(ctx, key, mscope, docID, rootID, time.Now().UnixNano())
	if err != nil {
		return err
	}
	keyer := &anchorKeyer{d: d, ctx: ctx, grammar: d.anchorGrammarFor(ctx, key, rootID), taken: map[string]bool{}}
	for _, p := range parts {
		if p.anchor != "" {
			keyer.taken[p.anchor] = true
		}
	}
	titles := map[string]string{}
	parents := map[string]string{}
	for _, p := range parts {
		titles[p.id], parents[p.id] = p.title, p.parentID
	}
	pathOf := func(id string) []string {
		var path []string
		for cur := id; cur != "" && cur != rootID; cur = parents[cur] {
			path = append([]string{titles[cur]}, path...)
		}
		return path
	}
	for _, p := range parts {
		if p.anchor != "" && p.stableID != "" {
			continue
		}
		a := p.anchor
		if a == "" {
			a = keyer.next(pathOf(p.id))
		}
		sid := p.stableID
		if sid == "" {
			sid = a
		}
		if err := d.exec(ctx, key, `UPDATE chunks SET anchor = ?, stable_id = ? WHERE id = ?`, a, sid, p.id); err != nil {
			return err
		}
	}
	return nil
}

// documentByWorkKey finds the document whose root holds work key wk: claimed as
// one (work_key), else a root whose class identity is that key (identity_key) —
// a typed work the ontology already names.
func (d *Document) documentByWorkKey(ctx context.Context, key sqlmem.ScopeKey, wk string) (docID, rootID, status string, err error) {
	for _, col := range []string{"work_key", "identity_key"} {
		res, err := d.query(ctx, key, `SELECT d.id, d.root_chunk_id, COALESCE(d.status, '') FROM documents d
			JOIN chunks c ON c.id = d.root_chunk_id WHERE c.`+col+` = ? LIMIT 1`, wk)
		if err != nil {
			return "", "", "", err
		}
		if len(res.Rows) > 0 {
			return asStr(res.Rows[0][0]), asStr(res.Rows[0][1]), asStr(res.Rows[0][2]), nil
		}
	}
	return "", "", "", nil
}

// claimWorkKey sets a root's work_key. A key another document's root holds, in
// either column, is the ontology's identity_collision: refused, never overwritten.
func (d *Document) claimWorkKey(ctx context.Context, key sqlmem.ScopeKey, rootID, wk string) *tools.Result {
	collision := func() *tools.Result {
		r := errBusiness("identity_collision: another document in this scope already holds the work key "+wk, "Reimport onto that document (work_key or its document_id) instead.")
		return &r
	}
	fault := func(err error) *tools.Result {
		r := errFrom("work key "+wk+": "+err.Error(), err)
		return &r
	}
	holder, _, _, err := d.documentByWorkKey(ctx, key, wk)
	if err != nil {
		return fault(err)
	}
	row, ok, err := d.getChunkRow(ctx, key, rootID)
	if err != nil {
		return fault(err)
	}
	if !ok {
		return fault(fmt.Errorf("no such root chunk %s", rootID))
	}
	if holder != "" && holder != row.DocumentID {
		return collision()
	}
	if holder == "" {
		if r := workKeyClaimAllowed(ctx, d.newDocACL(ctx, key)); r != nil {
			return r
		}
	}
	if err := d.exec(ctx, key, `UPDATE chunks SET work_key = ? WHERE id = ?`, wk, rootID); err != nil {
		// The unique index refuses the key only when a concurrent claim won it; any
		// other failure is a fault, and is reported as one.
		if h, _, _, herr := d.documentByWorkKey(ctx, key, wk); herr == nil && h != "" && h != row.DocumentID {
			return collision()
		}
		return fault(err)
	}
	return nil
}

// resolveAnchor finds a work by its key, and a part of it by anchor (or
// stable_id) in force at as_of, filing a placeholder for a work the scope does not
// hold when asked to. Every consumer and the Web UI resolve a citation this way.
func (d *Document) resolveAnchor(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	wk := strings.TrimSpace(in.WorkKey)
	if wk == "" {
		return errValidation("resolve_anchor: missing required field: work_key", "Pass the cited work's key, e.g. law:ua/2297-vi."), nil
	}
	if in.CreatePlaceholder {
		// The same lock a first-edition import takes: filing a placeholder while the
		// work is being imported would leave the import's copy keyless.
		unlock, err := d.editions.lock(ctx, key, "work", wk)
		if err != nil {
			return errFrom("resolve_anchor: "+err.Error(), err), nil
		}
		defer unlock()
	}
	acl := d.newDocACL(ctx, key)
	docID, rootID, status, err := d.documentByWorkKey(ctx, key, wk)
	if err != nil {
		return errFrom("resolve_anchor: "+err.Error(), err), nil
	}
	placeholder := status == placeholderStatus
	if docID == "" {
		if !in.CreatePlaceholder {
			return jsonResult(map[string]any{"work_key": wk, "found": false})
		}
		if r := workKeyClaimAllowed(ctx, acl); r != nil {
			return *r, nil
		}
		path := "/documents/unresolved/" + placeholderSlug(wk)
		if acl != nil {
			if r := acl.path(ctx, "resolve_anchor", "write", path); r != nil {
				return *r, nil
			}
		}
		cd, _ := d.createDocument(ctx, key, mscope, docInput{Title: wk, Path: path})
		if cd.IsError {
			return cd, nil
		}
		docID, rootID = resultField(cd, "document_id"), resultField(cd, "root_chunk_id")
		if r := d.claimWorkKey(ctx, key, rootID, wk); r != nil {
			// A concurrent resolver filed it first: drop ours through the normal
			// delete (it has a Path name and a body) and answer with theirs — under
			// the same read rule as any other holder.
			_, _ = d.deleteDocument(ctx, key, mscope, docInput{ID: docID})
			if docID, rootID, status, err = d.documentByWorkKey(ctx, key, wk); err != nil || docID == "" {
				return *r, nil
			}
			if acl != nil {
				if rr, err := acl.document(ctx, "resolve_anchor", "read", docID); err != nil || rr != nil {
					return workKeyUnreadable("resolve_anchor", wk), nil
				}
			}
		} else {
			if err := d.exec(ctx, key, `UPDATE documents SET status = ? WHERE id = ?`, placeholderStatus, docID); err != nil {
				return errFrom("resolve_anchor: "+err.Error(), err), nil
			}
			if err := d.exec(ctx, key, `UPDATE chunks SET status = ? WHERE id = ?`, placeholderStatus, rootID); err != nil {
				return errFrom("resolve_anchor: "+err.Error(), err), nil
			}
		}
		placeholder = true
	} else if acl != nil {
		if r, err := acl.document(ctx, "resolve_anchor", "read", docID); err != nil {
			return errFrom("resolve_anchor: "+err.Error(), err), nil
		} else if r != nil {
			// A lookup is a read: an unreadable holder answers as no holder, so the
			// op is not an oracle for which works the scope holds. Filing a
			// placeholder would claim the key, which the unique key refuses either way.
			if !in.CreatePlaceholder {
				return jsonResult(map[string]any{"work_key": wk, "found": false})
			}
			return workKeyUnreadable("resolve_anchor", wk), nil
		}
	}
	out := map[string]any{"work_key": wk, "found": true, "document_id": docID, "root_chunk_id": rootID, "placeholder": placeholder}
	if a := strings.TrimSpace(in.Anchor); a != "" {
		if err := d.assignAnchors(ctx, key, mscope, docID, rootID); err != nil {
			return errFrom("resolve_anchor: "+err.Error(), err), nil
		}
		at := time.Now().UnixNano()
		if in.AsOf != nil {
			at = *in.AsOf
		}
		res, err := d.query(ctx, key, `SELECT id FROM chunks
			WHERE document_id = ? AND (anchor = ? OR stable_id = ?)
			  AND (valid_at IS NULL OR valid_at <= ?) AND (invalid_at IS NULL OR invalid_at > ?)
			ORDER BY CASE WHEN anchor = ? THEN 0 ELSE 1 END, valid_at DESC LIMIT 1`, docID, a, a, at, at, a)
		if err != nil {
			return errFrom("resolve_anchor: "+err.Error(), err), nil
		}
		if len(res.Rows) > 0 {
			out["chunk_id"] = asStr(res.Rows[0][0])
		}
	}
	return jsonResult(out)
}

// workKeyClaimAllowed refuses a caller who may not write the whole scope a work
// key, the rule RFC FO applies to every identity key: a key is a claim on the
// scope's namespace, and a writer confined to part of it must not squat one.
func workKeyClaimAllowed(ctx context.Context, acl *docACL) *tools.Result {
	if acl == nil || acl.scopeWritableQuietly(ctx) {
		return nil
	}
	r := errPermission("a work key is claimed for the whole scope, and the caller's access-control rules do not let it write the whole scope",
		"Import without work_key, or ask an operator to file the work.")
	return &r
}

// workKeyUnreadable refuses a call whose work key is held by a document the
// caller may not read. That the key is taken cannot be hidden — the unique key
// refuses a second holder either way — but which document holds it, and its path,
// can be.
func workKeyUnreadable(op, wk string) tools.Result {
	return errPermission(op+": the work key "+wk+" is held by a document the caller's access-control rules do not let it read",
		"Do not retry it; work with a document the caller may reach.")
}

func placeholderSlug(wk string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(wk) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// editionLocks serialises, within this process, the reimports of one work and the
// placeholder filings for one work key. Another replica is not covered; there, the
// unique work_key index refuses a second claim, and the loser removes its copy.
type editionLocks struct {
	mu sync.Mutex
	m  map[string]*editionLock
}

// editionLock is a one-slot channel, so a waiter gives up when its call is
// cancelled; n counts holders and waiters, so the entry goes when nobody needs it.
type editionLock struct {
	slot chan struct{}
	n    int
}

func (l *editionLocks) lock(ctx context.Context, key sqlmem.ScopeKey, kind, name string) (func(), error) {
	k := strings.Join([]string{key.Tenant, key.Scope, key.ScopeID, kind, name}, "\x00")
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*editionLock{}
	}
	e := l.m[k]
	if e == nil {
		e = &editionLock{slot: make(chan struct{}, 1)}
		l.m[k] = e
	}
	e.n++
	l.mu.Unlock()
	forget := func() {
		l.mu.Lock()
		if e.n--; e.n == 0 {
			delete(l.m, k)
		}
		l.mu.Unlock()
	}
	select {
	case e.slot <- struct{}{}:
		return func() { <-e.slot; forget() }, nil
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	}
}

// incomingPart is one part of the edition being imported.
type incomingPart struct {
	c        mdChunk
	anchor   string
	parentAn string // "" = under the root
}

// reimportReport is what a keyed reimport did.
type reimportReport struct {
	Unchanged          int      `json:"unchanged"`
	Superseded         int      `json:"superseded"`
	Moved              int      `json:"moved"`
	Closed             int      `json:"closed"`
	Created            int      `json:"created"`
	StatementsCopied   int      `json:"statements_copied"`
	StatementsProposed int      `json:"statements_proposed"`
	changed            []string // ids of new, superseding and moved parts
}

// reimportMD reconciles an incoming edition onto the document it is an edition
// of, part by part, keyed by anchor:
//
//  1. same anchor, same body: untouched — ids, statements, evidence, tags survive;
//  2. a held part's body under another anchor (renumbered or moved): the part keeps
//     its id and stable_id and takes the new anchor — tried before 3, so the text
//     keeps its part when an inserted part shifts the anchors after it;
//  3. same anchor, new body, among what is left: superseded — the old part closes
//     at edition_date and a new one opens there, inheriting stable_id and tags,
//     joined by `supersedes`; its statements follow it where their evidence still
//     holds (moveStatements);
//  4. a held part nothing matched: it closes, never deleted;
//  5. an incoming part nothing matched: a new part, valid from edition_date.
func (d *Document) reimportMD(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, chunks []mdChunk, edges []mdEdge) (tools.Result, error) {
	edition := time.Now().UnixNano()
	if in.EditionDate != nil {
		edition = *in.EditionDate
	}
	wk := strings.TrimSpace(in.WorkKey)
	docID, rootID := in.DocumentID, ""
	var docUnlock func()
	// One reimport of a work at a time: each reads the held parts and then writes,
	// so two at once both saw the old part and left two in force under one anchor,
	// and two first editions both imported a document. Held from before the work is
	// looked up to after the last write. Work key first, then document: every path
	// takes them in that order.
	if wk != "" {
		unlock, err := d.editions.lock(ctx, key, "work", wk)
		if err != nil {
			return errFrom("import_md: "+err.Error(), err), nil
		}
		defer unlock()
	}
	lockDoc := func(id string) (tools.Result, bool) {
		unlock, err := d.editions.lock(ctx, key, "document", id)
		if err != nil {
			return errFrom("import_md: "+err.Error(), err), false
		}
		docUnlock = unlock
		return tools.Result{}, true
	}
	defer func() {
		if docUnlock != nil {
			docUnlock()
		}
	}()
	switch {
	case docID != "":
		if r, ok := lockDoc(docID); !ok {
			return r, nil
		}
		res, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ?`, docID)
		if err != nil {
			return errFrom("import_md: "+err.Error(), err), nil
		}
		if len(res.Rows) == 0 {
			return errNotFound("import_md: no such document: "+docID, "Reimport onto a document of this scope, or by work_key."), nil
		}
		rootID = asStr(res.Rows[0][0])
		if wk != "" {
			if r := d.claimWorkKey(ctx, key, rootID, wk); r != nil {
				return *r, nil
			}
		}
	case wk != "":
		var err error
		if docID, rootID, _, err = d.documentByWorkKey(ctx, key, wk); err != nil {
			return errFrom("import_md: "+err.Error(), err), nil
		}
		if docID != "" {
			if r, ok := lockDoc(docID); !ok {
				return r, nil
			}
		}
	default:
		return errValidation("import_md: reimport needs document_id or work_key — the document this is an edition of", ""), nil
	}
	acl := d.newDocACL(ctx, key)
	if docID == "" {
		// Asked before anything is written: a refusal after the import would leave
		// a keyless copy behind.
		if r := workKeyClaimAllowed(ctx, acl); r != nil {
			return *r, nil
		}
		// The first edition of a work: an ordinary import, then the work key.
		res, err := d.importMD(ctx, key, mscope, docInput{Markdown: in.Markdown, Path: in.Path, Title: in.Title,
			SourceURL: in.SourceURL, SourceKind: in.SourceKind, SourceTitle: in.SourceTitle})
		if err != nil || res.IsError {
			return res, err
		}
		docID, rootID = resultField(res, "document_id"), resultField(res, "root_chunk_id")
		if r := d.claimWorkKey(ctx, key, rootID, wk); r != nil {
			// Another process claimed the key first (the lock is per process): drop the
			// keyless copy rather than leave it beside the work.
			_, _ = d.deleteDocument(ctx, key, mscope, docInput{ID: docID})
			return *r, nil
		}
		if err := d.assignAnchors(ctx, key, mscope, docID, rootID); err != nil {
			return errFrom("import_md: "+err.Error(), err), nil
		}
		// importMD announced it already.
		return jsonResult(map[string]any{"document_id": docID, "root_chunk_id": rootID, "work_key": wk,
			"reimport": reimportReport{Created: len(chunks) - 1}})
	}
	if acl != nil {
		if r, err := acl.document(ctx, "import_md", "write", docID); err != nil {
			return errFrom("import_md: "+err.Error(), err), nil
		} else if r != nil {
			if ok, _ := acl.readable(ctx, docID); !ok && in.DocumentID == "" {
				return workKeyUnreadable("import_md", wk), nil
			}
			return *r, nil
		}
	}

	// Editions are applied in date order. One older than a part in force would be
	// reconciled against the newer text and replace it: currentParts reads what is
	// in force at the edition date, and the newer edition's parts are not yet.
	// Filling in an older edition behind a newer one (splitting the intervals) is
	// not supported, so it is refused rather than half done.
	fl, err := d.query(ctx, key, `SELECT MAX(valid_at) FROM chunks
		WHERE document_id = ? AND id <> ? AND (invalid_at IS NULL OR invalid_at > ?)`, docID, rootID, time.Now().UnixNano())
	if err != nil {
		return errFrom("import_md: "+err.Error(), err), nil
	}
	if len(fl.Rows) > 0 {
		if latest, ok := asInt64(fl.Rows[0][0]); ok && edition < latest {
			return errBusiness(fmt.Sprintf("import_md: edition_date %d is earlier than the edition already in force (its parts are valid from %d). Nothing was imported.", edition, latest),
				"Import editions in date order: pass an edition_date at or after the one in force. An older edition cannot be inserted behind a newer one."), nil
		}
	}
	if err := d.assignAnchors(ctx, key, mscope, docID, rootID); err != nil {
		return errFrom("import_md: "+err.Error(), err), nil
	}
	held, err := d.currentParts(ctx, key, mscope, docID, rootID, edition)
	if err != nil {
		return errFrom("import_md: "+err.Error(), err), nil
	}

	// The incoming edition's parts, keyed the same way the held ones were.
	keyer := &anchorKeyer{d: d, ctx: ctx, grammar: d.anchorGrammarFor(ctx, key, rootID), taken: map[string]bool{}}
	var parts []incomingPart
	type frame struct {
		level         int
		title, anchor string
	}
	var stack []frame
	for _, c := range chunks[1:] {
		for len(stack) > 0 && stack[len(stack)-1].level >= c.level {
			stack = stack[:len(stack)-1]
		}
		path := make([]string, 0, len(stack)+1)
		for _, f := range stack {
			path = append(path, f.title)
		}
		path = append(path, c.title)
		p := incomingPart{c: c, anchor: keyer.next(path)}
		if len(stack) > 0 {
			p.parentAn = stack[len(stack)-1].anchor
		}
		parts = append(parts, p)
		stack = append(stack, frame{level: c.level, title: c.title, anchor: p.anchor})
	}

	// A part whose new version an earlier, interrupted run of this edition already
	// wrote (joined by `supersedes`) is finished here, not matched again: its
	// statements are carried and it closes below, like any superseded part.
	remap := map[string]string{} // superseded part → its new version
	newBody := map[string]string{}
	var toClose []string
	pend, err := d.query(ctx, key, `SELECT e.from_id, e.to_id FROM chunk_edges e
		JOIN chunks n ON n.id = e.from_id JOIN chunks o ON o.id = e.to_id
		WHERE e.kind = 'supersedes' AND n.document_id = ? AND o.document_id = ?
		  AND (n.invalid_at IS NULL OR n.invalid_at > ?) AND (o.invalid_at IS NULL OR o.invalid_at > ?)`,
		docID, docID, edition, edition)
	if err != nil {
		return errFrom("import_md: "+err.Error(), err), nil
	}
	for _, r := range pend.Rows {
		remap[asStr(r[1])] = asStr(r[0])
	}
	bodyOf := map[string]string{}
	for _, h := range held {
		bodyOf[h.id] = h.body
	}
	live := held[:0:0]
	for _, h := range held {
		if nv, ok := remap[h.id]; ok {
			newBody[nv] = bodyOf[nv]
			toClose = append(toClose, h.id)
			continue
		}
		live = append(live, h)
	}
	held = live

	// Match the edition to the held parts, in passes, before anything is written:
	//
	//  1. same anchor, same body: unchanged;
	//  2. a held part's body under another anchor: a move — the text keeps its part,
	//     so inserting an article and renumbering the rest is a run of moves, never a
	//     chain of supersessions that would hand one article's links to the next;
	//  3. same anchor, new body, among what is left: superseded;
	//  4. anything else: new.
	const (
		actCreate = iota
		actUnchanged
		actMove
		actSupersede
	)
	type step struct {
		act  int
		held *editionPart
	}
	steps := make([]step, len(parts))
	byAnchor := map[string][]*editionPart{}
	byBody := map[string][]*editionPart{}
	for i := range held {
		byAnchor[held[i].anchor] = append(byAnchor[held[i].anchor], &held[i])
		b := strings.TrimSpace(held[i].body)
		byBody[b] = append(byBody[b], &held[i])
	}
	incomingAnchors := map[string]bool{}
	for _, p := range parts {
		incomingAnchors[p.anchor] = true
	}
	matched := map[string]bool{}
	take := func(i, act int, h *editionPart) {
		matched[h.id] = true
		steps[i] = step{act: act, held: h}
	}
	for i, p := range parts {
		for _, h := range byAnchor[p.anchor] {
			if !matched[h.id] && strings.TrimSpace(h.body) == strings.TrimSpace(p.c.body) {
				take(i, actUnchanged, h)
				break
			}
		}
	}
	for i, p := range parts {
		body := strings.TrimSpace(p.c.body)
		if steps[i].held != nil || body == "" {
			continue
		}
		// Prefer a part whose anchor the edition no longer uses: of two copies of the
		// same text, the one that lost its place is the one that moved.
		var mv *editionPart
		for _, cand := range byBody[body] {
			if matched[cand.id] {
				continue
			}
			if !incomingAnchors[cand.anchor] {
				mv = cand
				break
			}
			if mv == nil {
				mv = cand
			}
		}
		if mv != nil {
			take(i, actMove, mv)
		}
	}
	for i, p := range parts {
		if steps[i].held != nil {
			continue
		}
		for _, h := range byAnchor[p.anchor] {
			if !matched[h.id] {
				take(i, actSupersede, h)
				break
			}
		}
	}

	// stable ids already given out in this document: a new part does not take one
	// that names another part's text.
	stables := map[string]bool{}
	sres, err := d.query(ctx, key, `SELECT DISTINCT stable_id FROM chunks WHERE document_id = ? AND stable_id IS NOT NULL`, docID)
	if err != nil {
		return errFrom("import_md: "+err.Error(), err), nil
	}
	for _, r := range sres.Rows {
		stables[asStr(r[0])] = true
	}
	freshStable := func(anchor string) string {
		s := anchor
		if stables[s] {
			s = fmt.Sprintf("%s@%d", anchor, edition)
			for n := 2; stables[s]; n++ {
				s = fmt.Sprintf("%s@%d~%d", anchor, edition, n)
			}
		}
		stables[s] = true
		return s
	}

	rep := reimportReport{}
	current := map[string]string{"": rootID} // anchor -> the chunk in force for it now
	order := map[string][]string{}           // parent chunk -> children in edition order
	bodyOrigin := ""
	if _, imported := importProvenanceFrom(ctx); imported {
		bodyOrigin = store.OriginImport
	}

	for i, p := range parts {
		parent := current[p.parentAn]
		if parent == "" {
			parent = rootID
		}
		var id string
		switch st := steps[i]; st.act {
		case actUnchanged:
			id = st.held.id
			if err := d.exec(ctx, key, `UPDATE chunks SET parent_id = ?, title = ? WHERE id = ?`, parent, titleOrUntitled(p.c.title), id); err != nil {
				return errFrom("import_md: "+err.Error(), err), nil
			}
			rep.Unchanged++
		case actMove:
			id = st.held.id
			if err := d.exec(ctx, key, `UPDATE chunks SET parent_id = ?, title = ?, anchor = ? WHERE id = ?`,
				parent, titleOrUntitled(p.c.title), p.anchor, id); err != nil {
				return errFrom("import_md: "+err.Error(), err), nil
			}
			rep.Moved++
			rep.changed = append(rep.changed, id)
		case actSupersede:
			newID, r, err := d.supersedePart(ctx, key, mscope, docID, st.held, p, parent, edition, bodyOrigin)
			if err != nil {
				return errFrom("import_md: "+err.Error(), err), nil
			}
			if r != nil {
				return *r, nil
			}
			id = newID
			remap[st.held.id], newBody[id] = id, p.c.body
			toClose = append(toClose, st.held.id)
			rep.Superseded++
			rep.changed = append(rep.changed, id)
		default:
			cc, _ := d.createChunk(ctx, key, mscope, docInput{
				DocumentID: docID, ParentID: parent, Title: titleOrUntitled(p.c.title),
				Type: p.c.typ, Status: p.c.status, Body: p.c.body, Fields: p.c.fields,
				ValidAt: &edition, bodyOrigin: bodyOrigin,
			})
			if cc.IsError {
				return cc, nil
			}
			id = resultField(cc, "id")
			if err := d.exec(ctx, key, `UPDATE chunks SET anchor = ?, stable_id = ? WHERE id = ?`, p.anchor, freshStable(p.anchor), id); err != nil {
				return errFrom("import_md: "+err.Error(), err), nil
			}
			rep.Created++
			rep.changed = append(rep.changed, id)
		}
		current[p.anchor] = id
		order[parent] = append(order[parent], id)
	}

	// Statements follow the superseded parts BEFORE those close, so a run cut short
	// here leaves them open and paired, and running it again finishes the carry.
	copied, proposed, err := d.moveStatements(ctx, key, acl, remap, newBody)
	if err != nil {
		return errFrom("import_md: "+err.Error(), err), nil
	}
	rep.StatementsCopied, rep.StatementsProposed = copied, proposed

	// Superseded parts, and parts the edition no longer has, close at its date;
	// nothing is deleted.
	for i := range held {
		if !matched[held[i].id] {
			toClose = append(toClose, held[i].id)
			rep.Closed++
		}
	}
	for _, id := range toClose {
		if err := d.exec(ctx, key, `UPDATE chunks SET invalid_at = ? WHERE id = ?`, edition, id); err != nil {
			return errFrom("import_md: "+err.Error(), err), nil
		}
	}
	// The root: its title and preamble follow the edition. Updated in place, not
	// superseded — the root IS the document (its id, its Path name, its work key) —
	// and an earlier preamble stays readable in the root's history.
	if changed, r := d.reconcileRoot(ctx, key, mscope, rootID, chunks[0]); r != nil {
		return *r, nil
	} else if changed {
		rep.changed = append(rep.changed, rootID)
	}
	// Sibling order follows the edition.
	for _, kids := range order {
		for i, id := range kids {
			if err := d.exec(ctx, key, `UPDATE chunks SET position = ? WHERE id = ?`, i, id); err != nil {
				return errFrom("import_md: "+err.Error(), err), nil
			}
		}
	}
	// A placeholder filed for this work is now held.
	_ = d.exec(ctx, key, `UPDATE documents SET status = NULL WHERE id = ? AND status = ?`, docID, placeholderStatus)
	_ = d.exec(ctx, key, `UPDATE chunks SET status = NULL WHERE id = ? AND status = ?`, rootID, placeholderStatus)

	d.publishIngested(ctx, key, mscope, docID, "reimported", rep.changed)
	out := map[string]any{"document_id": docID, "root_chunk_id": rootID, "edition_date": edition, "reimport": rep}
	if wk != "" {
		out["work_key"] = wk
	}
	if len(rep.changed) > 0 {
		out["changed_chunk_ids"] = rep.changed
	}
	return jsonResult(out)
}

// reconcileRoot brings the root's title and body to the edition's, through
// update_chunk, so the body's revision log, embedding and [[name]] links follow as
// for any edit. Reports whether anything changed.
func (d *Document) reconcileRoot(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, rootID string, c mdChunk) (bool, *tools.Result) {
	fail := func(err error) (bool, *tools.Result) {
		r := errFrom("import_md: root: "+err.Error(), err)
		return false, &r
	}
	row, ok, err := d.getChunkRow(ctx, key, rootID)
	if err != nil {
		return fail(err)
	}
	if !ok {
		return fail(fmt.Errorf("no such chunk %s", rootID))
	}
	cb, err := d.readBody(ctx, mscope, key, rootID)
	if err != nil {
		return fail(err)
	}
	upd := map[string]any{"id": rootID, "revision": row.Revision}
	in := docInput{ID: rootID, Revision: &row.Revision}
	if t := titleOrUntitled(c.title); t != row.Title {
		upd["title"], in.Title = t, t
	}
	if strings.TrimSpace(c.body) != strings.TrimSpace(cb.Body) {
		upd["body"], in.Body = c.body, c.body
	}
	if len(upd) == 2 {
		return false, nil
	}
	raw, _ := json.Marshal(upd)
	res, err := d.updateChunk(ctx, key, mscope, in, raw)
	if err != nil {
		return fail(err)
	}
	if res.IsError {
		return false, &res
	}
	return true, nil
}

// supersedePart writes a changed part's new version: it opens at the edition date
// with the held version's stable_id and tags, joined to it by `supersedes`. The
// held version closes, and its statements follow, once every part is reconciled
// (reimportMD).
//
// The anchor, the tags and the `supersedes` edge are one transaction: the edge is
// how a rerun recognises a new version an interrupted run already wrote, so it must
// not exist without the rest.
func (d *Document) supersedePart(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, docID string, old *editionPart, p incomingPart, parent string, edition int64, bodyOrigin string) (string, *tools.Result, error) {
	cc, _ := d.createChunk(ctx, key, mscope, docInput{
		DocumentID: docID, ParentID: parent, Title: titleOrUntitled(p.c.title),
		Type: firstNonEmpty(p.c.typ, old.typ), Status: firstNonEmpty(p.c.status, old.status),
		Body: p.c.body, Fields: p.c.fields, ValidAt: &edition, bodyOrigin: bodyOrigin,
	})
	if cc.IsError {
		return "", &cc, nil
	}
	newID := resultField(cc, "id")
	stable := old.stableID
	if stable == "" {
		stable = old.anchor
	}
	now := time.Now().UnixNano()
	err := d.withSqlTxn(ctx, key, func(txn string) error {
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{`UPDATE chunks SET anchor = ?, stable_id = ? WHERE id = ?`, []any{p.anchor, stable, newID}},
			{`INSERT INTO chunk_tags (chunk_id, tag) SELECT ?, tag FROM chunk_tags WHERE chunk_id = ?`, []any{newID, old.id}},
			{`INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at, method, valid_at) VALUES (?, ?, ?, 'supersedes', ?, ?, ?)`,
				[]any{newDocID(), newID, old.id, now, edgeMethodRuntime, edition}},
		} {
			if err := d.execTxn(ctx, txn, s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return newID, nil, nil
}

// moveStatements carries the superseded parts' statements to their new versions.
// remap names each superseded part's new version, newBody its text.
//
// Each statement is visited ONCE, with both ends remapped: one between two parts
// that both changed is copied once, onto the two new versions.
//
// A statement whose evidence quoting a superseded part still occurs in that part's
// new text is COPIED (attributes.revision_of naming the original); so is a
// statement with no evidence there, and a manual one, unconditionally — a person's
// link is not something a text change can disprove. Anything else stays on the
// old part, where it still holds for the old part's interval: its evidence there is
// marked stale and its status drops to proposed, so it is re-verified rather than
// silently carried or silently lost.
//
// An inline [[name]] link (auto) is never copied: it belongs to the text that
// makes it, and the new version's own [[name]] links were derived from its body
// when it was written. A copy would outlive an edition that dropped the link.
//
// A statement whose other end is in a document the caller may not link is left
// exactly as it is: re-checking or copying it is a change to that document's
// relations, which the reimport's write on this document does not authorize.
//
// Rerunnable: a copy already present (an earlier, interrupted run) is left alone,
// and each copy is written with its evidence in one transaction, so a present
// copy is a complete one.
func (d *Document) moveStatements(ctx context.Context, key sqlmem.ScopeKey, acl *docACL, remap, newBody map[string]string) (copied, proposed int, err error) {
	olds := make([]string, 0, len(remap))
	for old := range remap {
		olds = append(olds, old)
	}
	sort.Strings(olds)
	seen := map[string]bool{}
	var stmts []statementRow
	var ids []string
	for _, old := range olds {
		res, err := d.query(ctx, key, `SELECT `+statementCols+` FROM chunk_edges e
			WHERE (e.from_id = ? OR e.to_id = ?) AND e.kind NOT IN ('supersedes', 'about')
			  AND e.status <> ? AND e.expired_at IS NULL`, old, old, edgeStatusRejected)
		if err != nil {
			return 0, 0, err
		}
		for _, r := range res.Rows {
			s := scanStatement(r)
			if seen[s.ID] || s.Auto {
				continue
			}
			seen[s.ID] = true
			stmts = append(stmts, s)
			ids = append(ids, s.ID)
		}
	}
	ev, _, err := d.evidenceFor(ctx, key, ids, 0)
	if err != nil {
		return 0, 0, err
	}
	mapped := func(id string) string {
		if nid, ok := remap[id]; ok {
			return nid
		}
		return id
	}
	now := time.Now().UnixNano()
	for _, s := range stmts {
		if acl != nil {
			var ends []string
			for _, end := range []string{s.FromID, s.ToID} {
				if _, ours := remap[end]; !ours {
					ends = append(ends, end)
				}
			}
			if len(ends) == 0 {
				ends = []string{s.FromID} // both ends in this document: one verdict
			}
			denied := false
			for _, end := range ends {
				r, err := acl.chunk(ctx, "import_md", "link", end)
				if err != nil {
					return copied, proposed, err
				}
				denied = denied || r != nil
			}
			if denied {
				continue
			}
		}
		holds := true
		for _, e := range ev[s.ID] {
			if nid, ok := remap[e.ChunkID]; ok && !strings.Contains(newBody[nid], e.Quote) {
				holds = false
			}
		}
		if !holds && s.Method != edgeMethodManual {
			for _, end := range []string{s.FromID, s.ToID} {
				if _, ours := remap[end]; !ours {
					continue
				}
				if err := d.exec(ctx, key, `UPDATE chunk_edge_evidence SET stale = 1 WHERE edge_id = ? AND chunk_id = ?`, s.ID, end); err != nil {
					return copied, proposed, err
				}
			}
			if s.Status == edgeStatusAsserted {
				if err := d.exec(ctx, key, `UPDATE chunk_edges SET status = ? WHERE id = ?`, edgeStatusProposed, s.ID); err != nil {
					return copied, proposed, err
				}
			}
			proposed++
			continue
		}
		from, to := mapped(s.FromID), mapped(s.ToID)
		if have, err := d.statementByKey(ctx, key, from, to, s.Kind, s.ValidAt, s.Discriminator); err != nil {
			return copied, proposed, err
		} else if have != nil {
			continue
		}
		attrs := map[string]any{}
		for k, v := range s.Attributes {
			attrs[k] = v
		}
		attrs["revision_of"] = s.ID
		ab, _ := json.Marshal(attrs)
		auto := 0
		if s.Auto {
			auto = 1
		}
		copyID := newDocID()
		err := d.withSqlTxn(ctx, key, func(txn string) error {
			if err := d.execTxn(ctx, txn, `INSERT INTO chunk_edges
				(id, from_id, to_id, kind, created_at, auto, status, method, confidence, valid_at, invalid_at,
				 actor, run_id, ontology_version, discriminator, attributes)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				copyID, from, to, s.Kind, now, auto, s.Status, s.Method, s.Confidence, s.ValidAt, s.InvalidAt,
				nullIfEmpty(s.Actor), nullIfEmpty(s.RunID), s.OntologyVersion, s.Discriminator, string(ab)); err != nil {
				return err
			}
			for _, e := range ev[s.ID] {
				chunkID, stale := e.ChunkID, 0
				var start, end any
				if e.CharStart != nil {
					start, end = *e.CharStart, *e.CharEnd
				}
				if nid, ok := remap[chunkID]; ok {
					if st, en, ok := quoteSpan(newBody[nid], e.Quote, nil, nil); ok {
						chunkID, start, end = nid, st, en
					} else {
						stale = 1 // a manual statement copied though its quote is gone
					}
				}
				if err := d.execTxn(ctx, txn, `INSERT INTO chunk_edge_evidence
					(id, edge_id, chunk_id, session_id, event_seq, ref_scope, ref_natural_key, quote, char_start, char_end, stale, created_at, run_id)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					newDocID(), copyID, nullIfEmpty(chunkID), nullIfEmpty(e.SessionID), e.EventSeq,
					nullIfEmpty(e.Scope), nullIfEmpty(e.NaturalKey), e.Quote, start, end, stale, now, nullIfEmpty(tools.RunID(ctx))); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return copied, proposed, err
		}
		copied++
	}
	return copied, proposed, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// publishIngested tells the scope's `documents/ingested` channel that a document
// arrived or changed by import — what makes "on import" an event a links agent
// can subscribe to, rather than a poll.
func (d *Document) publishIngested(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, docID, reason string, changed []string) {
	if d.Bus == nil {
		return
	}
	ev := map[string]any{"document_id": docID, "reason": reason, "timestamp": time.Now().UnixNano()}
	if len(changed) > 0 {
		ev["changed_chunk_ids"] = changed
	}
	payload, _ := json.Marshal(ev)
	w := d.Writer
	if w == nil {
		w = &channels.StorePublisher{Store: d.Store, Bus: d.Bus}
	}
	_, _ = w.Write(ctx, channels.WriteRequest{
		Channel:     "documents/ingested",
		TenantID:    tools.RunIdentity(ctx).TenantID,
		Scope:       mscope,
		ScopeID:     docMemScopeID(mscope, key),
		Payload:     payload,
		PublishedBy: tools.RunIdentity(ctx).UserID,
		MaxMessages: 256,
	})
}
