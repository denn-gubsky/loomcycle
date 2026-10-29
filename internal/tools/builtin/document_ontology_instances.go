package builtin

// document_ontology_instances.go — adopting the Instances section into the tenant entity
// registry (RFC FO P1c-2).
//
// An instance becomes REAL on an explicit operator confirm, never on boot or on an edit:
// it is adopted as the same subject node the memory consolidator writes — a
// /facts/<slug> document whose root is the entity — under the consolidator's own key
// (decision 10f.3), so a fact the consolidator learns about it lands on it rather than
// beside it. The key is then recorded on the instance chunk, which is what lets a rename
// keep it. Statements among instances are written with method `ontology`.
//
// NEVER DESTRUCTIVE. A removed instance's subject is not erased — an operator deletes a
// subject through erasure, never by editing the ontology (RFC FO §3.5) — it is reported
// as orphaned. A subject the consolidator already learned is LINKED, not duplicated.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// edgeMethodOntology is the statement method of a relation declared in the ontology's
// Instances section. Not a manual method, so a statement that breaks a relation rule is
// annotated rather than refused — the declaration is the operator's, and losing it to a
// refusal would be worse than flagging it.
const edgeMethodOntology = "ontology"

// instanceSubjectTag marks a subject document that an ontology instance stands for, so
// a removed instance's subject can be found and reported (never erased).
const instanceSubjectTag = "ontology-instance"

// InstanceState is one instance's adoption state.
type InstanceState struct {
	Name  string `json:"name"`
	Class string `json:"class,omitempty"`
	Key   string `json:"key,omitempty"`
	// State: pending (would be adopted), adopted (created now), linked (an existing
	// subject took it), renamed, unchanged, error.
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// InstanceReport is what adoption did (or, dry run, would do).
type InstanceReport struct {
	Instances  []InstanceState `json:"instances,omitempty"`
	Adopted    int             `json:"adopted"`
	Linked     int             `json:"linked"`
	Renamed    int             `json:"renamed"`
	Statements int             `json:"statements"`
	// Orphaned names the subjects adopted from instances that are no longer declared —
	// kept, never erased.
	Orphaned []string `json:"orphaned,omitempty"`
	Errors   []string `json:"errors,omitempty"`
}

// AdoptInstances adopts the run's tenant's ontology instances into its entity registry.
// With dryRun it writes nothing and reports each instance's state. Operator plane only:
// the caller is the confirm action.
func (d *Document) AdoptInstances(ctx context.Context, dryRun bool) (InstanceReport, error) {
	var rep InstanceReport
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return rep, err
	}
	read, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath)
	if err != nil {
		return rep, err
	}
	if read.SectionChunks[memrank.SectionInstances] == "" {
		return rep, nil
	}
	if read.partial {
		// A partial read can lose an instance's recorded key and adopt it again under a
		// new one; never act on one.
		return rep, fmt.Errorf("the ontology could not be read completely; try again")
	}
	classes := map[string]memrank.OntologyTerm{}
	for _, c := range memrank.EffectiveClasses(read.Terms, true) {
		classes[strings.ToLower(c.Name)] = c
	}

	roots := map[string]string{} // instance name (lowercase) → subject root chunk
	keys := map[string]bool{}
	for _, inst := range read.Instances {
		st := InstanceState{Name: inst.Name, Class: inst.Class}
		class, declared := classes[strings.ToLower(inst.Class)]
		if inst.Class == "" || !declared {
			st.State = "error"
			st.Note = "no declared class: @a " + inst.Class
			if inst.Class == "" {
				st.Note = "no @a class"
			}
			rep.Errors = append(rep.Errors, inst.Name+": "+st.Note)
			rep.Instances = append(rep.Instances, st)
			continue
		}
		st.Class = class.Name
		var trusted bool
		st.Key, trusted = d.instanceKey(ctx, key, mscope, inst, class.Name)
		if inst.RegistryKey != "" && !trusted {
			st.Note = "its recorded key was not written by adoption and is ignored"
		}
		keys[st.Key] = true
		existing, err := d.chunkIDByNaturalKey(ctx, key, st.Key)
		if err != nil {
			return rep, err
		}
		switch {
		case existing != "":
			roots[strings.ToLower(inst.Name)] = existing
			st.State = d.reconcileExistingSubject(ctx, key, mscope, inst, existing, trusted, dryRun, &rep)
		case dryRun:
			st.State = "pending"
		default:
			root, cerr := d.createInstanceSubject(ctx, key, mscope, inst, class.Name, st.Key)
			if cerr != nil {
				st.State, st.Note = "error", cerr.Error()
				rep.Errors = append(rep.Errors, inst.Name+": "+cerr.Error())
				rep.Instances = append(rep.Instances, st)
				continue
			}
			roots[strings.ToLower(inst.Name)] = root
			st.State = "adopted"
			rep.Adopted++
			d.publishSubjectEvent(ctx, key, mscope, "adopted", st.Key, "")
		}
		if !dryRun && !trusted {
			if err := d.setInstanceRegistryKey(ctx, key, mscope, inst.ChunkID, st.Key); err != nil {
				rep.Errors = append(rep.Errors, inst.Name+": recording its key: "+err.Error())
			}
		}
		rep.Instances = append(rep.Instances, st)
	}

	if !dryRun {
		d.writeInstanceStatements(ctx, key, mscope, read, roots, &rep)
	}
	rep.Orphaned = d.orphanedInstanceSubjects(ctx, key, keys)
	return rep, nil
}

