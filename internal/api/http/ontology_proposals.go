package http

// ontology_proposals.go — the two operator actions RFC CA phase 1 adds: resolve a
// proposal, and adopt a type that is in force but not in the document.
//
// BOTH ARE DEDICATED OPERATIONS RATHER THAN DOCUMENT WRITES, and that is the decision
// worth stating. Every effect here is reachable through the generic /v1/_document route
// by a caller who knows the chunk shape — clear a status, create a chunk with the right
// bullets. What these buy is that the operation cannot produce a state the reader
// silently ignores:
//
//   - resolving a proposal accepts only `accept`/`reject`, only on a chunk that is
//     actually a proposal, and only in the tenant's own ontology document;
//   - adopting copies the fields from the EFFECTIVE ontology server-side, so an adopted
//     type cannot drift from the standard one it claims to override, and it records what
//     it was copied from.
//
// The narrowing is the same argument the draft↔confirmed flip already makes: a
// free-form status write exists elsewhere, and the value of this route is that it
// cannot spell the status wrong.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// handleOntologyProposal serves POST /v1/_ontology/proposals — accept or reject one.
//
// Accepting CLEARS the status, which puts the entity in force exactly where it already
// sits in the tree; nothing moves, because a proposal is the entity switched off rather
// than a copy waiting to be transcribed. Rejecting sets `rejected` and keeps the chunk
// as a tombstone, so a curator can read it and stop re-proposing what was turned down.
func (s *Server) handleOntologyProposal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChunkID string `json:"chunk_id"`
		Action  string `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			"expected {\"chunk_id\": \"…\", \"action\": \"accept\"|\"reject\"}")
		return
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	if action != "accept" && action != "reject" {
		writeJSONError(w, http.StatusBadRequest, "invalid_action", "action must be \"accept\" or \"reject\"")
		return
	}
	chunkID := strings.TrimSpace(body.ChunkID)
	if chunkID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "chunk_id is required")
		return
	}

	tenant := s.ontologyTenant(r)
	if s.store == nil || s.sqlMem == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable",
			"the ontology needs SQL Memory; none is configured")
		return
	}
	mi := memInject{Tenant: tenant}
	s.ensureOntologyDoc(r.Context(), mi)
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(r.Context(), mi)

	read, err := doc.OntologyTermsFromTree(dctx, "tenant", meminject.OntologyPath)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable", err.Error())
		return
	}
	// The chunk must be a PROPOSAL IN THIS TENANT'S ontology. Both halves matter: the
	// first stops this route becoming a general status-writer, and the second stops it
	// becoming a way to reach another tenant's chunk by id. A miss is an opaque 404 —
	// "not a proposal here" and "does not exist" are the same answer to a caller who
	// should not be able to tell them apart.
	var target *meminject.OntologyProposal
	for i := range read.Proposals {
		if read.Proposals[i].ChunkID == chunkID {
			target = &read.Proposals[i]
			break
		}
	}
	if target == nil {
		writeJSONError(w, http.StatusNotFound, "not_a_proposal",
			"no proposal with that chunk_id in this tenant's ontology")
		return
	}
	if action == "accept" && target.Status != meminject.OntologyStatusProposed {
		// Re-accepting a rejection would be a silent un-reject. Say what state it is in.
		writeJSONError(w, http.StatusConflict, "not_pending",
			fmt.Sprintf("that entity is %q, not %q — clear its status in the document editor if you meant to restore it",
				target.Status, meminject.OntologyStatusProposed))
		return
	}

	// A SUBJECT PROPOSAL ACCEPTS DIFFERENTLY, because accepting it means something
	// else. A type proposal in force is a type; a subject in force would be a TYPE
	// NAMED "Dave" — injected into the extractor's prompt, which would start typing
	// facts as Dave. What accepting a subject means is that the tenant now knows it,
	// and what reads that is the entity registry, not the type list. So this mints
	// the entity and removes the suggestion; the minted entity carries the evidence.
	if action == "accept" {
		if isSubject, f, evidence := doc.SubjectProposal(dctx, "tenant", chunkID); isSubject {
			docID, aerr := s.adoptSubjectProposal(dctx, doc, f, evidence, chunkID)
			if aerr != nil {
				writeJSONError(w, http.StatusInternalServerError, "adopt_failed", aerr.Error())
				return
			}
			_ = docID
			s.writeOntologyState(w, r.Context(), tenant)
			return
		}
	}

	newStatus := meminject.OntologyStatusRejected
	if action == "accept" {
		newStatus = "" // in force, in place
	}
	if err := s.setOntologyChunkStatus(dctx, doc, chunkID, newStatus); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "ontology_write_failed", err.Error())
		return
	}
	s.writeOntologyState(w, r.Context(), tenant)
}

// handleOntologyAdopt serves POST /v1/_ontology/adopt — take a copy of a standard type
// into the document so it can be extended or subclassed.
//
// The transcription this removes is the whole reason it exists: the documented way to
// subclass a standard type is to declare it yourself (which overrides the standard one
// by name) and nest beneath your copy — which meant reading four field names off the
// panel and retyping them, correctly, by hand.
func (s *Server) handleOntologyAdopt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		// Section selects what is adopted: "" or "classes" for a standard entity type,
		// "relations" for a standard relation kind (RFC FO P1b).
		Section string `json:"section"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "expected {\"name\": \"event\"}")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "name is required")
		return
	}

	tenant := s.ontologyTenant(r)
	if s.store == nil || s.sqlMem == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable",
			"the ontology needs SQL Memory; none is configured")
		return
	}
	mi := memInject{Tenant: tenant}
	s.ensureOntologyDoc(r.Context(), mi)
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(r.Context(), mi)

	switch strings.ToLower(strings.TrimSpace(body.Section)) {
	case "", meminject.SectionClasses:
	case meminject.SectionRelations:
		s.adoptRelation(w, r, doc, dctx, tenant, name)
		return
	default:
		writeJSONError(w, http.StatusBadRequest, "invalid_section", "section must be classes or relations")
		return
	}

	// The FIELDS come from the base seed, resolved here rather than sent by the caller.
	// A client-supplied field list could disagree with the type it claims to be a copy
	// of, and the disagreement would be invisible: the document would look like an
	// override of `event` while declaring something else.
	var source *meminject.OntologyTerm
	for _, t := range meminject.BaseSeedOntology() {
		if strings.EqualFold(t.Name, name) {
			term := t
			source = &term
			break
		}
	}
	if source == nil {
		writeJSONError(w, http.StatusBadRequest, "not_a_standard_type",
			"only a standard type can be adopted — this one is not in the base set, so it is "+
				"already yours to edit in the document")
		return
	}

	read, err := doc.OntologyTermsFromTree(dctx, "tenant", meminject.OntologyPath)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable", err.Error())
		return
	}
	for _, t := range read.Terms {
		if strings.EqualFold(t.Name, source.Name) {
			writeJSONError(w, http.StatusConflict, "already_declared",
				"your document already declares that type — edit it in the document instead")
			return
		}
	}
	if read.RootChunkID == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable",
			"the ontology document has no root chunk")
		return
	}

	// PROVENANCE goes in the chunk's structured fields, never the body (RFC CA §8.3).
	// The body is parsed for the entity's field bullets and belongs to the operator; a
	// machine-written line in it would be indistinguishable from something they wrote.
	//
	// Recorded because adoption FREEZES this copy: an override replaces the standard
	// term wholesale, so a field a later release adds to the standard `event` never
	// reaches an operator who adopted it. Without provenance that is undiscoverable
	// after the fact; with it, a future release can diff the frozen copy against the
	// current seed and say so.
	prov, _ := json.Marshal(map[string]any{
		"adopted_from":        source.Name,
		"adopted_from_source": "base",
		"adopted_at_version":  s.buildVersion,
	})
	req, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "scope": "tenant",
		"document_id": read.DocumentID, "parent_id": read.RootChunkID,
		"title": source.Name, "body": adoptedBody(*source), "fields": json.RawMessage(prov),
	})
	if res, aerr := doc.Execute(dctx, req); aerr != nil || res.IsError {
		msg := "adopt failed"
		if aerr != nil {
			msg = aerr.Error()
		} else if res.Text != "" {
			msg = res.Text
		}
		writeJSONError(w, http.StatusInternalServerError, "ontology_write_failed", msg)
		return
	}
	s.writeOntologyState(w, r.Context(), tenant)
}

