package builtin

// document_ontology.go — reading the ontology document as a TREE (RFC BZ phase 1).
//
// ONE CHUNK IS ONE ENTITY. Its title names the entity, backticked bullets in its body
// declare fields, everything else in the body is documentation, and its CHILD CHUNKS
// are its subclasses. The document's root chunk is not an entity; its children are the
// root entities.
//
// WHY THIS REPLACES READING THE EXPORTED MARKDOWN, which is not a preference but a
// requirement. export_md renders chunk depth as `strings.Repeat("#", level)`, so after
// flattening these two lines are the same bytes:
//
//	### incident      ← was a child chunk's title (a subclass)
//	### incident      ← was a line inside a body (a comment)
//
// The distinction the rule rests on is destroyed by the export. Reading chunks makes it
// free: parent_id IS the hierarchy.
//
// It also fixes a silent data-loss bug. The markdown parser matched only "## ", so a
// nested "### incident" was recognised as neither a term nor a title and was dropped —
// no error, no warning, nothing in the Settings panel. An operator who organised their
// ontology into a tree, which is the natural move in a document UI, had a document that
// read correctly and an ontology that did not match it.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ontologyChunk is one row of the tree walk.
type ontologyChunk struct {
	id       string
	parentID string
	title    string
	position int
	status   string
}

// OntologyTermsFromTree reads the ontology document at the given path and returns its
// entities with parentage resolved.
//
// Returns (nil, nil) when the document does not exist — provisioning is the caller's
// concern, and an absent document is "no tenant layer", not an error.
//
// DEPTH-FIRST IN POSITION ORDER, so the returned slice reads like the document: a
// parent immediately followed by its subclasses. EffectiveOntology sorts by name for
// rendering, but a caller showing the tree (the Settings panel) wants document order.
func (d *Document) OntologyTermsFromTree(ctx context.Context, scope, path string) (OntologyRead, error) {
	if d.Store == nil || d.SqlMem == nil {
		return OntologyRead{}, fmt.Errorf("ontology: requires SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)")
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return OntologyRead{}, err
	}
	return d.ontologyForKey(ctx, key, mscope, path)
}

// OntologyRead is one read of the ontology document.
//
// A struct rather than a growing return list: the read already reports terms and the
// confirm gate, and DepthCapped is the third thing a caller has to know. Each addition
// as a positional return is one more call site that can silently pass the wrong slot.
type OntologyRead struct {
	Terms     []memrank.OntologyTerm
	Confirmed bool
	// Proposals are the entities present but NOT in force — a curator's suggestions and
	// the operator's rejections. Returned alongside the terms rather than through a
	// second read, so the panel cannot show a proposal list that disagrees with the
	// type list it sits next to.
	Proposals []memrank.OntologyProposal
	// DocumentID and RootChunkID let a caller act on what it just read (accept a
	// proposal, adopt a type as a new root) without resolving the path again.
	DocumentID  string
	RootChunkID string
	// Sections lists the section kinds the document declares, in document order (RFC FO).
	// Empty for a section-less document, which reads exactly as it did before sections.
	Sections []string
	// Grammars are the `grammar:<name>` chunks of the classes section — data the anchor
	// resolver and the linking agent read. Only recognised in a sectioned document, so a
	// flat document's chunk titled like one keeps its old reading as a class.
	Grammars []memrank.OntologyGrammar
	// Relations are the entries of the Relations section, in document order, each with
	// its parent kind resolved (RFC FO P1b). Empty without such a section. The section
	// being present at all is recorded in Sections.
	Relations []memrank.OntologyRelation
	// SectionChunks maps each declared section kind to its chunk, so a proposal can be
	// filed into the right section.
	SectionChunks map[string]string
	// partial is set when any chunk body could not be read during the walk. Such a read
	// is still usable — it is what the walk always returned on a fault — but it must
	// never be CACHED, or one transient fault would strip classes and relations of their
	// declarations until the next edit.
	partial bool
	// Issues are document-level advisories: a duplicate section, an unknown section kind.
	// Per-class advisories live on each term's Issues.
	Issues []string
	// RootRevision is the ontology root chunk's revision — the ontology_version stamped
	// on facts and statements written under this vocabulary (RFC FO §3.6).
	RootRevision int
	// DepthCapped is true when the document nests deeper than OntologyMaxDepth, so
	// some types were flattened onto the cap rather than kept at their written depth.
	//
	// REPORTED because the flattening was otherwise invisible: an operator whose
	// level-5 type quietly became a level-4 sibling had no way to know their document
	// and their ontology disagreed — the exact failure this file was written to end.
	DepthCapped bool
}

