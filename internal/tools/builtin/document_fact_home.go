package builtin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// document_fact_home.go — RFC CV P3: move a fact into the document that IS its
// subject, and move the subject with it.
//
// A store written before subject-homing keeps every fact in ONE shared document,
// `/memory/entities`, with the subject as a sibling chunk and an `about` edge joining
// them. A store written after it keeps `/facts/<subject-slug>` per subject, whose ROOT
// chunk is the entity and whose children are its facts.
//
// THIS IS NOT TIDYING. `chunk_memory_meta.natural_key` is UNIQUE per scope, so while
// the old subject node holds `person:denn` the consolidator CANNOT create the document
// that claims the same key — measured: create_document fails on the constraint, so
// subjectDoc returns nothing, the fact is filed in the shared document AND its `about`
// edge is never written. A fact unreachable from the person it is about is the whole
// reason the entity tier exists. Until a scope is migrated it runs on the fallback
// mirrorEntity keeps for exactly this window; after it, the new shape is available.
//
// IT MOVES, IT DOES NOT COPY — the subject node included. A chunk keeps its id, so its
// body row, its embedding, its bi-temporal sidecar, its span, its access counters and
// every edge it has follow it for free. The subject node BECOMES the new document's
// root rather than being re-created beside it, which is why no edge has to be
// re-pointed and why the unique key is never held twice: there is only ever one chunk
// claiming `person:denn`, and it changes address.
//
// The move is `document_id` + `parent_id`, which is exactly the pair move_chunk refuses
// to change (a chunk parented across documents is corruption from the other side) — so
// this cannot be expressed as a sequence of tool calls, and that is why it is
// server-side.
//
// THE UNIT OF REFUSAL IS THE SUBJECT, not the scope. The collapse refused a whole scope
// because it DELETED rows: a partial run there left the operator with a success and
// some facts one run away from being dropped. Nothing here is deleted but an empty
// placeholder root, so a subject that cannot be homed — its type is no longer declared,
// its slug already names a different entity — is left exactly where it is, reported,
// and picked up by the next run once the operator has fixed the cause.
//
// A FACT WITH NO SUBJECT STAYS PUT, and that is the same refusal mirrorEntity makes
// when it writes one: there is no thing to file it under, and inventing one is how two
// different things end up merged onto a single node. `/memory/entities` remains the
// home for facts about nobody. `remember`-written facts stay too: they carry a subject
// on the fact chunk itself and have no subject node, so there is no natural key to
// follow, and deriving one would mean a Go twin of the bundle's slug.

// subjectDocPathPrefix is where a subject's document lives — `/facts/<slug>`, the
// same path subjectDoc() creates, because the migration must land on the document
// the next consolidation pass will find rather than a parallel one.
const subjectDocPathPrefix = "/facts/"

// aboutEdgeKind joins a fact to the subject it is about.
const aboutEdgeKind = "about"

// FactHomingSkip is one subject the migration did not move, and why. Reported
// rather than logged: a subject left behind is work the operator still has to do,
// and a count alone does not say what to fix.
type FactHomingSkip struct {
	NaturalKey string `json:"natural_key"`
	Subject    string `json:"subject,omitempty"`
	Facts      int    `json:"facts"`
	Reason     string `json:"reason"`
}

// FactHomingReport is what one run of the migration did (or, on a dry run, would
// do) to one scope.
type FactHomingReport struct {
	Scope               string           `json:"scope"`
	ScopeID             string           `json:"scope_id"`
	Tenant              string           `json:"tenant"`
	SharedDocument      string           `json:"shared_document,omitempty"`
	Subjects            int              `json:"subjects"`
	SubjectsHomed       int              `json:"subjects_homed"`
	DocumentsCreated    int              `json:"documents_created"`
	DocumentsAdopted    int              `json:"documents_adopted"`
	FactsMoved          int              `json:"facts_moved"`
	FactsWithoutSubject int              `json:"facts_without_subject"`
	Skipped             []FactHomingSkip `json:"skipped,omitempty"`
	// PathWarnings names subjects that WERE homed but could not be given their
	// Path-tree name — reachable by search and by id, invisible to a path browse.
	// Separate from Skipped because the two need opposite actions: a skip is work
	// still to do, this is work done and mis-filed.
	PathWarnings []string `json:"path_warnings,omitempty"`
	DryRun       bool     `json:"dry_run"`
	Note         string   `json:"note,omitempty"`
}