// adoptRelation copies a standard relation kind into the Relations section. Adopting is
// how a seed kind reaches the extraction prompt (RFC FO decision 10d.2): seed kinds are
// valid without it, but only the tenant's own kinds are listed to agents. The copy is a
// tenant kind and overrides the seed one wholesale, so it is frozen like an adopted type,
// with the same provenance in its fields.
func (s *Server) adoptRelation(w http.ResponseWriter, r *http.Request, doc *builtin.Document, dctx context.Context, tenant, name string) {
	var source *meminject.OntologyRelation
	for _, rel := range meminject.SeedRelations() {
		if rel.Name == name {
			kind := rel
			source = &kind
			break
		}
	}
	if source == nil {
		writeJSONError(w, http.StatusBadRequest, "not_a_standard_kind",
			"only a standard relation kind can be adopted — this one is not in the standard set")
		return
	}
	read, err := doc.OntologyTermsFromTree(dctx, "tenant", meminject.OntologyPath)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable", err.Error())
		return
	}
	section := read.SectionChunks[meminject.SectionRelations]
	if section == "" {
		writeJSONError(w, http.StatusConflict, "no_relations_section",
			"the document declares no Relations section — add a top-level heading whose first line is @section relations")
		return
	}
	for _, rel := range read.Relations {
		if rel.Name == source.Name {
			writeJSONError(w, http.StatusConflict, "already_declared",
				"your document already declares that kind — edit it in the document instead")
			return
		}
	}
	// Nested under the tenant's copy of its parent when there is one, so the sub-kind
	// tree the operator sees is the one that expands; under the section otherwise (the
	// seed parent still applies through the layering).
	parent := section
	for _, rel := range read.Relations {
		if rel.Name == source.Parent && rel.ChunkID != "" {
			parent = rel.ChunkID
		}
	}
	prov, _ := json.Marshal(map[string]any{
		"adopted_from":        source.Name,
		"adopted_from_source": "base",
		"adopted_at_version":  s.buildVersion,
	})
	req, _ := json.Marshal(map[string]any{
		"op": "create_chunk", "scope": "tenant",
		"document_id": read.DocumentID, "parent_id": parent,
		"title": source.Name, "body": adoptedRelationBody(*source), "fields": json.RawMessage(prov),
	})
	if res, aerr := doc.Execute(dctx, req); aerr != nil || res.IsError {
		msg := "adopt failed"
		if aerr != nil {
			msg = aerr.Error()
		} else if res.Text != "" {
			msg = res.Text
		}
		writeJSONError(w, http.StatusInternalServerError, "ontology_write_failed", msg)
		return
	}
	s.writeOntologyState(w, r.Context(), tenant)
}