// ontologyForKey is the reader proper: terms plus the document's confirmed status.
//
// Split out from OntologyTermsFromTree so the retrieval-side expansion can reuse it
// with a tenant key it built itself. One reader, two callers — the alternative was a
// second tree walk, and two walks of the same tree eventually disagree about it.
func (d *Document) ontologyForKey(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, path string) (OntologyRead, error) {
	// DELIBERATELY NO ensureSchema. Reading must not provision.
	//
	// This is called from a query path, so ensuring here would mean an agent running
	// `query_chunks type=x` CREATES its tenant's SQL-Memory schema — and on the
	// Postgres tier a per-scope LOGIN role — for a tenant that has no tenant documents
	// at all. A read that builds infrastructure can also FAIL to build it (the role
	// needs CREATEROLE), turning a working query into an error over a taxonomy the
	// tenant never had.
	//
	// Callers that legitimately provision (the server's ontology endpoints) do it
	// themselves before they get here. With no schema the lookup below simply errors
	// and the caller reads it as "no tenant layer", which is the truth.
	docID, err := d.docIDFromInput(ctx, key, docInput{Path: path})
	if err != nil {
		return OntologyRead{}, nil // no such document — no tenant layer
	}
	var rootID, status string
	if res, qerr := d.query(ctx, key,
		`SELECT root_chunk_id, coalesce(status, '') FROM documents WHERE id = ? LIMIT 1`, docID); qerr != nil {
		return OntologyRead{}, qerr
	} else if len(res.Rows) == 0 || len(res.Rows[0]) < 2 {
		return OntologyRead{}, nil
	} else {
		rootID, status = asStr(res.Rows[0][0]), asStr(res.Rows[0][1])
	}
	confirmed := strings.EqualFold(strings.TrimSpace(status), memrank.OntologyConfirmedStatus)

	res, err := d.query(ctx, key,
		`SELECT id, coalesce(parent_id, ''), coalesce(title, ''), position, coalesce(status, ''), revision
		   FROM chunks WHERE document_id = ? ORDER BY position`, docID)
	if err != nil {
		return OntologyRead{Confirmed: confirmed}, err
	}
	kids := map[string][]ontologyChunk{}
	rootRevision := 0
	for _, row := range res.Rows {
		if len(row) < 6 {
			continue
		}
		c := ontologyChunk{
			id: asStr(row[0]), parentID: asStr(row[1]),
			title: asStr(row[2]), position: asInt(row[3]), status: asStr(row[4]),
		}
		if c.id == rootID {
			rootRevision = asInt(row[5])
		}
		kids[c.parentID] = append(kids[c.parentID], c)
	}
	for k := range kids {
		sort.SliceStable(kids[k], func(i, j int) bool {
			return kids[k][i].position < kids[k][j].position
		})
	}

	// Bodies are read once and shared: the section pre-pass and the class walk both need
	// a root child's body, and each read is a Memory round-trip.
	bodies := map[string]string{}
	partial := false
	bodyOf := func(id string) string {
		if b, ok := bodies[id]; ok {
			return b
		}
		got, err := d.readBody(ctx, mscope, key, id)
		if err != nil {
			partial = true
		}
		bodies[id] = got.Body
		return got.Body
	}
	// entityName resolves an entry's name: the chunk's title, or — when the title is
	// empty — the first heading in its body. The title is authoritative because import_md
	// lifts a heading into it; the body fallback covers a chunk written through
	// update_chunk with a heading and no title, which would otherwise be nameless and
	// dropped. Read through bodyOf, so an untitled chunk's failed read marks the read
	// partial too.
	entityName := func(c ontologyChunk) string {
		if n := memrank.TrimOntologyName(c.title); n != "" {
			return n
		}
		return memrank.FirstHeadingName(bodyOf(c.id))
	}

	// SECTION PRE-PASS (RFC FO §3.1). A root child whose first directive is
	// `@section <kind>` is a section; anything else is a class, as before sections
	// existed. At most one section of each kind: a second is reported and read the way
	// it read before sections — as a class — so declaring sections can never take out of
	// force a type that was in force.
	sectionOf := map[string]string{}     // chunk id → kind, for the sections that count
	sectionChunks := map[string]string{} // kind → chunk id
	var sections, issues []string
	seenKind := map[string]bool{}
	for _, c := range kids[rootID] {
		body := bodyOf(c.id)
		if iss := memrank.SectionIssue(body); iss != "" {
			issues = append(issues, strconv.Quote(memrank.TrimOntologyName(c.title))+": "+iss)
		}
		kind := memrank.SectionKind(body)
		if kind == "" {
			continue
		}
		if seenKind[kind] {
			issues = append(issues, "a second @section "+kind+" ("+strconv.Quote(memrank.TrimOntologyName(c.title))+
				") is ignored — the first one wins — and read as a class")
			continue
		}
		seenKind[kind] = true
		sectionOf[c.id] = kind
		sectionChunks[kind] = c.id
		sections = append(sections, kind)
	}
	sectioned := len(sections) > 0

	var out []memrank.OntologyTerm
	var proposals []memrank.OntologyProposal
	var grammars []memrank.OntologyGrammar
	depthCapped := false
	// The walk starts at the ROOT CHUNK'S CHILDREN: the root is the document's title
	// ("Tenant Ontology"), not an entity. Including it would invent a type nobody
	// declared and make every real entity its subclass.
	//
	// docClass is true inside the classes section beneath a class named Document: those
	// are document classes, which govern documents.type and never reach the prompt.
	var walk func(chunks []ontologyChunk, parentChunkID, parentName string, depth int, docClass bool)
	walk = func(chunks []ontologyChunk, parentChunkID, parentName string, depth int, docClass bool) {
		for _, c := range chunks {
			// A grammar is data under a class, not a subclass of it.
			if sectioned {
				if gname := memrank.GrammarName(c.title); gname != "" {
					grammars = append(grammars, memrank.ParseGrammarBody(gname, parentName, bodyOf(c.id)))
					continue
				}
			}
			name := entityName(c)
			// INERT: a proposal (or a rejected one) is in the document and not in force.
			// Recorded so the operator can act on it, then skipped.
			if memrank.IsInertEntityStatus(c.status) {
				if name != "" {
					body := bodyOf(c.id)
					proposals = append(proposals, memrank.OntologyProposal{
						ChunkID: c.id, Name: name, Parent: parentName,
						Fields: memrank.ParseOntologyFields(body),
						Status: strings.ToLower(strings.TrimSpace(c.status)),
						Body:   body,
					})
				}
				// Its children keep walking against the nearest IN-FORCE ancestor, so
				// rejecting a parent never silently switches off a live type beneath it.
				// Turning types off is what this reader exists to prevent.
				walk(kids[c.id], c.id, parentName, depth, docClass)
				continue
			}
			if name == "" {
				// No title and no leading heading: nothing names this entity. Its
				// children are still walked, attached to the nearest NAMED ancestor, so
				// an unnamed organisational chunk does not orphan a real subclass.
				walk(kids[c.id], c.id, parentName, depth, docClass)
				continue
			}
			// The scope directive is read on THIS path too, not only from the
			// whole-document Markdown. This is the reader the live ontology actually
			// goes through — one chunk per type — so a declaration parsed in only one
			// of the two places would work in the template and do nothing in
			// production.
			decl := memrank.ParseClassBody(bodyOf(c.id))
			isDoc := docClass || (parentName == "" && sectionOf[parentChunkID] == memrank.SectionClasses &&
				strings.EqualFold(name, memrank.DocumentClassRoot))
			out = append(out, memrank.OntologyTerm{
				Name:              name,
				Fields:            decl.Fields,
				Source:            "tenant",
				Parent:            parentName,
				MemoryScope:       decl.MemoryScope,
				MemoryScopeIssue:  decl.MemoryScopeIssue,
				FieldSpecs:        decl.FieldSpecs,
				TitleIdentity:     decl.TitleIdentity,
				IsDocumentClass:   isDoc,
				IdentifierPattern: decl.IdentifierPattern,
				TitlePattern:      decl.TitlePattern,
				AnchorGrammar:     decl.AnchorGrammar,
				CitationGrammar:   decl.CitationGrammar,
				Exclude:           decl.Exclude,
				RenamedFrom:       decl.RenamedFrom,
				Issues:            decl.Issues,
			})
			// AT the cap, this node's children are reported with this node's OWN
			// parent — so a would-be level-5 type lands at level 4 as a sibling, and
			// every deeper descendant collapses into that same flat set. The chain is
			// bounded at ontologyMaxDepth levels and nothing is discarded.
			childParent := name
			if depth >= memrank.OntologyMaxDepth {
				childParent = parentName
				if len(kids[c.id]) > 0 {
					depthCapped = true
				}
			}
			walk(kids[c.id], c.id, childParent, depth+1, isDoc)
		}
	}
	// walkRelations reads the Relations section: one chunk is one relation kind, a
	// child chunk a sub-kind. The same fail-open rules as classes — an inert entry is a
	// proposal and its children attach to the nearest in-force kind, an unnamed chunk
	// is skipped with its children kept, and the depth cap flattens rather than drops.
	var relations []memrank.OntologyRelation
	var walkRelations func(chunks []ontologyChunk, parentName string, depth int)
	walkRelations = func(chunks []ontologyChunk, parentName string, depth int) {
		for _, c := range chunks {
			name := entityName(c)
			if memrank.IsInertEntityStatus(c.status) {
				if name != "" {
					body := bodyOf(c.id)
					proposals = append(proposals, memrank.OntologyProposal{
						ChunkID: c.id, Name: name, Parent: parentName,
						Status: strings.ToLower(strings.TrimSpace(c.status)),
						Body:   body, Section: memrank.SectionRelations,
					})
				}
				walkRelations(kids[c.id], parentName, depth)
				continue
			}
			if name == "" {
				walkRelations(kids[c.id], parentName, depth)
				continue
			}
			rel := memrank.ParseRelationBody(name, bodyOf(c.id))
			rel.Parent, rel.ChunkID = parentName, c.id
			relations = append(relations, rel)
			childParent := name
			if depth >= memrank.OntologyMaxDepth {
				childParent = parentName
				if len(kids[c.id]) > 0 {
					depthCapped = true
				}
			}
			walkRelations(kids[c.id], childParent, depth+1)
		}
	}
	for _, c := range kids[rootID] {
		switch sectionOf[c.id] {
		case memrank.SectionClasses:
			// Depth counts from the SECTION (RFC FO §3.1): its children are root classes.
			walk(kids[c.id], c.id, "", 1, false)
		case memrank.SectionRelations:
			// Depth counts from the section, as for classes; a child entry is a sub-kind.
			walkRelations(kids[c.id], "", 1)
		case memrank.SectionSchemes, memrank.SectionInstances:
			// Recognised and kept out of the class list; their entries are read by the
			// phase that gives them meaning.
		default:
			// No directive: a class, exactly as before sections existed.
			walk([]ontologyChunk{c}, rootID, "", 1, false)
		}
	}
	return OntologyRead{
		Terms: out, Confirmed: confirmed, DepthCapped: depthCapped,
		Proposals: proposals, DocumentID: docID, RootChunkID: rootID,
		Sections: sections, Grammars: grammars, Issues: issues, RootRevision: rootRevision,
		Relations: relations, SectionChunks: sectionChunks, partial: partial,
	}, nil
}

