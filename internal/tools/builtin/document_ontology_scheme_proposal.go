package builtin

// document_ontology_scheme_proposal.go — a WHOLE concept scheme proposed at once (RFC GM
// P3.1), the shape import_scheme (P1.4) files too.
//
// THE SHAPE. One proposed chunk for the scheme, under the Schemes section (under the
// document root when there is none yet), and one proposed chunk per concept beneath it,
// nested as the concepts are. Every chunk's body is its meaning with any directive line
// quoted (neutraliseDirectives), and the directives travel as STRUCTURED, INERT data in
// the chunk's fields (RFC GQ D12):
//
//	scheme:  {"scheme_proposal":  {"header": {domain, projects, origin, source, version}}}
//	concept: {"concept_proposal": {notation, alt, exact_match}}
//
// Inert three ways: the scheme chunk is proposed, so schemeWalker reads it as one
// proposal and never walks under it; each concept chunk is proposed too, so no lookup by
// title takes one for an entity in force; and no directive is in any body.
//
// ACCEPTING (AcceptSchemeProposal, from the operator's proposals route) checks the
// stored data again, puts the Schemes section in place when the proposal carried it,
// writes the header into the scheme's body and clears its status, then does the same for
// each concept, parent before child. Every chunk keeps its id. The data is checked
// before anything is written; a store fault part-way leaves the remaining concepts as
// ordinary concept proposals under the accepted scheme, each still acceptable alone.
// Rejecting is the route's ordinary reject: the scheme becomes a tombstone and nothing
// in force changes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	schemeProposalField  = "scheme_proposal"
	conceptProposalField = "concept_proposal"
)

type schemeProposalMark struct {
	Header memrank.SchemeHeader `json:"header"`
}

type conceptProposalMark struct {
	Notation   string   `json:"notation,omitempty"`
	Alt        []string `json:"alt,omitempty"`
	ExactMatch string   `json:"exact_match,omitempty"`
}

// proposalMark decodes one marker key of a chunk's fields; ok is false when the key is
// absent or does not decode.
func proposalMark(fields json.RawMessage, key string, into any) bool {
	if len(fields) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(fields, &m) != nil || len(m[key]) == 0 {
		return false
	}
	return json.Unmarshal(m[key], into) == nil
}

// withoutMark is fields with the marker key removed, every other key kept.
func withoutMark(fields json.RawMessage, key string) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(fields) > 0 {
		_ = json.Unmarshal(fields, &m)
	}
	delete(m, key)
	out, _ := json.Marshal(m)
	return out
}

// wantsWholeScheme reports a propose_entity call that names a new scheme's concepts or
// header: what makes a schemes proposal a whole scheme rather than one concept.
func wantsWholeScheme(in docInput) bool {
	return len(in.Concepts) > 0 || in.Header != nil
}

// proposeScheme is propose_entity section=schemes for a NEW scheme: no parent, and the
// scheme's concepts (and header) given. The body is the scheme's meaning.
func (d *Document) proposeScheme(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, read OntologyRead, name string, in docInput) (tools.Result, error) {
	if strings.TrimSpace(in.Parent) != "" {
		return errValidation("propose_entity: concepts and header describe a NEW scheme, which has no parent",
			"Omit parent to propose a whole scheme, or omit concepts and header to propose one concept under parent."), nil
	}
	p := memrank.SchemeProposal{Name: name, Meaning: in.Body, Concepts: in.Concepts}
	if in.Header != nil {
		p.Header = *in.Header
	}
	return d.fileSchemeProposal(ctx, key, mscope, read, "propose_entity", p, nil)
}