// adoptedRelationBody writes a seed kind out as directives, so the adopted entry is an
// ordinary editable chunk that parses back to the same kind.
func adoptedRelationBody(rel meminject.OntologyRelation) string {
	var b strings.Builder
	if rel.Meaning != "" {
		b.WriteString(strings.ToUpper(rel.Meaning[:1]) + rel.Meaning[1:] + ".\n\n")
	}
	line := func(s string) { b.WriteString(s + "\n") }
	if rel.Inverse != "" {
		line("@inverse " + rel.Inverse)
	}
	for _, flag := range []struct {
		on   bool
		name string
	}{{rel.Symmetric, "symmetric"}, {rel.Asymmetric, "asymmetric"}, {rel.Irreflexive, "irreflexive"},
		{rel.Transitive, "transitive"}, {rel.Functional, "functional"}, {rel.Dated, "dated"},
		{rel.Walk, "walk"}, {rel.AutoAssert, "auto_assert"}} {
		if flag.on {
			line("@" + flag.name)
		}
	}
	if len(rel.Qualifiers) > 0 {
		var qs []string
		for _, q := range rel.Qualifiers {
			if len(q.Values) > 0 {
				qs = append(qs, q.Name+"("+strings.Join(q.Values, "|")+")")
			} else {
				qs = append(qs, q.Name)
			}
		}
		line("@qualifiers " + strings.Join(qs, ", "))
	}
	if rel.Discriminator != "" {
		line("@discriminator " + rel.Discriminator)
	}
	if rel.TransitiveWithin != "" {
		line("@transitive_within " + rel.TransitiveWithin)
	}
	return b.String()
}