// expandTypeFilter turns a type filter into the type plus every type that is a
// SUBCLASS of it in the tenant's confirmed ontology.
//
// THIS IS THE POINT OF THE HIERARCHY. Without it a taxonomy is decorative: an
// operator gains a tidy document and no new answers, because a search for `event`
// still misses every `incident` they carefully classified. Storage keeps the CONCRETE
// type only and expansion happens here, at query time — materialising ancestors into
// each stored row would mean re-parenting a type silently invalidates history, since
// facts recorded before the move would still claim the old ancestor and need a
// backfill nobody will run. Expanding at query time makes a correction take effect
// immediately and retroactively, which is what an operator fixing their taxonomy
// expects.
//
// Returns nil for an empty filter, and a single-element slice when there is nothing to
// expand — the caller then emits the same `type = ?` SQL it always did.
//
// ONLY WHEN CONFIRMED. A draft ontology is inert everywhere else, and retrieval must
// not be the one surface where an unconfirmed edit quietly changes answers.
func (d *Document) expandTypeFilter(ctx context.Context, typ string) []string {
	typ = strings.TrimSpace(typ)
	if typ == "" || d.Store == nil || d.SqlMem == nil {
		return nil
	}
	terms, confirmed := d.tenantOntologyForRun(ctx)
	if !confirmed || len(terms) == 0 {
		return []string{typ}
	}
	kids := make(map[string][]string, len(terms))
	for _, t := range terms {
		if t.Parent != "" {
			kids[t.Parent] = append(kids[t.Parent], t.Name)
		}
	}
	out := []string{typ}
	seen := map[string]bool{typ: true}
	// Breadth-first with a visited set, bounded by the depth cap. The chunk tree
	// cannot cycle, but this runs inside a query path and a cycle here would hang the
	// request rather than return a wrong row.
	frontier := []string{typ}
	for depth := 0; depth < memrank.OntologyMaxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, name := range frontier {
			for _, kid := range kids[name] {
				if seen[kid] {
					continue
				}
				seen[kid] = true
				out = append(out, kid)
				next = append(next, kid)
			}
		}
		frontier = next
	}
	sort.Strings(out[1:]) // stable output; the requested type stays first
	return out
}