// homingSubject is one old-shape subject node and the facts pointing at it.
type homingSubject struct {
	chunkID    string
	naturalKey string
	slug       string
	typ        string
	status     string
	name       string
	factIDs    []string
}

// HomeFactsUnderSubjects moves every fact in the scope's shared entity document
// into its subject's own `/facts/<slug>` document, creating that document when it
// does not exist yet and removing the now-duplicate subject node from the shared
// one. It is IDEMPOTENT: a second run finds nothing left to move, and a run
// interrupted between creating a subject's document and deleting its old node
// leaves a duplicate that the next run resolves — duplication, never loss, is the
// failure mode chosen at every step.
func (d *Document) HomeFactsUnderSubjects(ctx context.Context, tenantID string,
	scope store.MemoryScope, scopeID string, dryRun bool) (FactHomingReport, error) {

	rep := FactHomingReport{Scope: string(scope), ScopeID: scopeID, Tenant: tenantID, DryRun: dryRun}
	if d.SqlMem == nil || d.Store == nil {
		return rep, fmt.Errorf("subject homing requires SQL Memory and a persistent store")
	}
	key, ok := docScopeKeyFor(tenantID, scope, scopeID)
	if !ok {
		return rep, fmt.Errorf("scope %s/%s is not a document scope", scope, scopeID)
	}
	// Every dirent read and every document write below reads the tenant off the
	// context (direntTenant), so an operator-triggered call has to stamp it — there
	// is no run identity on an HTTP request, and an unstamped call would resolve
	// paths in the default tenant's tree while writing SQL in the named one.
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: tenantID})

	sharedID, err := d.docIDFromInput(ctx, key, docInput{Path: rememberedFactsPath})
	if err != nil {
		// No shared entity document is the state of a scope that was never written
		// in the old shape, or has already been migrated and had it removed. Nothing
		// to do is a successful outcome, not a fault.
		rep.Note = "no " + rememberedFactsPath + " document in this scope — nothing to home"
		return rep, nil
	}
	rep.SharedDocument = sharedID

	subjects, factsNoSubject, err := d.collectHomingSubjects(ctx, key, sharedID)
	if err != nil {
		return rep, err
	}
	rep.Subjects = len(subjects)
	rep.FactsWithoutSubject = factsNoSubject

	for i := range subjects {
		s := subjects[i]
		if s.slug == "" {
			rep.Skipped = append(rep.Skipped, FactHomingSkip{
				NaturalKey: s.naturalKey, Subject: s.name, Facts: len(s.factIDs),
				Reason: "natural key is not an entity key (want <type>:<slug>), so there is no identity path to home it at",
			})
			continue
		}
		// The ontology gate, applied HERE because the document is assembled below
		// rather than through create_document. A type the tenant has stopped declaring
		// is a subject left where it is, not one filed under a kind nobody can find.
		if refused := d.gateEntityType(ctx, docInput{Type: s.typ, Subject: s.name}); refused != nil {
			rep.Skipped = append(rep.Skipped, FactHomingSkip{
				NaturalKey: s.naturalKey, Subject: s.name, Facts: len(s.factIDs),
				Reason: strings.TrimSpace(refused.Text),
			})
			continue
		}
		path := subjectDocPathPrefix + s.slug
		target, terr := d.subjectHomeTarget(ctx, key, path, s)
		if terr != nil {
			rep.Skipped = append(rep.Skipped, FactHomingSkip{
				NaturalKey: s.naturalKey, Subject: s.name, Facts: len(s.factIDs),
				Reason: terr.Error(),
			})
			continue
		}
		rep.SubjectsHomed++
		if target.adopt {
			rep.DocumentsAdopted++
		} else {
			rep.DocumentsCreated++
		}
		rep.FactsMoved += len(s.factIDs)
		if dryRun {
			continue
		}
		if err := d.homeOneSubject(ctx, key, target, s); err != nil {
			return rep, fmt.Errorf("homing %s: %w (%d subjects were already homed and are committed)",
				s.naturalKey, err, rep.SubjectsHomed-1)
		}
		// The placeholder root an adopted document came with, after its SQL side is
		// committed — bodies live in a different database and cannot join that
		// transaction, and an orphaned body is invisible dead k/v where an orphaned
		// row would not be.
		if target.dropRoot != "" {
			_, _ = d.Store.MemoryDelete(ctx, tenantID, scope, key.ScopeID, chunkBodyKey(target.dropRoot))
		}
		// The Path-tree name LAST, and only for a document this run created: a failure
		// here leaves the facts correctly homed and merely unnamed, which is the
		// failure direction create_document already chose (it reports a path_warning
		// rather than undoing the document).
		if !target.adopt {
			if _, perr := d.registerDocDirent(ctx, key, target.docID, path); perr != nil {
				rep.PathWarnings = append(rep.PathWarnings,
					s.naturalKey+" was homed in document "+target.docID+" but "+path+
						" could not be registered, so it is reachable by search and by id "+
						"and not by path: "+perr.Error())
			}
		}
	}

	if dryRun {
		rep.Note = strings.TrimSpace(rep.Note + " DRY RUN — nothing was created, moved or removed. Re-send with dry_run=false.")
	}
	return rep, nil
}