// reconcileExistingSubject handles an instance whose key already has a subject: first
// adoption of a subject the consolidator learned (linked), a renamed instance (renamed —
// the subject takes the new title, keeps the old one as an alias, keeps its key), or
// nothing to do.
func (d *Document) reconcileExistingSubject(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope,
	inst memrank.OntologyInstance, root string, trusted, dryRun bool, rep *InstanceReport) string {
	row, ok, err := d.getChunkRow(ctx, key, root)
	if err != nil || !ok {
		rep.Errors = append(rep.Errors, inst.Name+": cannot read its subject")
		return "error"
	}
	state := "unchanged"
	if !trusted {
		state = "linked"
		rep.Linked++
	}
	renamed := !strings.EqualFold(strings.TrimSpace(row.Title), strings.TrimSpace(inst.Name))
	if renamed {
		state = "renamed"
		rep.Renamed++
	}
	if dryRun {
		return state
	}
	_ = d.tagDocument(ctx, key, row.DocumentID, instanceSubjectTag)
	if !trusted {
		// Record WHICH instance this subject now stands for — the provenance that lets a
		// later rename of the instance keep this key (see instanceKey).
		if err := d.mergeSubjectFields(ctx, key, mscope, root, map[string]any{"instance_chunk": inst.ChunkID}); err != nil {
			rep.Errors = append(rep.Errors, inst.Name+": recording its instance: "+err.Error())
		}
	}
	if renamed {
		if err := d.renameInstanceSubject(ctx, key, mscope, root, row.Title, inst.Name); err != nil {
			rep.Errors = append(rep.Errors, inst.Name+": renaming its subject: "+err.Error())
		}
	}
	return state
}

