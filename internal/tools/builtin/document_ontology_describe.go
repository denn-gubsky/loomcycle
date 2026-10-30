package builtin

// document_ontology_describe.go — Document op=describe_ontology (RFC FP §13 R4).
//
// The parsed tenant ontology, as data, for a code agent that has to act on it (the
// links-building agent reads it once per pass). The alternative was a second parser in
// the agent's JavaScript, and two parsers of one document eventually disagree about it.
//
// IN FORCE, NOT AS WRITTEN. Every section is derived exactly as the gates derive it: a
// draft ontology governs nothing, so it describes the seed classes and nothing else, and
// the relation vocabulary exists only with a confirmed Relations section. An agent acting
// on a declaration the runtime does not enforce would write what the gates then refuse
// or annotate. TestDescribeOntology_MatchesTheGates holds the two to each other.

import (
	"context"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// describedClass is one class in force, with its directives and fields resolved through
// inheritance — what a consumer acts on, not what the class's own chunk declares.
type describedClass struct {
	Name   string `json:"name"`
	Parent string `json:"parent,omitempty"`
	Source string `json:"source"`
	// Kind is "document" for Document and its descendants (they govern documents.type),
	// "chunk" otherwise.
	Kind string `json:"kind"`
	// Fields is the complete field set: inherited first, then the class's own.
	Fields        []memrank.FieldSpec `json:"fields,omitempty"`
	TitleIdentity bool                `json:"title_identity,omitempty"`
	// Directives holds each class directive's effective value: declared, else inherited.
	Directives  map[string]string `json:"directives,omitempty"`
	MemoryScope string            `json:"memory_scope,omitempty"`
	RenamedFrom string            `json:"renamed_from,omitempty"`
	Issues      []string          `json:"issues,omitempty"`
}

// describedGrammar is a grammar plus the uses classes in force make of it.
type describedGrammar struct {
	memrank.OntologyGrammar
	// UsedAs is "anchor" and/or "citation": how classes in force name this grammar. A
	// grammar has no kind of its own.
	UsedAs []string `json:"used_as,omitempty"`
}

// describedInstance is one entry of a confirmed Instances section: a real thing the
// tenant declared, and — once adopted — the registry subject it stands for.
type describedInstance struct {
	Name string `json:"name"`
	// Class is the `@a` class, as the classes in force name it.
	Class string   `json:"class,omitempty"`
	Alt   []string `json:"alt,omitempty"`
	// ChunkID is the instance's own chunk in the ontology document.
	ChunkID string `json:"chunk_id"`
	// RegistryKey and SubjectChunkID name the adopted subject node. Both are absent
	// unless the adoption is one the adoption code itself would trust.
	RegistryKey    string   `json:"registry_key,omitempty"`
	SubjectChunkID string   `json:"subject_chunk_id,omitempty"`
	Issues         []string `json:"issues,omitempty"`
	// declaredClass is Class when a class in force has that name, "" otherwise: only an
	// instance of a declared class is ever adopted.
	declaredClass string
}

// ontologyDescription is describe_ontology's answer.
type ontologyDescription struct {
	Exists     bool   `json:"exists"`
	DocumentID string `json:"document_id,omitempty"`
	// Status is the document's: "draft" or "confirmed" ("" without a document). Sections
	// have no status of their own — a draft governs nothing, whatever it declares.
	Status    string   `json:"status"`
	Confirmed bool     `json:"confirmed"`
	Version   int      `json:"version"`
	Sections  []string `json:"sections"`
	// Partial is true when part of the document could not be read: the description is
	// what could be read, and a pass should not treat an absent entry as removed.
	Partial   bool                       `json:"partial,omitempty"`
	Classes   []describedClass           `json:"classes"`
	Relations []memrank.OntologyRelation `json:"relations"`
	Grammars  []describedGrammar         `json:"grammars"`
	Schemes   []memrank.OntologyScheme   `json:"schemes"`
	Instances []describedInstance        `json:"instances"`
	Issues    []string                   `json:"issues,omitempty"`
}

// describeOntology answers op=describe_ontology for the caller's tenant. The call's
// `scope` does not apply: the ontology is the tenant's.
func (d *Document) describeOntology(ctx context.Context) (tools.Result, error) {
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return docFail("describe_ontology", err), nil
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		return errBusiness("describe_ontology: the tenant ontology could not be read", "Retry the call; a store fault is usually transient."), nil
	}
	acl := d.newDocACL(ctx, key)
	if read.DocumentID != "" && acl != nil {
		// The read verb on the ontology document itself, at the names it is filed under,
		// so a rule that grants exactly /memory/ontology is enough. The seed a tenant
		// without a document gets is compiled in and needs no check.
		if r, err := acl.document(ctx, "describe_ontology", "read", read.DocumentID); err != nil {
			return errFrom("describe_ontology: "+err.Error(), err), nil
		} else if r != nil {
			return *r, nil
		}
	}
	out := describeOntologyRead(read)
	d.describeInstanceSubjects(ctx, key, mscope, acl, read, &out)
	return jsonResult(out)
}

