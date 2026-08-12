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

	tenant, _ := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
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

	tenant, _ := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	if s.store == nil || s.sqlMem == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable",
			"the ontology needs SQL Memory; none is configured")
		return
	}
	mi := memInject{Tenant: tenant}
	s.ensureOntologyDoc(r.Context(), mi)
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(r.Context(), mi)

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