// instanceKey is the registry key an instance is adopted under, and whether it came
// from a trusted record.
//
// The key recorded on an instance chunk is TRUSTED only when it is the key derived from
// the instance's own class and name, or when the subject it names records that it was
// adopted from THIS instance chunk (createInstanceSubject and a link write that). Any
// other recorded key is ignored: an ontology chunk's fields can be written by whoever can
// write the chunk — a proposal accepted in place, say — and a key naming another subject
// would link this instance to it and rename it. Ignoring it falls back to the derived
// key, which is what a fresh adoption would use anyway.
func (d *Document) instanceKey(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, inst memrank.OntologyInstance, class string) (string, bool) {
	derived := memrank.ConsolidatorEntityKey(class, inst.Name)
	if inst.RegistryKey == "" {
		return derived, false
	}
	if inst.RegistryKey == derived {
		return derived, true
	}
	root, err := d.chunkIDByNaturalKey(ctx, key, inst.RegistryKey)
	if err != nil || root == "" {
		return derived, false
	}
	cb, err := d.readBody(ctx, mscope, key, root)
	if err != nil || len(cb.Fields) == 0 {
		return derived, false
	}
	var f struct {
		InstanceChunk string `json:"instance_chunk"`
	}
	if json.Unmarshal(cb.Fields, &f) != nil || f.InstanceChunk == "" || f.InstanceChunk != inst.ChunkID {
		return derived, false
	}
	return inst.RegistryKey, true
}

// mergeSubjectFields merges keys into a subject root's fields, keeping the rest. Through
// update_chunk, like every other change to a stored chunk.
func (d *Document) mergeSubjectFields(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, root string, add map[string]any) error {
	row, ok, err := d.getChunkRow(ctx, key, root)
	if err != nil || !ok {
		return fmt.Errorf("cannot read the subject")
	}
	cb, err := d.readBody(ctx, mscope, key, root)
	if err != nil {
		return err
	}
	fields := map[string]any{}
	if len(cb.Fields) > 0 {
		_ = json.Unmarshal(cb.Fields, &fields)
	}
	for k, v := range add {
		fields[k] = v
	}
	fb, _ := json.Marshal(fields)
	rev := row.Revision
	raw, _ := json.Marshal(map[string]any{"fields": json.RawMessage(fb), "revision": rev})
	res, err := d.updateChunk(ctx, key, mscope, docInput{Scope: "tenant", ID: root, Fields: fb, Revision: &rev}, raw)
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("%s", res.Text)
	}
	return nil
}

// createInstanceSubject writes the subject node exactly as subject adoption does: a
// /facts/<slug> document whose root carries type + subject + natural_key. If the path is
// taken by another subject with the same name, the class disambiguates it.
func (d *Document) createInstanceSubject(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope,
	inst memrank.OntologyInstance, class, subjectKey string) (string, error) {
	slug := memrank.ConsolidatorSlug(inst.Name)
	var out map[string]any
	for _, path := range []string{"/facts/" + slug, "/facts/" + slug + "-" + memrank.MirrorSegment(class)} {
		res, err := d.createDocument(ctx, key, mscope, docInput{
			Scope: "tenant", Title: inst.Name, Path: path,
			Type: class, Subject: inst.Name, NaturalKey: subjectKey, Tags: []string{instanceSubjectTag},
		})
		if err != nil {
			return "", err
		}
		if !res.IsError {
			_ = json.Unmarshal([]byte(res.Text), &out)
			break
		}
		if !strings.Contains(res.Text, "path") {
			return "", fmt.Errorf("%s", res.Text)
		}
	}
	root, _ := out["root_chunk_id"].(string)
	if root == "" {
		return "", fmt.Errorf("no path under /facts was free for %s", inst.Name)
	}
	fields, _ := json.Marshal(map[string]any{
		"adopted_from": "ontology", "instance_chunk": inst.ChunkID, "aliases": inst.Alt,
	})
	body := "Adopted from the tenant ontology's Instances section, where an operator declared it.\n\n" +
		"It is the same subject the memory tier records facts about. Removing it from the ontology " +
		"does not erase it; an operator deletes a subject through erasure."
	// create_document writes an empty root body and no fields, so both go on afterwards.
	// Best-effort, as for an adopted subject proposal: the entity existing is the adoption.
	_ = d.writeBody(ctx, mscope, key, root, class, body, fields)
	return root, nil
}

