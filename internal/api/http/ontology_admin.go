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
	"log/slog"
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

	// --- RFC FO P1a. All omitempty, and the class/usage reports are filled only for a
	// SECTIONED document, so a section-less ontology's response is unchanged.

	// Sections lists the section kinds the document declares, in document order.
	Sections []string `json:"sections,omitempty"`
	// DocumentClasses is the effective document-class tree (the seed Document unless
	// the tenant overrides it). Kept apart from Effective, which is the entity ontology a
	// run's prompt is built from — document classes never reach the prompt.
	DocumentClasses []meminject.OntologyTerm `json:"document_classes,omitempty"`
	// Grammars are the anchor/citation grammars the classes section declares; one with
	// an issue is disabled.
	Grammars []meminject.OntologyGrammar `json:"grammars,omitempty"`
	// OntologyVersion is the root chunk's revision — what facts written now are stamped
	// with. FactVersions counts the tenant scope's facts by the version they carry.
	OntologyVersion int                    `json:"ontology_version,omitempty"`
	FactVersions    []builtin.VersionCount `json:"fact_versions,omitempty"`
	// UndeclaredTypes are chunks.type values in use in the TENANT scope that no class
	// declares (structural types excluded), with counts — what an operator would declare
	// or map with @renamed_from. Tenant scope only: user scopes are per person and are
	// not swept from a settings page.
	UndeclaredTypes []builtin.TypeCount `json:"undeclared_types,omitempty"`

	// --- RFC FO P1b. Filled only when the document declares a Relations section.

	// Relations is the tenant's own relation kinds as written, each with its issues
	// (an entry named after a reserved kind is marked: its directives do nothing).
	Relations []meminject.OntologyRelation `json:"relations,omitempty"`
	// EffectiveRelations is the vocabulary in force — reserved ⊕ seed ⊕ tenant — and
	// empty while the document is draft, which is the gate made visible.
	EffectiveRelations []meminject.OntologyRelation `json:"effective_relations,omitempty"`
	// AdoptableRelations names the seed kinds the document does not declare — what the
	// adopt action can copy in, and so list to agents.
	AdoptableRelations []string `json:"adoptable_relations,omitempty"`
	// StatementVersions counts the tenant scope's relation statements by the ontology
	// version they were written under; UndatedStatements counts live statements of a
	// @dated kind that carry no valid_at.
	StatementVersions []builtin.VersionCount `json:"statement_versions,omitempty"`
	UndatedStatements int                    `json:"undated_statements,omitempty"`

	// --- RFC FO P1c. Filled only when the document declares a Schemes section.

	// Schemes are the concept schemes as written, each concept with its mirror tag and,
	// on a file plan, its retention and ACL (shown, not enforced yet).
	Schemes []meminject.OntologyScheme `json:"schemes,omitempty"`
	// SchemeMirrorStale lists the concepts whose chunk does not carry its mirror tag
	// (or still carries an old one) — what confirming again would fix.
	SchemeMirrorStale []string `json:"scheme_mirror_stale,omitempty"`
	// SchemeMirror reports what the mirror changed, on the confirm that ran it.
	SchemeMirror *builtin.SchemeMirrorReport `json:"scheme_mirror,omitempty"`

	// --- RFC FO P1c-2. Filled only when the document declares an Instances section.

	// Instances is each instance's adoption state as a confirm would leave it (a dry
	// run): pending, unchanged, linked, renamed or error — with the registry key — and
	// the subjects adopted from instances no longer declared (kept, never erased).
	Instances *builtin.InstanceReport `json:"instances,omitempty"`
	// InstanceAdoption reports what adoption did, on the confirm that ran it.
	InstanceAdoption *builtin.InstanceReport `json:"instance_adoption,omitempty"`
}

