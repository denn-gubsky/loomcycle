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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
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