// renameInstanceSubject gives the subject the instance's new title and keeps the old one
// among its aliases. The key — and so every fact filed under it — is unchanged.
func (d *Document) renameInstanceSubject(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, root, oldTitle, newTitle string) error {
	row, ok, err := d.getChunkRow(ctx, key, root)
	if err != nil || !ok {
		return fmt.Errorf("cannot read the subject")
	}
	cb, err := d.readBody(ctx, mscope, key, root)
	if err != nil {
		return err
	}
	fields := map[string]any{}
	if len(cb.Fields) > 0 {
		_ = json.Unmarshal(cb.Fields, &fields)
	}
	aliases, _ := fields["aliases"].([]any)
	aliases = append(aliases, oldTitle)
	fields["aliases"] = aliases
	fb, _ := json.Marshal(fields)
	rev := row.Revision
	raw, _ := json.Marshal(map[string]any{"title": newTitle, "fields": json.RawMessage(fb), "revision": rev})
	res, err := d.updateChunk(ctx, key, mscope, docInput{Scope: "tenant", ID: root, Title: newTitle, Fields: fb, Revision: &rev}, raw)
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("%s", res.Text)
	}
	return nil
}

// setInstanceRegistryKey records the adopted key on the instance chunk. Through
// update_chunk, so the revision bumps and the cached ontology read is invalidated.
func (d *Document) setInstanceRegistryKey(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, chunkID, subjectKey string) error {
	row, ok, err := d.getChunkRow(ctx, key, chunkID)
	if err != nil || !ok {
		return fmt.Errorf("cannot read the instance chunk")
	}
	cb, err := d.readBody(ctx, mscope, key, chunkID)
	if err != nil {
		return err
	}
	fields := map[string]any{}
	if len(cb.Fields) > 0 {
		_ = json.Unmarshal(cb.Fields, &fields)
	}
	fields["registry_key"] = subjectKey
	fb, _ := json.Marshal(fields)
	rev := row.Revision
	raw, _ := json.Marshal(map[string]any{"fields": json.RawMessage(fb), "revision": rev})
	res, err := d.updateChunk(ctx, key, mscope, docInput{Scope: "tenant", ID: chunkID, Fields: fb, Revision: &rev}, raw)
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("%s", res.Text)
	}
	return nil
}

// writeInstanceStatements writes, between adopted subjects: instance_of to the class
// chunk (where the class has one — a standard class does not), the parent's @nesting
// kind, and each declared relation. Idempotent: a statement is unique per (from, to,
// kind), so re-confirming re-asserts rather than duplicates.
func (d *Document) writeInstanceStatements(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope,
	read OntologyRead, roots map[string]string, rep *InstanceReport) {
	view := d.relationView(ctx)
	link := func(from, to, kind, who string) {
		// Checked HERE rather than left to the statement layer: a reserved kind is the
		// runtime's alone, and with a Relations section an instance may only use kinds in
		// force — the method is not manual, so the gate would otherwise only annotate.
		if memrank.IsReservedRelation(kind) {
			rep.Errors = append(rep.Errors, who+": "+kind+" is written only by the runtime; not written")
			return
		}
		if view.active {
			if _, declared := view.byName[kind]; !declared {
				rep.Errors = append(rep.Errors, who+": "+kind+" is not a relation kind this tenant declares; not written")
				return
			}
		}
		res, err := d.linkChunks(ctx, key, mscope, docInput{Scope: "tenant", FromID: from, ToID: to, Kind: kind, edgeMethod: edgeMethodOntology})
		switch {
		case err != nil:
			rep.Errors = append(rep.Errors, who+": "+kind+": "+err.Error())
		case res.IsError:
			rep.Errors = append(rep.Errors, who+": "+kind+": "+res.Text)
		default:
			rep.Statements++
		}
	}
	for _, inst := range read.Instances {
		from := roots[strings.ToLower(inst.Name)]
		if from == "" {
			continue
		}
		if classChunk := d.chunkIDForEntity(ctx, key, read.DocumentID, inst.Class); classChunk != "" {
			link(from, classChunk, "instance_of", inst.Name)
		}
		if inst.Parent != "" && inst.ParentKind != "" {
			if to := roots[strings.ToLower(inst.Parent)]; to != "" {
				link(from, to, inst.ParentKind, inst.Name)
			}
		}
		for _, r := range inst.Relations {
			to := roots[strings.ToLower(r.Target)]
			if to == "" {
				rep.Errors = append(rep.Errors, inst.Name+": @"+r.Kind+" "+r.Target+": no adopted instance has that name")
				continue
			}
			link(from, to, r.Kind, inst.Name)
		}
	}
}