// tenantOntologyForRun reads the run's tenant ontology for retrieval purposes.
//
// The tenant key is built HERE rather than through resolveScope, deliberately
// skipping the `scope=tenant` grant check, and that is not a hole:
//
//   - The tenant comes from the run's server-stamped identity. Nothing the model says
//     reaches it.
//   - The ontology is operator-authored CONFIG, not tenant data. It already reaches
//     the agent — the server renders it into the system prompt with grants it stamps
//     itself, for exactly this reason.
//   - Nothing from the tenant scope is returned. The only effect is which of the
//     agent's OWN rows, in the scope it already queried, match its filter.
//
// Requiring the grant instead would mean subtype expansion worked only for agents
// holding tenant-write authority — so the operators most careful about scoping would
// be the ones whose taxonomy silently did nothing.
//
// Not cached: an operator who fixes their taxonomy expects the next query to reflect
// it, and the read is two point lookups on a filter that is not on any hot path.
func (d *Document) tenantOntologyForRun(ctx context.Context) ([]memrank.OntologyTerm, bool) {
	terms, confirmed, _ := d.tenantOntologyState(ctx)
	return terms, confirmed
}

// tenantOntologyState additionally reports whether the tenant HAS an ontology document.
//
// That third value matters to the write gate and not to retrieval. The seed is compiled
// in, so the effective set is never empty and "no terms" cannot distinguish a tenant that
// never opened the ontology from one whose layer is empty. A gate reading only the terms
// would enforce a vocabulary nobody chose to curate, changing behaviour for every
// deployment that has not opted in.
func (d *Document) tenantOntologyState(ctx context.Context) (terms []memrank.OntologyTerm, confirmed, exists bool) {
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		// Best-effort, exactly as the prompt-side render is: a store fault must not
		// turn a retrieval into an error. The unexpanded filter still answers the
		// question that was asked, just narrowly.
		return nil, false, false
	}
	return memrank.ResolveInheritance(read.Terms), read.Confirmed, read.DocumentID != ""
}

