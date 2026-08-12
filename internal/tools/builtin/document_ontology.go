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
	"fmt"
	"sort"
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
		`SELECT id, coalesce(parent_id, ''), coalesce(title, ''), position, coalesce(status, '')
		   FROM chunks WHERE document_id = ? ORDER BY position`, docID)
	if err != nil {
		return OntologyRead{Confirmed: confirmed}, err
	}
	kids := map[string][]ontologyChunk{}
	for _, row := range res.Rows {
		if len(row) < 5 {
			continue
		}
		c := ontologyChunk{
			id: asStr(row[0]), parentID: asStr(row[1]),
			title: asStr(row[2]), position: asInt(row[3]), status: asStr(row[4]),
		}
		kids[c.parentID] = append(kids[c.parentID], c)
	}
	for k := range kids {
		sort.SliceStable(kids[k], func(i, j int) bool {
			return kids[k][i].position < kids[k][j].position
		})
	}

	var out []memrank.OntologyTerm
	var proposals []memrank.OntologyProposal
	depthCapped := false
	// The walk starts at the ROOT CHUNK'S CHILDREN: the root is the document's title
	// ("Tenant Ontology"), not an entity. Including it would invent a type nobody
	// declared and make every real entity its subclass.
	var walk func(parentChunkID, parentName string, depth int)
	walk = func(parentChunkID, parentName string, depth int) {
		for _, c := range kids[parentChunkID] {
			name := d.ontologyEntityName(ctx, mscope, key, c)
			// INERT: a proposal (or a rejected one) is in the document and not in force.
			// Recorded so the operator can act on it, then skipped.
			if memrank.IsInertEntityStatus(c.status) {
				if name != "" {
					body, _ := d.readBody(ctx, mscope, key.ScopeID, c.id)
					proposals = append(proposals, memrank.OntologyProposal{
						ChunkID: c.id, Name: name, Parent: parentName,
						Fields: memrank.ParseOntologyFields(body.Body),
						Status: strings.ToLower(strings.TrimSpace(c.status)),
						Body:   body.Body,
					})
				}
				// Its children keep walking against the nearest IN-FORCE ancestor, so
				// rejecting a parent never silently switches off a live type beneath it.
				// Turning types off is what this reader exists to prevent.
				walk(c.id, parentName, depth)
				continue
			}
			if name == "" {
				// No title and no leading heading: nothing names this entity. Its
				// children are still walked, attached to the nearest NAMED ancestor, so
				// an unnamed organisational chunk does not orphan a real subclass.
				walk(c.id, parentName, depth)
				continue
			}
			body, _ := d.readBody(ctx, mscope, key.ScopeID, c.id)
			out = append(out, memrank.OntologyTerm{
				Name:   name,
				Fields: memrank.ParseOntologyFields(body.Body),
				Source: "tenant",
				Parent: parentName,
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
			walk(c.id, childParent, depth+1)
		}
	}
	walk(rootID, "", 1)
	return OntologyRead{
		Terms: out, Confirmed: confirmed, DepthCapped: depthCapped,
		Proposals: proposals, DocumentID: docID, RootChunkID: rootID,
	}, nil
}

// ontologyEntityName resolves an entity's name: the chunk's title, or — when the title
// is empty — the first `## ` heading in its body.
//
// The title is authoritative because import_md already lifts a heading into it, so a
// document authored the normal way has the name exactly there. The body fallback covers
// a chunk written directly through update_chunk, where the operator typed a heading and
// no title was ever set; without it their entity would be nameless and dropped, which
// is the failure mode this whole change exists to remove.
func (d *Document) ontologyEntityName(ctx context.Context, mscope store.MemoryScope, key sqlmem.ScopeKey, c ontologyChunk) string {
	if n := memrank.TrimOntologyName(c.title); n != "" {
		return n
	}
	body, err := d.readBody(ctx, mscope, key.ScopeID, c.id)
	if err != nil {
		return ""
	}
	return memrank.FirstHeadingName(body.Body)
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
	key := sqlmem.ScopeKey{
		Tenant:  sqlScopeTenant(ctx),
		Scope:   "tenant",
		ScopeID: sqlScopeTenant(ctx),
	}
	read, err := d.ontologyForKey(ctx, key, store.MemoryScopeTenant, memrank.OntologyPath)
	if err != nil {
		// Best-effort, exactly as the prompt-side render is: a store fault must not
		// turn a retrieval into an error. The unexpanded filter still answers the
		// question that was asked, just narrowly.
		return nil, false
	}
	return memrank.ResolveInheritance(read.Terms), read.Confirmed
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
