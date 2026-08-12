package http

// ontology_admin.go — the OPERATOR surface for the tenant ontology: read its
// state, and flip it between draft and confirmed.
//
// The flip was always specified as an operator action rather than something the
// runtime decides, and until now the only way to perform it was to open the
// document in the Path browser and type `confirmed` into the root chunk's status
// box. That works, but it is not a control — nothing tells an operator that the
// word is load-bearing, that their edits are currently inert, or which types are
// actually reaching the extractor. A gate nobody can see the state of is a gate
// that silently stays shut.
//
// So this is deliberately a READ plus a two-value WRITE, not a general document
// editor. Authoring the ontology stays in the Path/Document browser, where editing
// Markdown belongs; what lives here is the part the browser cannot express — the
// gate, and the layered result of passing it.
//
// Both routes are ScopeTenant. A tenant operator confirming its OWN tenant's
// ontology is the intended actor, and refusing the write here while
// POST /v1/_document update_chunk already accepts it from the same token would be
// theatre rather than a boundary.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// ontologyResponse is the wire shape of GET/POST /v1/_ontology.
//
// It reports the tenant layer and the EFFECTIVE ontology separately, because the
// difference between them IS the feature: while the document is draft, Terms is
// what the operator wrote and Effective shows it is not there. Computing both
// server-side means the UI never reimplements the layering — and cannot drift from
// what a run actually gets, which is the whole reason for showing it.
type ontologyResponse struct {
	Tenant string `json:"tenant"`
	// Path + DocumentID let the UI deep-link the Path browser for editing rather
	// than duplicating a Markdown editor here.
	Path        string `json:"path"`
	DocumentID  string `json:"document_id,omitempty"`
	RootChunkID string `json:"root_chunk_id,omitempty"`
	// Status is the root chunk's raw status; Confirmed is the decision derived from
	// it. Both, because they can disagree in the one way that matters: a status of
	// `confirmd` is not confirmed, and reporting only the boolean would hide why.
	Status    string `json:"status"`
	Confirmed bool   `json:"confirmed"`
	// Provisioned is false when the document does not exist yet AND could not be
	// created (no SQL Memory). Distinguishes "nothing here" from "not available",
	// which the UI needs in order to say something useful.
	Provisioned bool `json:"provisioned"`
	// Terms is the tenant layer as parsed from the document — reported whether or
	// not it is active, so an operator can see what confirming would do.
	Terms []meminject.OntologyTerm `json:"terms"`
	// Effective is what a run gets right now: the base seed, with the tenant layer
	// applied only if Confirmed. Each term carries its own source, so the UI can
	// mark which half it came from without diffing against the seed.
	Effective []meminject.OntologyTerm `json:"effective"`
	// Notes carries reader-level caveats the operator has to act on: the document had
	// no per-entity chunks and was read flat, or it nests deeper than the cap and was
	// flattened. A LIST because those conditions are independent and more than one can
	// hold at once — folded into one string, the panel could not render them
	// separately and each new case would make it worse.
	//
	// Surfaced rather than logged because the consequence lands on the operator's
	// data, not on the server.
	Notes []string `json:"notes,omitempty"`
	// Proposals are entities present in the document but NOT in force — a curator's
	// suggestions and the operator's rejections (RFC CA). Reported from the same read
	// that produced Terms, so the two cannot disagree.
	Proposals []meminject.OntologyProposal `json:"proposals,omitempty"`
	// Adoptable names the standard types this document does not declare — the ones the
	// adopt action can copy in. Computed server-side because the answer is "in the
	// effective set and not in the document", which the UI would otherwise re-derive
	// from two lists and get subtly wrong (a tenant term that overrides a standard name
	// is NOT adoptable).
	Adoptable []string `json:"adoptable,omitempty"`
}