// fileSchemeProposal checks p and writes it as the proposed chunks described above.
func (d *Document) fileSchemeProposal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, read OntologyRead,
	op string, p memrank.SchemeProposal, warnings []string) (tools.Result, error) {
	more, err := p.Check()
	if err != nil {
		next := "Fix it and file the proposal again."
		if errors.Is(err, memrank.ErrSchemeTooLarge) {
			next = "Propose a smaller scheme, or its upper levels first. A larger scheme needs a document of its own, " +
				"which is not supported yet (RFC GM P1.5)."
		}
		return errValidation(op+": "+err.Error(), next), nil
	}
	warnings = append(warnings, more...)
	seg := memrank.MirrorSegment(p.Name)
	for _, s := range read.Schemes {
		if memrank.MirrorSegment(s.Name) == seg {
			return errBusiness(op+": a scheme named "+s.Name+" is already declared",
				"Propose its new concepts one by one with parent set to it, or pick another name."), nil
		}
	}
	for _, pr := range read.Proposals {
		if pr.Scheme != nil && memrank.MirrorSegment(pr.Name) == seg {
			return errBusiness(op+": scheme "+pr.Name+" is already "+pr.Status+" — an operator has seen it",
				"Do not propose it again; an operator decides."), nil
		}
	}
	parent := read.SectionChunks[memrank.SectionSchemes]
	if parent == "" {
		// The proposal carries the section: accepting puts one in place (RFC GM P3.1).
		parent = read.RootChunkID
	}
	mark, _ := json.Marshal(map[string]any{schemeProposalField: schemeProposalMark{Header: p.Header}})
	schemeID, err := d.createProposalChunk(ctx, key, mscope, read.DocumentID, parent, p.Name, p.Meaning, mark)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	var file func(parentID string, cs []memrank.ProposedConcept) error
	file = func(parentID string, cs []memrank.ProposedConcept) error {
		for _, c := range cs {
			mark, _ := json.Marshal(map[string]any{conceptProposalField: conceptProposalMark{
				Notation: c.Notation, Alt: c.Alt, ExactMatch: c.ExactMatch}})
			id, err := d.createProposalChunk(ctx, key, mscope, read.DocumentID, parentID, c.Name, c.Meaning, mark)
			if err != nil {
				return err
			}
			if err := file(id, c.Children); err != nil {
				return err
			}
		}
		return nil
	}
	if err := file(schemeID, p.Concepts); err != nil {
		// Half a proposal is still inert, but it would be reviewed as the whole one.
		_, _ = d.cascadeDeleteChunks(ctx, key, mscope, []string{schemeID})
		return errFrom(op+": "+err.Error(), err), nil
	}
	out := map[string]any{
		"proposed":  p.Name,
		"chunk_id":  schemeID,
		"parent_id": parent,
		"concepts":  len(memrank.OutlineOf(p.Concepts)),
		"header":    p.Header,
		"note": "Filed as one suggestion, not in force: the scheme, its header and its concepts. An operator " +
			"accepts or rejects it in the Ontology page; nothing changes for any run until they do.",
	}
	if parent == read.RootChunkID {
		out["carries_section"] = true
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return jsonResult(out)
}

// createProposalChunk writes one proposed chunk and returns its id. Called directly, not
// through Execute, for the reason createOntologyProposal gives.
func (d *Document) createProposalChunk(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope,
	docID, parentID, title, meaning string, fields json.RawMessage) (string, error) {
	res, err := d.createChunk(ctx, key, mscope, docInput{
		Scope: "tenant", DocumentID: docID, ParentID: parentID, Title: title,
		Status: memrank.OntologyStatusProposed, Body: neutraliseDirectives(meaning), Fields: fields,
	})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", errors.New(res.Text)
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(res.Text), &out) != nil || out.ID == "" {
		return "", fmt.Errorf("create_chunk returned no id")
	}
	return out.ID, nil
}

// proposedView is what the reviewer is shown of a whole-scheme proposal; nil for a chunk
// that is not one.
func (w *schemeWalker) proposedView(c ontologyChunk) *memrank.ProposedSchemeView {
	if w.fieldsOf == nil {
		return nil
	}
	var mark schemeProposalMark
	if !proposalMark(w.fieldsOf(c.id), schemeProposalField, &mark) {
		return nil
	}
	// The same concepts AcceptSchemeProposal gathers (proposed, marked; an unmarked
	// chunk and its subtree are left out), normalised by the same Check, so what the
	// reviewer sees is what accepting writes — directives included.
	var walk func(id string) []memrank.ProposedConcept
	walk = func(id string) []memrank.ProposedConcept {
		var out []memrank.ProposedConcept
		for _, k := range w.kids[id] {
			var m conceptProposalMark
			if strings.ToLower(strings.TrimSpace(k.status)) != memrank.OntologyStatusProposed ||
				!proposalMark(w.fieldsOf(k.id), conceptProposalField, &m) {
				continue
			}
			out = append(out, memrank.ProposedConcept{Name: w.entityName(k), Notation: m.Notation, Alt: m.Alt,
				ExactMatch: m.ExactMatch, Children: walk(k.id)})
		}
		return out
	}
	p := memrank.SchemeProposal{Name: w.entityName(c), Header: mark.Header, Concepts: walk(c.id)}
	// A proposal that fails Check is shown as stored; accepting it is refused anyway.
	_, _ = p.Check()
	v := &memrank.ProposedSchemeView{Header: p.Header, Outline: memrank.OutlineOf(p.Concepts)}
	v.Concepts = len(v.Outline)
	return v
}

// rootProposal reads a whole-scheme proposal filed under the document root — one that
// carries its Schemes section — as the scheme proposal it is, and reports whether c was
// one. Without it the class walk would read the scheme and each concept as a class.
func (w *schemeWalker) rootProposal(c ontologyChunk) bool {
	if !memrank.IsInertEntityStatus(c.status) || w.proposedView(c) == nil {
		return false
	}
	w.schemes([]ontologyChunk{c})
	return true
}

