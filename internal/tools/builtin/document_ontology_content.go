package builtin

// The ontology's content version (RFC GQ P3): one value that changes when, and only
// when, what the ontology in force declares changes.
//
// The root chunk's revision (ontology_version) cannot say that: it moves on a confirm
// and on an edit of the title, and stays put when a class three levels down is
// rewritten. The plan fingerprint cannot either: it moves on every proposal filed.
//
// What is hashed is each in-force entry's parsed declaration — the same value
// entrySignature compares, with the meaning added back — so an edit the plan calls a
// directive change always moves the version. Left out on purpose: proposals and
// rejections, the Review section (owner ruling, 2026-11-25), section headers, the
// root, anything under a section this build does not know, advisories, and sibling
// position (a concept's rank in an @ordered scheme is part of its declaration, so it
// counts).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// ontologyContentVersionLen is how many hex characters of the hash are shown and
// stored: 64 bits, far past any collision among one tenant's releases.
const ontologyContentVersionLen = 16

// planKindGrammar is a grammar chunk of the Classes section. Not a plan entry (a plan
// cannot name one), but data the classes in force are read with.
const planKindGrammar = "grammar"

// ontologyContentItem is one hashed entry. Entry is the parsed declaration; its JSON
// carries the name, the entry status, the meaning and every directive.
type ontologyContentItem struct {
	ChunkID string `json:"chunk_id"`
	Kind    string `json:"kind"`
	// Parent is the chunk of the entry the reader put this one under: a class's
	// superclass, a relation's parent kind, a concept's broader concept or its scheme,
	// an instance's parent instance, a grammar's class. Absent at the top of a section.
	Parent string `json:"parent,omitempty"`
	Entry  any    `json:"entry"`
}

// The declarations below are an entry as parsed, without its advisories. entrySignature
// builds on the same functions, so the two cannot drift: whatever makes a signature
// differ makes the hashed item differ (but for an instance's registry key, below).

func classDeclaration(t memrank.OntologyTerm) memrank.OntologyTerm {
	t.Issues, t.MemoryScopeIssue = nil, ""
	return t
}

func relationDeclaration(rel memrank.OntologyRelation) memrank.OntologyRelation {
	rel.Issues, rel.NameIssue = nil, ""
	return rel
}

// schemeDeclaration is the scheme's own entry: each concept is an entry of its own.
func schemeDeclaration(s memrank.OntologyScheme) memrank.OntologyScheme {
	s.Issues, s.Concepts = nil, nil
	return s
}

func conceptDeclaration(c memrank.OntologyConcept) memrank.OntologyConcept {
	c.Issues = nil
	return c
}

func instanceDeclaration(in memrank.OntologyInstance) memrank.OntologyInstance {
	in.Issues = nil
	return in
}

// ontologyContentVersion hashes the in-force entries of a read. treeParent maps each
// chunk of the document to its parent chunk, as read (a plan's moves applied).
//
// Pure: the same entries give the same value on every store tier and every replica.
func ontologyContentVersion(r OntologyRead, treeParent map[string]string) string {
	return ontologyContentHash(ontologyContentItems(r, treeParent))
}

// ontologyContentHash is the shown form of the items' hash: sha256 over their JSON,
// which is canonical because the items are sorted and every map the encoder meets is
// written in key order. "" when they cannot be rendered, which no parsed read causes.
func ontologyContentHash(items []ontologyContentItem) string {
	b, err := json.Marshal(items)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:ontologyContentVersionLen]
}