// orphanedInstanceSubjects names the subjects adopted from instances whose key no
// current instance carries.
func (d *Document) orphanedInstanceSubjects(ctx context.Context, key sqlmem.ScopeKey, current map[string]bool) []string {
	res, err := d.query(ctx, key,
		`SELECT coalesce(m.natural_key, ''), doc.title FROM document_tags t
		   JOIN documents doc ON doc.id = t.document_id
		   LEFT JOIN chunk_memory_meta m ON m.chunk_id = doc.root_chunk_id
		  WHERE t.tag = ?`, instanceSubjectTag)
	if err != nil {
		return nil
	}
	var out []string
	for _, row := range res.Rows {
		if len(row) < 2 {
			continue
		}
		if nk := asStr(row[0]); nk != "" && !current[nk] {
			out = append(out, asStr(row[1])+" ("+nk+")")
		}
	}
	sort.Strings(out)
	return out
}

// tagDocument adds one tag to a document, leaving its others.
func (d *Document) tagDocument(ctx context.Context, key sqlmem.ScopeKey, docID, tag string) error {
	have, err := d.listDocumentTags(ctx, key, docID)
	if err != nil {
		return err
	}
	for _, t := range have {
		if t == tag {
			return nil
		}
	}
	return d.replaceDocumentTags(ctx, key, docID, append(have, tag))
}

// proposeInstance files an inert instance into the Instances section. Its class comes
// from parent — an instance is a thing OF a declared class — and is written as the
// proposal's `@a` line, so accepting it in place makes it a complete instance.
func (d *Document) proposeInstance(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, read OntologyRead, name string, in docInput) (tools.Result, error) {
	section := read.SectionChunks[memrank.SectionInstances]
	if section == "" {
		return errBusiness("propose_entity: this tenant's ontology declares no Instances section, so there is "+
			"nowhere to propose an instance", "Record it as a fact instead; an operator decides whether to declare instances."), nil
	}
	class := strings.TrimSpace(in.Parent)
	if class == "" {
		return errValidation("propose_entity: an instance needs parent — the class it is an instance of",
			"Pass parent: a declared class, e.g. organization."), nil
	}
	var declared string
	var have []string
	for _, c := range memrank.EffectiveClasses(read.Terms, true) {
		have = append(have, c.Name)
		if strings.EqualFold(c.Name, class) {
			declared = c.Name
		}
	}
	if declared == "" {
		sort.Strings(have)
		return errValidation("propose_entity: no class named "+class+" is declared (have: "+strings.Join(have, ", ")+")",
			"Pass a declared class as parent."), nil
	}
	for _, inst := range read.Instances {
		if strings.EqualFold(inst.Name, name) {
			return errBusiness("propose_entity: instance "+name+" is already declared", "Use it as it is."), nil
		}
	}
	for _, p := range read.Proposals {
		if p.Section == memrank.SectionInstances && strings.EqualFold(p.Name, name) {
			return errBusiness("propose_entity: instance "+name+" is already "+p.Status+" — an operator has seen it",
				"Do not propose it again; an operator decides."), nil
		}
	}
	return d.createOntologyProposal(ctx, key, mscope, read.DocumentID, section, name, "@a "+declared+"\n\n"+neutraliseDirectives(in.Body))
}