// tenantOntologyRead is the whole read of the run's tenant ontology — terms plus the
// section-level results (grammars, root revision) the class-aware write paths need.
// ok is false on a store fault; an absent document is ok with an empty read.
//
// CACHED, and never stale (RFC FO decision 10d.4). The read walks every chunk of the
// ontology and reads each body, and since P1b it runs on every edge write through the
// relation gate — an import of a thousand edges would otherwise re-read the ontology a
// thousand times. The parse is kept on the shared sqlmem.Manager and reused only while
// ontologyFingerprint, one query on every call, is unchanged: any edit to any ontology
// chunk, on any replica, changes it. The returned read is shared — callers must not
// mutate its slices.
func (d *Document) tenantOntologyRead(ctx context.Context) (OntologyRead, bool) {
	key := sqlmem.ScopeKey{
		Tenant:  sqlScopeTenant(ctx),
		Scope:   "tenant",
		ScopeID: sqlScopeTenant(ctx),
	}
	docID, err := d.docIDFromInput(ctx, key, docInput{Path: memrank.OntologyPath})
	if err != nil {
		return OntologyRead{}, true // no such document — no tenant layer, as ontologyForKey reports it
	}
	fp, err := d.ontologyFingerprint(ctx, key, docID)
	if err != nil {
		return OntologyRead{}, false
	}
	var uncached OntologyRead
	v, err := d.SqlMem.Memo(key, "ontology", fp, func() (any, error) {
		read, rerr := d.ontologyForKey(ctx, key, store.MemoryScopeTenant, memrank.OntologyPath)
		if rerr != nil {
			return nil, rerr
		}
		if read.partial {
			// Hand it to this caller, store nothing: Memo does not keep a failed build.
			uncached = read
			return nil, errPartialOntologyRead
		}
		return read, nil
	})
	if errors.Is(err, errPartialOntologyRead) {
		return uncached, true
	}
	if err != nil {
		return OntologyRead{}, false
	}
	read, _ := v.(OntologyRead)
	return read, true
}

