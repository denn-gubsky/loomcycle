package builtin

// document_subject_proposal.go — RFC CV P4: a subject the tenant does not know yet
// is PROPOSED, never minted.
//
// The curator gate (decision 6) refuses to create a tenant entity from a name a model
// read in one user's transcript, and leaves the fact in the caller's own scope. That
// was the whole gate: correct, and a dead end — the pass named the waiting subject in
// a report and nothing else ever happened. This is the other half, so an operator has
// something to act on and the loop closes.
//
// IT REUSES THE ONTOLOGY'S INERT-CANDIDATE SURFACE rather than inventing a second
// approval queue, which is what decision 6 said to do. A proposal is a chunk in the
// tenant ontology document with status `proposed`: it is not in force, no run is told
// about it, and rejecting it leaves the tombstone that stops the nagging.
//
// ⚠️ AND IT MUST NOT CARRY A SIDECAR. The curator gate asks "does the tenant know this
// subject" by joining tenant chunks to `chunk_memory_meta` on the title. A proposal
// written with entity metadata would answer YES to that question — so proposing the
// subject would mint it, which is precisely what the gate refused to do. The proposal
// therefore carries its identity in the chunk's FIELDS, which ride in the body row,
// and a test pins that the gate still says "unknown" while a proposal is pending.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// SubjectProposalChunkType marks an ontology chunk as a proposed SUBJECT rather than
// a proposed type. Stored in the chunk's `type` column, which the ontology reader
// ignores, so a pending proposal behaves exactly as one did before this existed.
//
// The accept path branches on it — on the stored type, never on the content of the
// body, because the same object written two ways is how a classifier starts lying.
const SubjectProposalChunkType = "subject-proposal"

// subjectProposalFields is what a proposal carries so the accept path can mint the
// entity WITHOUT re-deriving its identity.
//
// THE PROPOSER OWNS THE KEY, and that is the point of writing it down. The natural key
// and the path slug come out of the consolidator's own `slug`/`entityKey` — an
// identity function that exists once, in the bundle. Re-implementing it in Go so the
// accept handler could compute the same key would put a second implementation of an
// identity behind a boundary where a divergence produces two subject nodes for one
// subject: the exact duplication this phase exists to remove. So the key travels as
// data and is used verbatim.
// SubjectProposalFields is exported because the operator plane adopts a proposal and
// must pass back exactly what was recorded — see the note above on why the identity
// travels as data rather than being re-derived.
type SubjectProposalFields struct {
	Subject    string `json:"subject"`
	EntityType string `json:"entity_type"`
	NaturalKey string `json:"natural_key"`
	PathSlug   string `json:"path_slug"`
}

// proposeSubject files an inert subject proposal against the tenant ontology.
//
// NO TENANT GRANT REQUIRED, the same carve-out propose_entity takes and for the same
// reason: a proposal cannot change what any run is told. Requiring the grant would mean
// only an agent that could already mint tenant entities could suggest one, which
// inverts the gate.
func (d *Document) proposeSubject(ctx context.Context, in docInput) (tools.Result, error) {
	subject := strings.TrimSpace(in.Subject)
	if subject == "" {
		subject = strings.TrimSpace(in.Title)
	}
	if subject == "" {
		return errResult("propose_subject: subject is required — it is the name an operator adopts"), nil
	}
	naturalKey := strings.TrimSpace(in.NaturalKey)
	if naturalKey == "" {
		return errResult("propose_subject: natural_key is required — the proposer owns the " +
			"subject's identity, and the adopt path mints the entity under exactly this key " +
			"so the two can never disagree"), nil
	}
	if len(in.Body) > proposalBodyMax {
		return errResult(fmt.Sprintf("propose_subject: body is %d bytes, over the %d-byte limit",
			len(in.Body), proposalBodyMax)), nil
	}

	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return errResult("propose_subject: " + err.Error()), nil
	}
	read, err := d.ontologyForKey(ctx, key, mscope, memory.OntologyPath)
	if err != nil {
		return errResult("propose_subject: " + err.Error()), nil
	}
	if read.DocumentID == "" || read.RootChunkID == "" {
		return errResult("propose_subject: this tenant has no ontology document yet — an " +
			"operator opens Settings → Ontology once to create it"), nil
	}
	// Already filed, in either state. An accepted one is gone (the entity replaced it),
	// so reaching here means pending or rejected — and a rejected twin means the
	// operator already said no, which is what the tombstone is for.
	for _, p := range read.Proposals {
		if strings.EqualFold(p.Name, subject) {
			return jsonResult(map[string]any{
				"proposed": subject, "already": p.Status,
				"note": "an operator has already seen this subject",
			})
		}
	}

	fields, _ := json.Marshal(SubjectProposalFields{
		Subject: subject, EntityType: entityTypeOfKey(naturalKey),
		NaturalKey: naturalKey, PathSlug: strings.TrimSpace(in.Path),
	})
	res, err := d.createChunk(ctx, key, mscope, docInput{
		Scope: "tenant", DocumentID: read.DocumentID, ParentID: read.RootChunkID,
		Title: subject, Status: memory.OntologyStatusProposed,
		Type: SubjectProposalChunkType, Body: in.Body, Fields: fields,
	})
	if err != nil {
		return errResult("propose_subject: " + err.Error()), nil
	}
	if res.IsError {
		return res, nil
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(res.Text), &out)
	return jsonResult(map[string]any{
		"proposed": subject, "chunk_id": out["id"], "natural_key": naturalKey,
		"note": "Filed as a suggestion, not in force. Facts about this subject stay in the " +
			"scope that learned them until an operator adopts it in Settings → Ontology.",
	})
}