// adoptedBody renders a standard type's fields the way an operator would have typed
// them, so the adopted chunk is an ordinary editable section and not a special case.
func adoptedBody(t meminject.OntologyTerm) string {
	var b strings.Builder
	b.WriteString("Adopted from the standard types so it can be extended or subclassed. " +
		"Your copy now overrides the standard one, wholesale — edit the fields freely.\n\n")
	for _, f := range meminject.AllFields(t) {
		b.WriteString("- `" + f + "`\n")
	}
	return b.String()
}

// setOntologyChunkStatus writes one chunk's status, reading its revision first.
//
// The revision is read here rather than accepted from the caller for the same reason the
// confirm toggle reads it: a stale revision from a client would either fail confusingly
// or, worse, be sent for the wrong chunk.
func (s *Server) setOntologyChunkStatus(dctx context.Context, doc *builtin.Document, chunkID, status string) error {
	rev, err := s.ontologyChunkRevision(dctx, doc, chunkID)
	if err != nil {
		return err
	}
	// Status ONLY — update_chunk is presence-based, so omitting body/fields leaves the
	// operator's Markdown alone. Sending them would risk blanking an entity to flip a
	// flag. An empty status is what "in force" looks like, so it is written explicitly.
	patch, _ := json.Marshal(map[string]any{
		"op": "update_chunk", "scope": "tenant", "id": chunkID,
		"status": status, "revision": rev,
	})
	res, err := doc.Execute(dctx, patch)
	if err != nil {
		return err
	}
	if res.IsError {
		return fmt.Errorf("%s", res.Text)
	}
	return nil
}

// writeOntologyState re-reads and returns the whole state after a mutation, so the
// response shows the EFFECTIVE result rather than the caller's assumption about it.
func (s *Server) writeOntologyState(w http.ResponseWriter, ctx context.Context, tenant string) {
	resp, err := s.ontologyState(ctx, tenant)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// adoptSubjectProposal mints the tenant entity a subject proposal stands for, then
// removes the proposal.
//
// MINT BEFORE REMOVE, and a failed mint leaves the proposal alone. The reverse order
// loses the suggestion on any fault and the operator has nothing left to act on —
// they would have to wait for the consolidator to meet the subject again to get the
// proposal back.
func (s *Server) adoptSubjectProposal(dctx context.Context, doc *builtin.Document, f builtin.SubjectProposalFields, evidence, chunkID string) (string, error) {
	docID, err := doc.AdoptSubjectProposalInScope(dctx, "tenant", f, evidence)
	if err != nil {
		return "", err
	}
	// Best-effort: the entity exists, which is what the operator asked for. A proposal
	// left behind is a duplicate suggestion, not a broken adoption, and propose_subject
	// reports it as already-seen rather than filing it twice.
	_ = doc.DeleteOntologyChunk(dctx, "tenant", chunkID)
	return docID, nil
}