// ontologyContentItems lists what the content version covers, sorted by chunk id so
// document order (sibling position) does not count.
func ontologyContentItems(r OntologyRead, treeParent map[string]string) []ontologyContentItem {
	// An entry's parent is known to the reader by NAME. Its chunk is the nearest chunk
	// above the entry that is an in-force entry of that name and kind: the reader only
	// ever attaches an entry to one of its own ancestors, so this is the same entry even
	// when two entries share a name. (Past the depth cap an entry is flattened onto an
	// ancestor's parent; were the skipped ancestor to carry that parent's name too, the
	// nearer one is taken. Stable either way.)
	nearest := func(id, name string, names map[string]string) string {
		if name == "" {
			return ""
		}
		at := id
		for range treeParent { // the tree cannot cycle; bounded all the same
			up, ok := treeParent[at]
			if !ok || up == "" {
				return ""
			}
			if n, isEntry := names[up]; isEntry && n == name {
				return up
			}
			at = up
		}
		return ""
	}

	items := []ontologyContentItem{}
	seen := map[string]bool{}
	add := func(id, kind, parent string, entry any) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		items = append(items, ontologyContentItem{ChunkID: id, Kind: kind, Parent: parent, Entry: entry})
	}

	classNames := make(map[string]string, len(r.Terms))
	classID := func(t memrank.OntologyTerm) string {
		if t.ChunkID != "" {
			return t.ChunkID
		}
		return r.classChunks[strings.ToLower(t.Name)] // a term built elsewhere, as planEntries reads it
	}
	for _, t := range r.Terms {
		classNames[classID(t)] = t.Name
	}
	for _, t := range r.Terms {
		id := classID(t)
		add(id, planKindClass, nearest(id, t.Parent, classNames), classDeclaration(t))
	}
	for _, g := range r.Grammars {
		// Its advisory is dropped like any other; a grammar that cannot be used still
		// reads differently from one that can, by its rules.
		g.Issue = ""
		add(g.ChunkID, planKindGrammar, nearest(g.ChunkID, g.Class, classNames), g)
	}

	relNames := make(map[string]string, len(r.Relations))
	for _, rel := range r.Relations {
		relNames[rel.ChunkID] = rel.Name
	}
	for _, rel := range r.Relations {
		add(rel.ChunkID, planKindRelation, nearest(rel.ChunkID, rel.Parent, relNames), relationDeclaration(rel))
	}

	for _, s := range r.Schemes {
		add(s.ChunkID, planKindScheme, "", schemeDeclaration(s))
		conceptNames := make(map[string]string, len(s.Concepts))
		for _, c := range s.Concepts {
			conceptNames[c.ChunkID] = c.Name
		}
		for _, c := range s.Concepts {
			parent := s.ChunkID // a top concept sits under its scheme
			if c.Parent != "" {
				parent = nearest(c.ChunkID, c.Parent, conceptNames)
			}
			add(c.ChunkID, planKindConcept, parent, conceptDeclaration(c))
		}
	}

	instNames := make(map[string]string, len(r.Instances))
	for _, in := range r.Instances {
		instNames[in.ChunkID] = in.Name
	}
	for _, in := range r.Instances {
		d := instanceDeclaration(in)
		// The key an instance was adopted under is written by the runtime at adoption,
		// not declared by anyone: counting it would move the version on the first
		// confirm of an ontology nobody edited.
		d.RegistryKey = ""
		add(in.ChunkID, planKindInstance, nearest(in.ChunkID, in.Parent, instNames), d)
	}

	sort.Slice(items, func(i, j int) bool { return items[i].ChunkID < items[j].ChunkID })
	return items
}

// ontologyContentTables are the tables whose rows record the content version they were
// written under, in a nullable ontology_content column beside ontology_version (RFC GQ
// P3.2). measure_points has no ontology_version (the root revision says nothing about
// what a point was computed from), but the content version does.
var ontologyContentTables = []string{"chunk_memory_meta", "chunk_edges", "chunk_annotations", "measure_points"}

// migrateOntologyContent adds ontology_content to each of those tables. ALTER on every
// scope, new and old alike, and not declared in any CREATE TABLE, so a scope has one
// shape whichever release created it. Existing rows stay NULL and read as "unknown":
// the version a row was written under cannot be reconstructed, so nothing is backfilled.
// Probe-then-ALTER like its siblings: the fast case is one 0-row SELECT per table.
func (d *Document) migrateOntologyContent(ctx context.Context, key sqlmem.ScopeKey) error {
	for _, table := range ontologyContentTables {
		if _, err := d.query(ctx, key, `SELECT ontology_content FROM `+table+` WHERE 1=0`); err == nil {
			continue
		}
		if err := d.exec(ctx, key, `ALTER TABLE `+table+` ADD COLUMN ontology_content TEXT`); err != nil {
			return fmt.Errorf("add %s.ontology_content: %w", table, err)
		}
	}
	return nil
}

// ontologyContentArg is the content version of the run's tenant ontology as a bind
// argument — what a fact, statement, annotation or measure point is stamped with. NULL
// when there is no ontology document, when it could not be read, or when the read was
// partial (which carries no version): a row is never stamped with a value no complete
// read of the ontology has.
//
// Read the same way as ontologyVersion — the run's server-stamped tenant, no grant
// check, failing open — and for the same reasons: it is provenance, one opaque value
// leaves the tenant scope, and a write must not fail because the ontology could not be
// read. The read is the cached one (tenantOntologyRead), so a write pays the
// fingerprint query, not a walk.
//
// Call it BEFORE opening a transaction: the read is on the tenant scope's database, and
// a SQLite scope has one connection.
func (d *Document) ontologyContentArg(ctx context.Context) any {
	if d.Store == nil || d.SqlMem == nil {
		return nil
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || read.ContentVersion == "" {
		return nil
	}
	return read.ContentVersion
}

// carryOntologyContent gives a statement's copy the content version its source was
// written under, inside the copy's transaction — as the copy carries ontology_version.
// A copy is the same statement re-pointed (a reimport's revision, a merge's re-point),
// not a new write, so it is not re-stamped. Read from the row rather than through
// statementRow so the many statement reads (the plan's no-provisioning ones included)
// do not each need the column.
func (d *Document) carryOntologyContent(ctx context.Context, txnID, sourceID, copyID string) error {
	return d.execTxn(ctx, txnID,
		`UPDATE chunk_edges SET ontology_content = (SELECT src.ontology_content FROM chunk_edges src WHERE src.id = ?) WHERE id = ?`,
		sourceID, copyID)
}