// errPartialOntologyRead marks a build that must not be cached (a chunk body could not
// be read).
var errPartialOntologyRead = errors.New("ontology read incomplete")

// ontologyFingerprint changes whenever anything the ontology read depends on changes:
// which document is named /memory/ontology, its status (the confirm gate), and its
// chunks — a count catches an add or delete, max(updated_at) a move, and sum(revision)
// every body, title, type or status edit, since update_chunk bumps the revision on each.
func (d *Document) ontologyFingerprint(ctx context.Context, key sqlmem.ScopeKey, docID string) (string, error) {
	res, err := d.query(ctx, key,
		`SELECT coalesce(doc.status, ''), count(c.id), coalesce(max(c.updated_at), 0), coalesce(sum(c.revision), 0)
		   FROM documents doc LEFT JOIN chunks c ON c.document_id = doc.id
		  WHERE doc.id = ? GROUP BY doc.status`, docID)
	if err != nil {
		return "", err
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) < 4 {
		return docID + "|absent", nil
	}
	r := res.Rows[0]
	return fmt.Sprintf("%s|%s|%d|%d|%d", docID, asStr(r[0]), asInt(r[1]), asInt64Val(r[2]), asInt64Val(r[3])), nil
}

// asInt64Val reads a numeric cell as int64 (0 for NULL or non-numeric).
func asInt64Val(v any) int64 {
	n, _ := asInt64(v)
	return n
}

// typeFilterSQL renders an expanded type filter as a WHERE fragment plus its args.
//
// Emits `= ?` for a single type so the common, hierarchy-free case produces byte-identical
// SQL to what it did before subclasses existed — no new query plan for a deployment that
// never nests anything.
func typeFilterSQL(column string, types []string) (string, []any) {
	if len(types) == 0 {
		return "", nil
	}
	if len(types) == 1 {
		return column + " = ?", []any{types[0]}
	}
	ph, args := inPlaceholders(types)
	return column + " IN (" + ph + ")", args
}