// handleOntology serves GET /v1/_ontology — the tenant ontology's state.
//
// Reading PROVISIONS the document on first reference, which is a real behaviour
// change and the point of having the route: provisioning was lazy-on-first-run, so
// an operator who wanted to set up the ontology BEFORE running anything had no
// document to edit. Now opening the page creates it (as `draft`, changing nothing).
func (s *Server) handleOntology(w http.ResponseWriter, r *http.Request) {
	// Admin may focus a tenant via ?tenant= (the UI's tenant switcher); a tenant
	// operator is pinned to its own and the wire value is ignored. An admin with no
	// focus operates on the default tenant — the same one its own runs use.
	tenant, _ := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))

	resp, err := s.ontologyState(r.Context(), tenant)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleOntologySetStatus serves POST /v1/_ontology — the draft↔confirmed flip.
//
// Accepts ONLY those two values. A free-form status write is already available on
// the generic document route; the whole value of this one is that it cannot produce
// a state the gate silently ignores.
func (s *Server) handleOntologySetStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "expected {\"status\": \"confirmed\"|\"draft\"}")
		return
	}
	status := strings.ToLower(strings.TrimSpace(body.Status))
	if status != meminject.OntologyConfirmedStatus && status != meminject.OntologyDraftStatus {
		writeJSONError(w, http.StatusBadRequest, "invalid_status",
			"status must be \""+meminject.OntologyConfirmedStatus+"\" or \""+meminject.OntologyDraftStatus+"\"")
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

	// The gate is the ROOT chunk's status, so the write needs the root's id and its
	// current revision. Both are read here rather than passed on the wire: the
	// caller is toggling one bit and should not have to know the document's
	// internal shape, and a revision that came from the client could be stale in a
	// way that silently targeted the wrong chunk.
	rootID, _, _, err := s.ontologyRoot(dctx, doc)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable", err.Error())
		return
	}
	rev, err := s.ontologyChunkRevision(dctx, doc, rootID)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable", err.Error())
		return
	}

	// Status ONLY. update_chunk is presence-based, so omitting body/fields leaves
	// the operator's Markdown untouched — sending them would risk blanking the
	// document to flip a flag.
	patch, _ := json.Marshal(map[string]any{
		"op": "update_chunk", "scope": "tenant", "id": rootID,
		"status": status, "revision": rev,
	})
	res, err := doc.Execute(dctx, patch)
	if err != nil || res.IsError {
		msg := res.Text
		if err != nil {
			msg = err.Error()
		}
		writeJSONError(w, http.StatusInternalServerError, "ontology_write_failed", msg)
		return
	}

	// Re-read rather than synthesizing the new state: the response then shows the
	// EFFECTIVE ontology after the flip, which is what the operator wanted to know.
	resp, err := s.ontologyState(r.Context(), tenant)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ontologyState composes the read shape for one tenant, provisioning on first