// collectHomingSubjects reads the shared entity document and pairs each subject
// node with the facts whose `about` edge points at it.
//
// WHY THE EDGE DECIDES, and not the chunk type. A distilled fact's type is the
// constant "fact", which makes `type = 'fact'` look like the obvious test — but
// `remember` stamps the CALLER's type, so that filter drops operator-remembered
// facts, and dropping them here would mean leaving them behind in a document whose
// subject node has just been deleted. Being the TARGET of an `about` edge is what an
// identity node structurally is, and being its SOURCE is what a fact is; that is the
// rule listFacts already applies for the same reason, and it needs no re-stamping so
// it holds for stores that already exist.
//
// The shared document is the ONLY source considered. A criterion like "every entity
// node that is not a document root" would also sweep up the ontology document's type
// chunks, which are entities of a different kind entirely — filing `person` under
// `/facts/person` would make a type into a subject.
func (d *Document) collectHomingSubjects(ctx context.Context, key sqlmem.ScopeKey, sharedID string) ([]homingSubject, int, error) {
	rootRes, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ?`, sharedID)
	if err != nil {
		return nil, 0, err
	}
	sharedRoot := ""
	if len(rootRes.Rows) > 0 {
		sharedRoot = asStr(rootRes.Rows[0][0])
	}

	res, err := d.query(ctx, key,
		`SELECT c.id, coalesce(c.title, ''), coalesce(c.type, ''), coalesce(c.status, ''), coalesce(m.natural_key, ''), coalesce(m.subject, '')
		   FROM chunks c LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id
		  WHERE c.document_id = ?`, sharedID)
	if err != nil {
		return nil, 0, err
	}
	// A TRUNCATED READ IS A FAULT, not a smaller job. Migrating the visible half and
	// reporting success would leave the rest silently behind, which is the exact
	// shape of the purge this project already trusted for its own count.
	if res.Truncated {
		return nil, 0, fmt.Errorf("the shared entity document has more chunks than one read returns; "+
			"refusing to home a partial view of it (document %s)", sharedID)
	}

	type localChunk struct{ title, typ, status, naturalKey, subject string }
	here := map[string]localChunk{}
	for _, row := range res.Rows {
		if len(row) < 6 {
			continue
		}
		id := asStr(row[0])
		if id == "" || id == sharedRoot {
			continue
		}
		here[id] = localChunk{asStr(row[1]), asStr(row[2]), asStr(row[3]), asStr(row[4]), asStr(row[5])}
	}

	eres, err := d.query(ctx, key, `SELECT from_id, to_id FROM chunk_edges WHERE kind = ?`, aboutEdgeKind)
	if err != nil {
		return nil, 0, err
	}
	if eres.Truncated {
		return nil, 0, fmt.Errorf("the scope has more `about` edges than one read returns; " +
			"refusing to home facts from a partial view of the graph")
	}

	byID := map[string]*homingSubject{}
	isFact := map[string]bool{}
	type aboutEdge struct{ from, to string }
	var edges []aboutEdge
	for _, row := range eres.Rows {
		if len(row) < 2 {
			continue
		}
		from, to := asStr(row[0]), asStr(row[1])
		c, inDoc := here[to]
		if !inDoc {
			continue // the subject lives elsewhere already — nothing here to home it at
		}
		// BELT AND BRACES: the two planes share the `memory/` keyspace verbatim, so a
		// chunk keyed there is a FACT however the graph is shaped. Treating one as a
		// subject would file a fact's own key as an identity path.
		if c.naturalKey != "" && strings.HasPrefix(c.naturalKey, MemoryFactKeyPrefix) {
			continue
		}
		if _, ok := here[from]; !ok {
			continue // the fact is not in this document; only its subject is
		}
		if _, seen := byID[to]; !seen {
			name := c.subject
			if name == "" {
				name = c.title
			}
			byID[to] = &homingSubject{
				chunkID: to, naturalKey: c.naturalKey, slug: entitySlugOfKey(c.naturalKey),
				typ: c.typ, status: c.status, name: name,
			}
		}
		isFact[from] = true
		edges = append(edges, aboutEdge{from, to})
	}

	// ONE HOME, MANY REFERENCES. Containment is single-parent by construction, so a
	// fact naming several subjects is filed under the first in natural-key order and
	// stays REACHABLE from the rest through the edges it keeps — which is what the
	// edge plane is for. Deterministic rather than arbitrary, so a re-run of the
	// migration reaches the same layout.
	sort.SliceStable(edges, func(i, j int) bool {
		ki, kj := byID[edges[i].to].naturalKey, byID[edges[j].to].naturalKey
		if ki != kj {
			return ki < kj
		}
		return edges[i].from < edges[j].from
	})
	homed := map[string]bool{}
	for _, e := range edges {
		if homed[e.from] {
			continue
		}
		homed[e.from] = true
		byID[e.to].factIDs = append(byID[e.to].factIDs, e.from)
	}

	out := make([]homingSubject, 0, len(byID))
	for _, s := range byID {
		sort.Strings(s.factIDs)
		out = append(out, *s)
	}
	// Deterministic across runs AND across the arbitrary order two subject nodes for
	// one slug come back in: the lower natural key is resolved first, so a merge
	// lands the same way twice.
	sort.SliceStable(out, func(i, j int) bool { return out[i].naturalKey < out[j].naturalKey })

	// A chunk here with a sidecar that no `about` edge reaches is a fact with nowhere
	// to go — one the pass wrote about nobody, or one `remember` wrote with its
	// subject on the fact itself and no subject node at all. It STAYS, and it is
	// counted so the gap reads as work still to do rather than a silent default.
	noSubject := 0
	for id, c := range here {
		// Keyed in the FACT keyspace is what makes a chunk a fact here — the one
		// namespace the k/v and chunk planes share verbatim. Testing merely "has a
		// sidecar" would also count a subject node whose facts happen to live
		// elsewhere, and report an identity as a fact with nowhere to go.
		if isFact[id] || !strings.HasPrefix(c.naturalKey, MemoryFactKeyPrefix) {
			continue
		}
		noSubject++
	}
	return out, noSubject, nil
}

// entitySlugOfKey takes the identity half of an entity natural key `<type>:<slug>`.
//
// The slug is READ from the key rather than recomputed from the name, deliberately:
// it was produced by the bundle's own slug(), and a Go twin of that function is a
// second definition of identity that can drift from the one that writes it — the
// migration would then create `/facts/<other-slug>` beside the document the next
// consolidation pass resolves.
func entitySlugOfKey(naturalKey string) string {
	i := strings.Index(naturalKey, ":")
	if i < 0 {
		return ""
	}
	slug := strings.TrimSpace(naturalKey[i+1:])
	// The slug becomes one Path segment. The bundle emits [a-z0-9-], but a key
	// hand-written through the tool surface need not, and a separator here would
	// silently re-home the subject somewhere else in the tree.
	if slug == "" || strings.ContainsAny(slug, "/\\") || slug == "." || slug == ".." {
		return ""
	}
	return slug
}

// homeTarget is where one subject is going: the document that will hold it, and the
// placeholder root (if any) it displaces.
type homeTarget struct {
	docID    string
	adopt    bool   // the document already existed at the path
	dropRoot string // the empty root an adopted document came with, to be removed
}

// subjectHomeTarget decides where a subject's document is, or that one has to be made.
//
// Adoption matters because of the leak this migration also ends: create_document used
// to insert the document and its root BEFORE the entity-metadata write that fails on
// the unique key, so an un-migrated scope accumulated one empty, path-less document per
// subject per pass. Those are exactly the documents to reuse.
func (d *Document) subjectHomeTarget(ctx context.Context, key sqlmem.ScopeKey, path string, s homingSubject) (homeTarget, error) {
	existing, derr := d.docIDFromInput(ctx, key, docInput{Path: path})
	if derr != nil {
		// ONLY "not there" means "make one". Treating a transient dirent read fault
		// as absence would create a SECOND document for a subject that already has
		// one, and the Path tree can name only one of them — a fork no later run
		// would notice, because the subject is gone from the shared document either
		// way.
		if !strings.Contains(derr.Error(), "no such path") {
			return homeTarget{}, derr
		}
		return homeTarget{docID: newDocID()}, nil
	}
	if existing == "" {
		return homeTarget{docID: newDocID()}, nil
	}
	res, qerr := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ?`, existing)
	if qerr != nil {
		return homeTarget{}, qerr
	}
	if len(res.Rows) == 0 {
		return homeTarget{}, fmt.Errorf("%s names document %s, which does not exist", path, existing)
	}
	root := asStr(res.Rows[0][0])
	kres, kerr := d.query(ctx, key, `SELECT coalesce(natural_key, '') FROM chunk_memory_meta WHERE chunk_id = ?`, root)
	if kerr != nil {
		return homeTarget{}, kerr
	}
	// A root that is ALREADY an entity is somebody's identity, and the unique key
	// guarantees it is not this subject's (whose key is still held by the node in the
	// shared document). Two subjects whose names slug alike is not a collision a
	// migration may resolve by picking one.
	if len(kres.Rows) > 0 && asStr(kres.Rows[0][0]) != "" {
		return homeTarget{}, fmt.Errorf("%s is already the entity %q, so homing %q there would "+
			"merge two subjects onto one node", path, asStr(kres.Rows[0][0]), s.naturalKey)
	}
	// Anything parented under the placeholder root would be orphaned by removing it.
	cres, cerr := d.query(ctx, key, `SELECT count(*) FROM chunks WHERE parent_id = ?`, root)
	if cerr != nil {
		return homeTarget{}, cerr
	}
	if len(cres.Rows) > 0 && asInt(cres.Rows[0][0]) > 0 {
		return homeTarget{}, fmt.Errorf("%s already holds content under a root that is not an "+
			"entity, so it is somebody else's document", path)
	}
	return homeTarget{docID: existing, adopt: true, dropRoot: root}, nil
}