// ontologyTenant is the tenant every ontology route reads and writes: an admin's
// explicit `?tenant=` focus, else the CALLER'S OWN tenant — the one its runs are stamped
// with and its off-run Document writes go to. A tenant operator is pinned to its own.
//
// It is NOT principalTenantScope, which resolves an unfocused admin to "" because it
// serves listings, where "" means "every tenant". The ontology is one tenant's
// document, and "" is a concrete tenant there. For a LOOMCYCLE_AUTH_TOKEN caller —
// principal tenant "default" — that made the page provision and read the ontology under
// "" while runs and the Web UI document browser used "default". SQL Memory maps both to
// the same database, so the page showed the classes' titles, but chunk bodies and Path
// names are keyed by the raw tenant: it read every class with no fields, runs read a
// second document, and an operator's edits and confirm never reached a run.
func (s *Server) ontologyTenant(r *http.Request) string {
	return s.adminFocusedTenant(r.Context(), r.URL.Query().Get("tenant"))
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
	// focus operates on its own tenant — the same one its own runs use.
	tenant := s.ontologyTenant(r)

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

	// The gate is the ROOT chunk's status, so the write needs the root's id and its
	// current revision. Both are read here rather than passed on the wire: the
	// caller is toggling one bit and should not have to know the document's
	// internal shape, and a revision that came from the client could be stale in a
	// way that silently targeted the wrong chunk.
	rootID, _, current, err := s.ontologyRoot(dctx, doc)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable", err.Error())
		return
	}

	// NO WRITE WHEN NOTHING CHANGES. Confirming an already-confirmed ontology is how an
	// operator re-applies its concept tags and instances; rewriting the same status would
	// bump the root's revision — the ontology_version every fact and statement is
	// stamped with — and each re-apply would mark everything as written under an older
	// vocabulary that never changed.
	if !strings.EqualFold(strings.TrimSpace(current), status) {
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
	}

	// Confirming applies the Schemes section: every concept chunk gets its mirror tag
	// (RFC FO P1c). Confirming an already-confirmed ontology is how an operator
	// re-applies after editing a scheme. A mirror fault does not undo the confirm — the
	// flip is what the operator asked for — and is reported as a note.
	var mirror *builtin.SchemeMirrorReport
	var mirrorNote string
	var adoption *builtin.InstanceReport
	if status == meminject.OntologyConfirmedStatus {
		if rep, merr := doc.MirrorSchemeTags(dctx, false); merr != nil {
			slog.Warn("ontology: scheme tag mirror failed", "tenant", tenant, "err", merr)
			mirrorNote = "The concept tags could not be applied this time; confirm again to retry."
		} else if rep.Concepts > 0 {
			mirror = &rep
		}
		// Instances become real here and only here (RFC FO §6): an explicit confirm, never
		// boot or an edit. Same failure posture as the mirror.
		if rep, aerr := doc.AdoptInstances(dctx, false); aerr != nil {
			slog.Warn("ontology: instance adoption failed", "tenant", tenant, "err", aerr)
			mirrorNote = strings.TrimSpace(mirrorNote + " The instances could not be adopted this time; confirm again to retry.")
		} else if len(rep.Instances) > 0 || len(rep.Orphaned) > 0 {
			adoption = &rep
		}
	}

	// Re-read rather than synthesizing the new state: the response then shows the
	// EFFECTIVE ontology after the flip, which is what the operator wanted to know.
	resp, err := s.ontologyState(r.Context(), tenant)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "ontology_unavailable", err.Error())
		return
	}
	resp.SchemeMirror = mirror
	resp.InstanceAdoption = adoption
	if mirrorNote != "" {
		resp.Notes = append(resp.Notes, mirrorNote)
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
	sectioned := false
	var relRead builtin.OntologyRead
	hasRelations := false
	if read, rerr := doc.OntologyTermsFromTree(dctx, "tenant", meminject.OntologyPath); rerr == nil {
		resp.Proposals = read.Proposals
		resp.Sections, resp.Grammars, resp.OntologyVersion = read.Sections, read.Grammars, read.RootRevision
		sectioned = len(read.Sections) > 0
		relRead = read
		_, hasRelations = read.SectionChunks[meminject.SectionRelations]
		if _, hasInstances := read.SectionChunks[meminject.SectionInstances]; hasInstances {
			if dry, derr := doc.AdoptInstances(dctx, true); derr == nil {
				resp.Instances = &dry
			}
		}
		if _, hasSchemes := read.SectionChunks[meminject.SectionSchemes]; hasSchemes {
			resp.Schemes = read.Schemes
			if dry, derr := doc.MirrorSchemeTags(dctx, true); derr == nil {
				resp.SchemeMirrorStale = dry.Stale
			}
		}
		// Document-level advisories (a duplicate or unknown section) join the reader
		// notes the panel already renders.
		notes = append(notes, read.Issues...)
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
	// Effective is what an agent is told about, so document classes — which never
	// reach a prompt — are reported in DocumentClasses instead.
	resp.Effective = meminject.EntityTermsOnly(meminject.EffectiveOntology(terms, confirmed))
	if sectioned {
		classes := meminject.EffectiveClasses(terms, confirmed)
		declaredClass := make(map[string]bool, len(classes))
		for _, c := range classes {
			declaredClass[strings.ToLower(c.Name)] = true
			if c.IsDocumentClass {
				resp.DocumentClasses = append(resp.DocumentClasses, c)
			}
		}
		if usage, uerr := doc.OntologyUsageIn(dctx, "tenant"); uerr == nil {
			resp.FactVersions = usage.Versions
			if hasRelations {
				resp.StatementVersions = usage.StatementVersions
			}
			for _, tc := range usage.Types {
				if !declaredClass[strings.ToLower(tc.Type)] && !meminject.IsReservedStructuralType(tc.Type) {
					resp.UndeclaredTypes = append(resp.UndeclaredTypes, tc)
				}
			}
		}
	}
	if hasRelations {
		resp.Relations = meminject.MarkReservedEntries(relRead.Relations)
		resp.EffectiveRelations = meminject.EffectiveRelations(relRead.Relations, true, confirmed)
		declaredRel := map[string]bool{}
		for _, r := range relRead.Relations {
			declaredRel[r.Name] = true
		}
		for _, r := range meminject.SeedRelations() {
			if !declaredRel[r.Name] {
				resp.AdoptableRelations = append(resp.AdoptableRelations, r.Name)
			}
		}
		var dated []string
		for _, r := range resp.EffectiveRelations {
			if r.Dated {
				dated = append(dated, r.Name)
			}
		}
		if n, uerr := doc.UndatedStatementsIn(dctx, "tenant", dated); uerr == nil {
			resp.UndatedStatements = n
		}
	}
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
	// RFC CA: these routes ARE the operator surface — the confirm toggle, accept/reject,
	// adopt. Marked so the ontology guard admits them while refusing an agent's tool call
	// (which is never stamped, because there is no wire field for it).
	dctx := tools.WithSubstrateOperator(s.docToolCtx(ctx, mi))
	dctx = withTenantDocGrants(dctx)
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