// reference.
func (s *Server) ontologyState(ctx context.Context, tenant string) (ontologyResponse, error) {
	resp := ontologyResponse{
		Tenant: tenant, Path: meminject.OntologyPath,
		Status:    meminject.OntologyDraftStatus,
		Terms:     []meminject.OntologyTerm{},
		Effective: meminject.EffectiveOntology(nil, false),
	}
	if s.store == nil || s.sqlMem == nil {
		// Degraded, not an error: a deployment with no SQL Memory still runs on the
		// base seed, and reporting that honestly beats a 503 the UI must special-case.
		return resp, nil
	}

	mi := memInject{Tenant: tenant}
	terms, confirmed, notes := s.tenantOntologyTerms(ctx, mi)

	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(ctx, mi)
	if read, rerr := doc.OntologyTermsFromTree(dctx, "tenant", meminject.OntologyPath); rerr == nil {
		resp.Proposals = read.Proposals
	}
	rootID, docID, status, err := s.ontologyRoot(dctx, doc)
	if err == nil {
		resp.Provisioned = true
		resp.RootChunkID, resp.DocumentID = rootID, docID
		if status != "" {
			resp.Status = status
		}
	}
	if terms != nil {
		// Resolve inheritance on the REPORTED tenant layer too, not only on the
		// effective one. Without this the panel's "this deployment defines" column
		// shows a subclass with no sign of what it inherits, which is exactly the
		// question an operator has when looking at a subclass that declares one field.
		reported, rerooted := meminject.EnforcePinnedRoots(terms)
		reported = meminject.ResolveInheritance(reported)
		for i := range reported {
			reported[i].NameIssue = meminject.OntologyNameIssue(reported[i].Name)
		}
		resp.Terms = reported
		if len(rerooted) > 0 {
			// Said out loud because the nesting was accepted by the document and then
			// dropped here. An operator who nested `preference` under something and saw
			// their document keep it would otherwise believe it took effect.
			notes = append(notes, "These are the memory tier's own structural types and "+
				"cannot be nested under another type, so they were kept as roots: "+
				strings.Join(rerooted, ", ")+". Subclassing them is fine — it is giving "+
				"them a parent that is not.")
		}
	}
	resp.Confirmed = confirmed
	resp.Notes = notes
	// A standard type is adoptable when the document does not already declare it. A
	// tenant term that overrides a standard name is a declaration, so it drops out here
	// — offering "adopt" for a type the operator already owns would be a no-op button.
	declared := make(map[string]bool, len(terms))
	for _, t := range terms {
		declared[strings.ToLower(t.Name)] = true
	}
	for _, t := range meminject.BaseSeedOntology() {
		if !declared[strings.ToLower(t.Name)] {
			resp.Adoptable = append(resp.Adoptable, t.Name)
		}
	}
	resp.Effective = meminject.EffectiveOntology(terms, confirmed)
	return resp, nil
}

// ontologyDocCtx stamps the tenant identity plus the tenant-scope grants the
// ontology read/write needs.
//
// The grants are stamped SERVER-SIDE, exactly as renderOntology does: this is an
// operator acting on operator-authored config in its own tenant, not an agent
// reaching tenant memory, so it does not require the caller to hold the agent-side
// tenant grants. The tenant itself is principal-derived and never from the wire.
func (s *Server) ontologyDocCtx(ctx context.Context, mi memInject) context.Context {
	dctx := s.docToolCtx(ctx, mi)
	dctx = tools.WithMemoryPolicy(dctx, tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}})
	dctx = tools.WithSqlMemPolicy(dctx, tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}})
	return dctx
}

// ontologyRoot returns the ontology document's (root chunk id, document id, root
// status).
func (s *Server) ontologyRoot(dctx context.Context, doc *builtin.Document) (rootID, docID, status string, err error) {
	req, _ := json.Marshal(map[string]any{
		"op": "get_document", "scope": "tenant", "path": meminject.OntologyPath,
	})
	res, derr := doc.Execute(dctx, req)
	if derr != nil {
		return "", "", "", derr
	}
	if res.IsError {
		return "", "", "", errors.New(res.Text)
	}
	var meta struct {
		DocumentID  string `json:"document_id"`
		RootChunkID string `json:"root_chunk_id"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal([]byte(res.Text), &meta); err != nil {
		return "", "", "", err
	}
	if meta.RootChunkID == "" {
		return "", "", "", errors.New("the ontology document has no root chunk")
	}
	return meta.RootChunkID, meta.DocumentID, meta.Status, nil
}

// ontologyChunkRevision reads a chunk's current revision for the
// optimistic-concurrency guard on update_chunk.
func (s *Server) ontologyChunkRevision(dctx context.Context, doc *builtin.Document, chunkID string) (int, error) {
	req, _ := json.Marshal(map[string]any{
		"op": "get_chunk", "scope": "tenant", "id": chunkID,
	})
	res, err := doc.Execute(dctx, req)
	if err != nil {
		return 0, err
	}
	if res.IsError {
		return 0, errors.New(res.Text)
	}
	var chunk struct {
		Revision int `json:"revision"`
	}
	if err := json.Unmarshal([]byte(res.Text), &chunk); err != nil {
		return 0, err
	}
	return chunk.Revision, nil
}