// entityTypeOfKey splits `person:ada-lovelace` into its type half. Empty when the key
// carries no type, which the adopt path treats as a subject with no declared kind.
func entityTypeOfKey(naturalKey string) string {
	if i := strings.Index(naturalKey, ":"); i > 0 {
		return naturalKey[:i]
	}
	return ""
}

// AdoptSubjectProposal mints the tenant entity a proposal stands for, and returns the
// document it created. The proposal chunk is the caller's to remove afterwards.
//
// ⚠️ THE SHAPE MUST MATCH WHAT THE CONSOLIDATOR WOULD HAVE WRITTEN — a `/facts/<slug>`
// document whose ROOT chunk is the entity node, carrying type + subject + natural_key.
// A bare chunk would look adopted and then collide: `natural_key` is UNIQUE per scope,
// so the next pass's create_document for the same subject fails, the subject resolves
// to nothing, and its facts land with no edge to it. That failure shipped once already.
func (d *Document) AdoptSubjectProposal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, f SubjectProposalFields, evidence string) (string, error) {
	slug := strings.TrimSpace(f.PathSlug)
	if slug == "" {
		slug = strings.TrimPrefix(f.NaturalKey, entityTypeOfKey(f.NaturalKey)+":")
	}
	if slug == "" {
		return "", fmt.Errorf("the proposal records no path for the subject")
	}
	// THE BODY CARRIES THE CONSEQUENCE, because the consequence is otherwise invisible.
	// Adoption shares this subject from now ON; it does not reach back for the facts
	// already learned about it, which stay in the scope that learned them. So a tenant
	// dossier adopted today can sit empty next to a user scope that knows plenty, and
	// the only place anyone would look to understand that is here.
	body := "Adopted from a proposal filed by the memory consolidator.\n\n" +
		"Facts learned about this subject BEFORE adoption stay in the scope that learned " +
		"them — adoption shares what is learned from now on, and never republishes " +
		"someone's history as a side effect of an operator accepting a name.\n\n" + evidence
	res, err := d.createDocument(ctx, key, mscope, docInput{
		Scope: "tenant", Title: f.Subject, Path: "/facts/" + slug,
		Type: f.EntityType, Subject: f.Subject, NaturalKey: f.NaturalKey, Body: body,
	})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("%s", res.Text)
	}
	var out struct {
		DocumentID  string `json:"document_id"`
		RootChunkID string `json:"root_chunk_id"`
	}
	_ = json.Unmarshal([]byte(res.Text), &out)
	// create_document writes an EMPTY root body and ignores the one it was given, so the
	// note has to be written onto the root afterwards. Best-effort: the entity existing
	// is what the operator asked for, and a dossier with no explanatory note is worse
	// documentation, not a failed adoption.
	if out.RootChunkID != "" {
		_ = d.writeBody(ctx, mscope, key, out.RootChunkID, f.EntityType, body, nil)
	}
	return out.DocumentID, nil
}

// SubjectProposal reports whether an ontology chunk is a subject proposal, and returns
// what it recorded. Reads the chunk's stored TYPE, never its prose.
func (d *Document) SubjectProposal(ctx context.Context, scope, chunkID string) (bool, SubjectProposalFields, string) {
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return false, SubjectProposalFields{}, ""
	}
	res, err := d.query(ctx, key, `SELECT coalesce(type, '') FROM chunks WHERE id = ?`, chunkID)
	if err != nil || len(res.Rows) == 0 || asStr(res.Rows[0][0]) != SubjectProposalChunkType {
		return false, SubjectProposalFields{}, ""
	}
	body, _ := d.readBody(ctx, mscope, key.ScopeID, chunkID)
	var f SubjectProposalFields
	if len(body.Fields) > 0 {
		_ = json.Unmarshal(body.Fields, &f)
	}
	return true, f, body.Body
}

// AdoptSubjectProposalInScope is AdoptSubjectProposal with the scope resolved by name,
// for the operator plane which addresses scopes as strings.
func (d *Document) AdoptSubjectProposalInScope(ctx context.Context, scope string, f SubjectProposalFields, evidence string) (string, error) {
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return "", err
	}
	return d.AdoptSubjectProposal(ctx, key, mscope, f, evidence)
}

// DeleteOntologyChunk removes one chunk from the ontology document. Used to retire a
// suggestion once the thing it suggested exists.
func (d *Document) DeleteOntologyChunk(ctx context.Context, scope, chunkID string) error {
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return err
	}
	_, err = d.cascadeDeleteChunks(ctx, key, mscope, []string{chunkID})
	return err
}