// describeOntologyRead derives the in-force description from one read. Pure, and it never
// mutates the read: the read is shared through the ontology cache.
func describeOntologyRead(read OntologyRead) ontologyDescription {
	out := ontologyDescription{
		Exists:     read.DocumentID != "",
		DocumentID: read.DocumentID,
		Confirmed:  read.Confirmed,
		Version:    read.RootRevision,
		Sections:   append([]string{}, read.Sections...),
		Partial:    read.partial,
		Issues:     append([]string(nil), read.Issues...),
		Relations:  []memrank.OntologyRelation{},
		Grammars:   []describedGrammar{},
		Schemes:    []memrank.OntologyScheme{},
		Instances:  []describedInstance{},
	}
	if out.Exists {
		out.Status = memrank.OntologyDraftStatus
		if read.Confirmed {
			out.Status = "confirmed"
		}
	}
	usedAs := map[string]map[string]bool{}
	for _, c := range memrank.EffectiveClasses(read.Terms, read.Confirmed) {
		dc := describedClass{Name: c.Name, Parent: c.Parent, Source: c.Source, Kind: "chunk",
			TitleIdentity: c.TitleIdentity || c.InheritedTitleIdentity,
			MemoryScope:   memrank.EffectiveMemoryScope(c), RenamedFrom: c.RenamedFrom,
			Issues: append([]string(nil), c.Issues...)}
		if c.IsDocumentClass {
			dc.Kind = "document"
		}
		dc.Fields = append(append([]memrank.FieldSpec{}, c.InheritedSpecs...), c.FieldSpecs...)
		for _, name := range memrank.InheritableDirectives() {
			if v := memrank.EffectiveDirective(c, name); v != "" {
				if dc.Directives == nil {
					dc.Directives = map[string]string{}
				}
				dc.Directives[name] = v
			}
		}
		for use, directive := range map[string]string{"anchor": "anchor_grammar", "citation": "citation_grammar"} {
			for _, g := range memrank.GrammarNames(dc.Directives[directive]) {
				if usedAs[g] == nil {
					usedAs[g] = map[string]bool{}
				}
				usedAs[g][use] = true
			}
		}
		out.Classes = append(out.Classes, dc)
	}
	if !read.Confirmed {
		return out
	}
	if hasSection(read.Sections, memrank.SectionRelations) {
		out.Relations = memrank.EffectiveRelations(read.Relations, true, true)
		sort.Slice(out.Relations, func(i, j int) bool { return out.Relations[i].Name < out.Relations[j].Name })
	}
	for _, g := range read.Grammars {
		dg := describedGrammar{OntologyGrammar: g}
		for use := range usedAs[g.Name] {
			dg.UsedAs = append(dg.UsedAs, use)
		}
		sort.Strings(dg.UsedAs)
		out.Grammars = append(out.Grammars, dg)
	}
	out.Schemes = append(out.Schemes, read.Schemes...)
	if hasSection(read.Sections, memrank.SectionInstances) {
		// Matched as adoption matches them: case-insensitively, against the classes in
		// force, reported under the declared spelling.
		declared := map[string]string{}
		for _, c := range out.Classes {
			declared[strings.ToLower(c.Name)] = c.Name
		}
		for _, inst := range read.Instances {
			di := describedInstance{Name: inst.Name, Class: inst.Class, ChunkID: inst.ChunkID,
				Alt: append([]string(nil), inst.Alt...), Issues: append([]string(nil), inst.Issues...)}
			if name, ok := declared[strings.ToLower(inst.Class)]; ok {
				di.Class, di.declaredClass = name, name
			} else if inst.Class != "" {
				di.Issues = append(di.Issues, "no declared class: @a "+inst.Class+"; it cannot be adopted")
			}
			out.Instances = append(out.Instances, di)
		}
	}
	return out
}