// SchemeAcceptReport says what accepting a scheme proposal put in force.
type SchemeAcceptReport struct {
	Scheme         string   `json:"scheme,omitempty"`
	Concepts       int      `json:"concepts"`
	SectionCreated bool     `json:"section_created,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

// ErrSchemeProposalInvalid wraps a stored proposal that no longer passes its checks;
// nothing was written.
var ErrSchemeProposalInvalid = errors.New("the proposal cannot be accepted")

// proposalNode is a concept chunk of a stored scheme proposal, in preorder.
type proposalNode struct {
	id, parent string
	revision   int
	body       chunkBody
}

// AcceptSchemeProposal accepts a whole-scheme proposal, or one concept proposal that
// carries structured directives (a concept left over from a scheme accepted part-way).
// handled is false for any other chunk, which the caller accepts as before.
func (d *Document) AcceptSchemeProposal(ctx context.Context, scope, chunkID string) (handled bool, rep SchemeAcceptReport, err error) {
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return false, rep, err
	}
	// One accept at a time per tenant ontology, read included: two accepts that both
	// saw no Schemes section would each add one, and a double-click would accept the
	// same proposal twice. Within this process only; on another replica the revision
	// each update carries still refuses the second write of a chunk.
	unlock, err := d.editions.lock(ctx, key, "ontology_accept", memrank.OntologyPath)
	if err != nil {
		return false, rep, err
	}
	defer unlock()
	read, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath)
	if err != nil {
		return false, rep, err
	}
	rows, err := d.query(ctx, key, `SELECT id, coalesce(parent_id, ''), coalesce(title, ''), position,
		coalesce(status, ''), revision FROM chunks WHERE document_id = ? ORDER BY position`, read.DocumentID)
	if err != nil {
		return false, rep, err
	}
	kids := map[string][]proposalNode{}
	byID := map[string]proposalNode{}
	titles, statuses := map[string]string{}, map[string]string{}
	for _, r := range rows.Rows {
		if len(r) < 6 {
			continue
		}
		n := proposalNode{id: asStr(r[0]), parent: asStr(r[1]), revision: asInt(r[5])}
		titles[n.id], statuses[n.id] = memrank.TrimOntologyName(asStr(r[2])), strings.ToLower(asStr(r[4]))
		kids[n.parent] = append(kids[n.parent], n)
		byID[n.id] = n
	}
	self, ok := byID[chunkID]
	if !ok || statuses[chunkID] != memrank.OntologyStatusProposed {
		return false, rep, nil
	}
	if self.body, err = d.readBody(ctx, mscope, key, chunkID); err != nil {
		return false, rep, err
	}
	var smark schemeProposalMark
	isScheme := proposalMark(self.body.Fields, schemeProposalField, &smark)
	var one conceptProposalMark
	if !isScheme && !proposalMark(self.body.Fields, conceptProposalField, &one) {
		return false, rep, nil
	}

	// Gather the concepts: the proposed, marked chunks under the scheme. A chunk without
	// the mark (filed beside the proposal some other way) is left as it is, with its
	// subtree: it is reviewed on its own.
	var nodes []proposalNode
	var gather func(id string) ([]memrank.ProposedConcept, error)
	gather = func(id string) ([]memrank.ProposedConcept, error) {
		var out []memrank.ProposedConcept
		for _, k := range kids[id] {
			if statuses[k.id] != memrank.OntologyStatusProposed {
				continue
			}
			b, err := d.readBody(ctx, mscope, key, k.id)
			if err != nil {
				return nil, err
			}
			var m conceptProposalMark
			if !proposalMark(b.Fields, conceptProposalField, &m) {
				continue
			}
			k.body = b
			nodes = append(nodes, k)
			c := memrank.ProposedConcept{Name: titles[k.id], Notation: m.Notation, Alt: m.Alt, ExactMatch: m.ExactMatch}
			if c.Children, err = gather(k.id); err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, nil
	}
	p := memrank.SchemeProposal{Name: titles[chunkID], Header: smark.Header}
	if isScheme {
		if p.Concepts, err = gather(chunkID); err != nil {
			return true, rep, err
		}
	} else {
		// One concept: checked as the only concept of a scheme of its own name.
		nodes = []proposalNode{self}
		p.Concepts = []memrank.ProposedConcept{{Name: titles[chunkID], Notation: one.Notation, Alt: one.Alt, ExactMatch: one.ExactMatch}}
	}
	warnings, err := p.Check()
	if err != nil {
		return true, rep, fmt.Errorf("%w: %v", ErrSchemeProposalInvalid, err)
	}
	if err := tagInForce(read, isScheme, self.parent, p.Name); err != nil {
		return true, rep, fmt.Errorf("%w: %v", ErrSchemeProposalInvalid, err)
	}
	rep.Warnings = warnings
	flat := flattenProposed(p.Concepts)
	if len(flat) != len(nodes) {
		return true, rep, fmt.Errorf("%w: its concepts changed while it was read; try again", ErrSchemeProposalInvalid)
	}
	if isScheme {
		rep.Scheme = p.Name
		if rep.SectionCreated, err = d.placeSchemeProposal(ctx, read, self); err != nil {
			return true, rep, err
		}
		if err := d.acceptProposalChunk(ctx, self, p.Header.Directives(), schemeProposalField); err != nil {
			return true, rep, err
		}
	}
	for i, n := range nodes {
		if err := d.acceptProposalChunk(ctx, n, flat[i].Directives(), conceptProposalField); err != nil {
			return true, rep, fmt.Errorf("accepted %d of %d concepts, then: %w; the rest stay proposed and "+
				"can be accepted one by one", rep.Concepts, len(nodes), err)
		}
		rep.Concepts++
	}
	return true, rep, nil
}

// tagInForce refuses what would put a mirror tag in force twice: a scheme whose name's
// segment an in-force scheme already has (declared since the proposal was filed), or a
// lone concept whose tag a concept in force beside it already has. The filing checks
// the first only, and only as things stood then.
func tagInForce(read OntologyRead, isScheme bool, parentID, name string) error {
	seg := memrank.MirrorSegment(name)
	if isScheme {
		for _, s := range read.Schemes {
			if memrank.MirrorSegment(s.Name) == seg {
				return fmt.Errorf("a scheme named %s is already in force", s.Name)
			}
		}
		return nil
	}
	parentTag := ""
	for _, s := range read.Schemes {
		if s.ChunkID == parentID {
			parentTag = memrank.MirrorSegment(s.Name)
		}
		for _, c := range s.Concepts {
			if c.ChunkID == parentID {
				parentTag = c.Tag
			}
		}
	}
	if parentTag == "" {
		return nil // under a concept still proposed: nothing in force beside it
	}
	tag := parentTag + "/" + seg
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			if c.Tag == tag {
				return fmt.Errorf("a concept with the tag %s is already in force", tag)
			}
		}
	}
	return nil
}

// placeSchemeProposal puts the Schemes section in place for a proposal filed without
// one, and moves the scheme under it. created says a section was added.
func (d *Document) placeSchemeProposal(ctx context.Context, read OntologyRead, scheme proposalNode) (created bool, err error) {
	section := read.SectionChunks[memrank.SectionSchemes]
	if section == "" {
		res, err := d.execOp(ctx, map[string]any{"op": "create_chunk", "scope": "tenant",
			"document_id": read.DocumentID, "parent_id": read.RootChunkID, "title": "Schemes",
			"body": "@section schemes\n\nConcept schemes: taxonomies over the classes above.\n"})
		if err != nil {
			return false, err
		}
		var out struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(res), &out) != nil || out.ID == "" {
			return false, fmt.Errorf("create the Schemes section: no id returned")
		}
		section, created = out.ID, true
	}
	if scheme.parent == section {
		return created, nil
	}
	_, err = d.execOp(ctx, map[string]any{"op": "move_chunk", "scope": "tenant", "id": scheme.id, "new_parent_id": section})
	return created, err
}

// acceptProposalChunk puts one proposed chunk in force in ONE update: its body becomes
// the checked directives above its own text (any directive line in that text stays
// quoted), the marker leaves its fields, and its status is cleared. The update carries
// the revision n's body was read at, so an edit made since is refused, not overwritten.
func (d *Document) acceptProposalChunk(ctx context.Context, n proposalNode, directives []string, mark string) error {
	body := neutraliseDirectives(strings.TrimSpace(n.body.Body))
	if len(directives) > 0 {
		body = strings.TrimSpace(strings.Join(directives, "\n") + "\n\n" + body)
	}
	_, err := d.execOp(ctx, map[string]any{"op": "update_chunk", "scope": "tenant", "id": n.id,
		"revision": n.revision, "body": body, "fields": withoutMark(n.body.Fields, mark), "status": ""})
	return err
}

// execOp runs one Document op on ctx (the operator's) and returns its JSON text.
func (d *Document) execOp(ctx context.Context, req map[string]any) (string, error) {
	raw, _ := json.Marshal(req)
	res, err := d.Execute(ctx, raw)
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("%s: %s", req["op"], res.Text)
	}
	return res.Text, nil
}

// flattenProposed lists a concept tree in preorder — the order the chunks were gathered
// in, which Check's flattening keeps.
func flattenProposed(cs []memrank.ProposedConcept) []memrank.ProposedConcept {
	var out []memrank.ProposedConcept
	for _, c := range cs {
		out = append(out, c)
		out = append(out, flattenProposed(c.Children)...)
	}
	return out
}
