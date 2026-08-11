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
func (d *Document) OntologyTermsFromTree(ctx context.Context, scope, path string) ([]memrank.OntologyTerm, error) {
	if d.Store == nil || d.SqlMem == nil {
		return nil, fmt.Errorf("ontology: requires SQL Memory (set LOOMCYCLE_SQLMEM_ENABLED=1)")
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		return nil, err
	}
	docID, err := d.docIDFromInput(ctx, key, docInput{Path: path})
	if err != nil {
		return nil, nil // no such document — no tenant layer
	}
	var rootID string
	if res, qerr := d.query(ctx, key,
		`SELECT root_chunk_id FROM documents WHERE id = ? LIMIT 1`, docID); qerr != nil {
		return nil, qerr
	} else if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return nil, nil
	} else {
		rootID = asStr(res.Rows[0][0])
	}

	res, err := d.query(ctx, key,
		`SELECT id, coalesce(parent_id, ''), coalesce(title, ''), position
		   FROM chunks WHERE document_id = ? ORDER BY position`, docID)
	if err != nil {
		return nil, err
	}
	kids := map[string][]ontologyChunk{}
	for _, row := range res.Rows {
		if len(row) < 4 {
			continue
		}
		c := ontologyChunk{
			id: asStr(row[0]), parentID: asStr(row[1]),
			title: asStr(row[2]), position: asInt(row[3]),
		}
		kids[c.parentID] = append(kids[c.parentID], c)
	}
	for k := range kids {
		sort.SliceStable(kids[k], func(i, j int) bool {
			return kids[k][i].position < kids[k][j].position
		})
	}

	var out []memrank.OntologyTerm
	// The walk starts at the ROOT CHUNK'S CHILDREN: the root is the document's title
	// ("Tenant Ontology"), not an entity. Including it would invent a type nobody
	// declared and make every real entity its subclass.
	var walk func(parentChunkID, parentName string, depth int)
	walk = func(parentChunkID, parentName string, depth int) {
		for _, c := range kids[parentChunkID] {
			name := d.ontologyEntityName(ctx, mscope, key, c)
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
			}
			walk(c.id, childParent, depth+1)
		}
	}
	walk(rootID, "", 1)
	return out, nil
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