// homeOneSubject makes the subject node the root of its own document and files its
// facts beneath it — all in ONE transaction, so a fault leaves the old shape intact
// rather than a fact filed under a root that is no longer there.
func (d *Document) homeOneSubject(ctx context.Context, key sqlmem.ScopeKey, target homeTarget, s homingSubject) error {
	now := time.Now().UnixNano()
	return d.withSqlTxn(ctx, key, func(txnID string) error {
		if target.adopt {
			// The placeholder root goes before the subject takes its place, so the
			// document never has two roots — and its own cascades run, because an
			// empty root still owns a revision row and a layout row.
			if err := d.deletePlaceholderRoot(ctx, txnID, target.dropRoot); err != nil {
				return err
			}
			if err := d.execTxn(ctx, txnID,
				`UPDATE documents SET root_chunk_id = ?, title = ?, type = ?, status = ?, updated_at = ? WHERE id = ?`,
				s.chunkID, s.name, nullIfEmpty(s.typ), nullIfEmpty(s.status), now, target.docID); err != nil {
				return err
			}
		} else if err := d.execTxn(ctx, txnID,
			`INSERT INTO documents (id, title, root_chunk_id, type, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			target.docID, s.name, s.chunkID, nullIfEmpty(s.typ), nullIfEmpty(s.status), now, now); err != nil {
			return err
		}
		// THE SUBJECT ITSELF MOVES. It keeps its chunk id, so its sidecar (the unique
		// natural key included), its edges and its body come with it — there is no
		// window in which two chunks claim the key, and nothing to re-point.
		if err := d.execTxn(ctx, txnID,
			`UPDATE chunks SET document_id = ?, parent_id = NULL, position = 0, updated_at = ? WHERE id = ?`,
			target.docID, now, s.chunkID); err != nil {
			return err
		}
		for i, fid := range s.factIDs {
			if err := d.execTxn(ctx, txnID,
				`UPDATE chunks SET document_id = ?, parent_id = ?, position = ?, updated_at = ? WHERE id = ?`,
				target.docID, s.chunkID, i, now, fid); err != nil {
				return err
			}
			// A fact is a leaf today, but a chunk left behind in the old document while
			// its parent lives in the new one is precisely the cross-document parentage
			// move_chunk refuses — so the subtree travels with it.
			if err := d.moveSubtreeDocument(ctx, txnID, fid, target.docID); err != nil {
				return err
			}
		}
		return nil
	})
}

// moveSubtreeDocument re-stamps document_id on every descendant of a moved chunk,
// leaving parent_id alone — the shape inside the subtree is unchanged, only which
// document owns it.
func (d *Document) moveSubtreeDocument(ctx context.Context, txnID, chunkID, docID string) error {
	frontier := []string{chunkID}
	seen := map[string]bool{chunkID: true}
	for depth := 0; len(frontier) > 0 && depth <= maxChunkDepth; depth++ {
		var next []string
		for _, pid := range frontier {
			res, err := d.queryTxn(ctx, txnID, `SELECT id FROM chunks WHERE parent_id = ?`, pid)
			if err != nil {
				return err
			}
			if res.Truncated {
				return fmt.Errorf("subtree too wide to move safely (a level exceeds the row cap)")
			}
			for _, row := range res.Rows {
				cid := asStr(row[0])
				if cid == "" || seen[cid] {
					continue
				}
				seen[cid] = true
				if err := d.execTxn(ctx, txnID, `UPDATE chunks SET document_id = ? WHERE id = ?`, docID, cid); err != nil {
					return err
				}
				next = append(next, cid)
			}
		}
		frontier = next
	}
	return nil
}

// deletePlaceholderRoot removes the empty root an adopted document came with. Every
// per-chunk cascade delete_chunk performs is repeated, because a revision or layout row
// reachable only through the chunk that is going is an orphan nothing can see.
func (d *Document) deletePlaceholderRoot(ctx context.Context, txnID, chunkID string) error {
	for _, stmt := range []string{
		`DELETE FROM chunk_edges WHERE from_id = ? OR to_id = ?`,
		`DELETE FROM chunk_assets WHERE chunk_id = ?`,
		`DELETE FROM chunk_tags WHERE chunk_id = ?`,
		`DELETE FROM chunk_memory_meta WHERE chunk_id = ?`,
		`DELETE FROM chunk_revisions WHERE chunk_id = ?`,
		`DELETE FROM chunk_layout WHERE chunk_id = ?`,
		`DELETE FROM chunks WHERE id = ?`,
	} {
		args := []any{chunkID}
		if strings.Contains(stmt, "OR to_id") {
			args = append(args, chunkID)
		}
		if err := d.execTxn(ctx, txnID, stmt, args...); err != nil {
			return err
		}
	}
	return nil
}