// describeInstanceSubjects names, for each described instance, the registry subject it
// was adopted as — only where adoption itself would trust the recorded key (instanceKey),
// the subject exists, and the caller may read it.
//
// The ontology is operator config and is described to any caller; a subject node is
// tenant DATA, so naming one takes what reading it would: the tenant grant, and read
// on the subject document under the caller's access-control rules. Without either, the
// instance is described without its subject — no id leaks, and the caller could not
// have written a statement at the node anyway.
//
// Cost: one query resolves every recorded key. A key that is not the derived one (an
// instance renamed after adoption) additionally reads that subject's fields, as adoption
// does; derived keys — the common case — read nothing more.
func (d *Document) describeInstanceSubjects(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope,
	acl *docACL, read OntologyRead, out *ontologyDescription) {
	if len(out.Instances) == 0 {
		return
	}
	if _, _, err := d.resolveScope(ctx, "tenant"); err != nil {
		return
	}
	recorded := map[string]memrank.OntologyInstance{} // instance chunk → instance
	var keys []string
	seen := map[string]bool{}
	for _, inst := range read.Instances {
		recorded[inst.ChunkID] = inst
		if inst.RegistryKey != "" && !seen[inst.RegistryKey] {
			seen[inst.RegistryKey] = true
			keys = append(keys, inst.RegistryKey)
		}
	}
	if len(keys) == 0 {
		return
	}
	unreadable := func() {
		out.Partial = true
		out.Issues = append(out.Issues, "the instances' adopted subjects could not be read; read again")
	}
	ph, args := inPlaceholders(keys)
	res, err := d.query(ctx, key,
		`SELECT m.natural_key, m.chunk_id, c.document_id FROM chunk_memory_meta m
		   JOIN chunks c ON c.id = m.chunk_id
		  WHERE m.natural_key IN (`+ph+`)`, args...)
	if err != nil {
		unreadable()
		return
	}
	type subject struct{ chunk, doc string }
	subjects := map[string]subject{}
	for _, row := range res.Rows {
		if len(row) >= 3 {
			subjects[asStr(row[0])] = subject{chunk: asStr(row[1]), doc: asStr(row[2])}
		}
	}
	subjectOf := func(nk string) (string, error) { return subjects[nk].chunk, nil }
	for i := range out.Instances {
		di := &out.Instances[i]
		inst, ok := recorded[di.ChunkID]
		if !ok || di.declaredClass == "" {
			continue
		}
		subjectKey, trusted := d.instanceKey(ctx, key, mscope, inst, di.declaredClass, subjectOf)
		s := subjects[subjectKey]
		if !trusted || s.chunk == "" {
			continue
		}
		if acl != nil {
			ok, err := acl.readable(ctx, s.doc)
			if err != nil {
				unreadable()
				return
			}
			if !ok {
				continue
			}
		}
		di.RegistryKey, di.SubjectChunkID = subjectKey, s.chunk
	}
}
