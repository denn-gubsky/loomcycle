package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// createOntologyChunk writes one chunk as the operator and returns its id.
func createOntologyChunk(t *testing.T, d *Document, ctx context.Context, docID string, req map[string]any) string {
	t.Helper()
	req["op"], req["scope"], req["document_id"] = "create_chunk", "tenant", docID
	raw, _ := json.Marshal(req)
	out, r := docExec(t, d, ctx, string(raw))
	if r.IsError {
		t.Fatalf("create_chunk %v: %s", req["title"], refusalText(r))
	}
	return out["id"].(string)
}

func fileAircraft(t *testing.T, d *Document, agentCtx context.Context) string {
	t.Helper()
	out, r := docExec(t, d, agentCtx, aircraftProposal)
	if r.IsError {
		t.Fatalf("propose: %s", refusalText(r))
	}
	return out["chunk_id"].(string)
}

// TestAcceptSchemeProposal_RefusesSchemeDeclaredSinceFiling: the "already declared"
// check runs again at accept, so a scheme declared by hand between filing and review
// does not end up in force twice with the same mirror tags.
func TestAcceptSchemeProposal_RefusesSchemeDeclaredSinceFiling(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	schemeID := fileAircraft(t, d, agentCtx)
	read := readOntologyNow(t, d, opCtx)
	createOntologyChunk(t, d, opCtx, read.DocumentID, map[string]any{
		"parent_id": read.SectionChunks[memrank.SectionSchemes], "title": "Aircraft Systems",
		"body": "Declared by hand.\n\n"})
	handled, _, err := d.AcceptSchemeProposal(opCtx, "tenant", schemeID)
	if !handled || !errors.Is(err, ErrSchemeProposalInvalid) {
		t.Fatalf("accept = %v, %v; want a refusal", handled, err)
	}
	after := readOntologyNow(t, d, opCtx)
	n := 0
	for _, s := range after.Schemes {
		if memrank.MirrorSegment(s.Name) == "aircraft-systems" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d schemes in force with the segment aircraft-systems, want 1", n)
	}
	if p := schemeProposalNamed(after, "Aircraft systems"); p == nil || p.Status != memrank.OntologyStatusProposed {
		t.Errorf("the refused proposal is no longer proposed: %#v", p)
	}
}

// TestAcceptSchemeProposal_RefusesLeftoverConceptWhoseTagIsInForce: a concept left over
// from a scheme accepted part-way is checked against the concepts in force beside it,
// so accepting it cannot put a second concept with the same tag in force; one with a
// fresh tag still accepts.
func TestAcceptSchemeProposal_RefusesLeftoverConceptWhoseTagIsInForce(t *testing.T) {
	d, opCtx, _ := schemeProposalFixture(t, schemesOntologyMD)
	read := readOntologyNow(t, d, opCtx)
	topics := schemeNamed(read, "topics")
	if topics == nil {
		t.Fatalf("schemes = %#v", read.Schemes)
	}
	var rm string
	for _, c := range topics.Concepts {
		if c.Name == "Risk Management" {
			rm = c.ChunkID
		}
	}
	mark := json.RawMessage(`{"concept_proposal":{"notation":"X1"}}`)
	dup := createOntologyChunk(t, d, opCtx, read.DocumentID, map[string]any{"parent_id": topics.ChunkID,
		"title": "risk management", "status": memrank.OntologyStatusProposed, "fields": mark, "body": "A copy."})
	nested := createOntologyChunk(t, d, opCtx, read.DocumentID, map[string]any{"parent_id": rm,
		"title": "Outsourcing", "status": memrank.OntologyStatusProposed, "fields": mark, "body": "A copy."})
	fresh := createOntologyChunk(t, d, opCtx, read.DocumentID, map[string]any{"parent_id": rm,
		"title": "Vendor risk", "status": memrank.OntologyStatusProposed, "fields": mark, "body": "New."})
	for name, id := range map[string]string{"top level": dup, "nested": nested} {
		handled, _, err := d.AcceptSchemeProposal(opCtx, "tenant", id)
		if !handled || !errors.Is(err, ErrSchemeProposalInvalid) {
			t.Errorf("%s: accept = %v, %v; want a refusal", name, handled, err)
		}
	}
	if handled, rep, err := d.AcceptSchemeProposal(opCtx, "tenant", fresh); !handled || err != nil || rep.Concepts != 1 {
		t.Fatalf("a fresh concept: accept = %v %#v %v", handled, rep, err)
	}
	seen := map[string]int{}
	for _, c := range schemeNamed(readOntologyNow(t, d, opCtx), "topics").Concepts {
		seen[c.Tag]++
	}
	for tag, n := range seen {
		if n > 1 {
			t.Errorf("tag %s is in force %d times", tag, n)
		}
	}
	if c := schemeConcepts(readOntologyNow(t, d, opCtx))["topics/risk-management/vendor-risk"]; c.Notation != "X1" {
		t.Errorf("the fresh concept = %#v", c)
	}
}

// TestAcceptProposalChunk_StaleRevisionIsRefused: the body accept writes was read with
// a revision, and the update is made against THAT revision, so an operator's edit made
// after the read is not overwritten silently.
func TestAcceptProposalChunk_StaleRevisionIsRefused(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	schemeID := fileAircraft(t, d, agentCtx)
	id := proposalChunkIDs(t, d, opCtx, schemeID)["Avionics"]
	key := mustTenantKey(t, d, opCtx)
	g, _ := docExec(t, d, opCtx, `{"op":"get_chunk","scope":"tenant","id":"`+id+`"}`)
	body, err := d.readBody(opCtx, "tenant", key, id)
	if err != nil {
		t.Fatal(err)
	}
	stale := proposalNode{id: id, revision: int(g["revision"].(float64)), body: body}
	if _, r := docExec(t, d, opCtx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"revision":%d,"body":"Edited by the operator."}`,
		id, stale.revision)); r.IsError {
		t.Fatalf("edit: %s", r.Text)
	}
	if err := d.acceptProposalChunk(opCtx, stale, nil, conceptProposalField); err == nil {
		t.Error("an update from a stale read went through")
	}
	if b, _ := d.readBody(opCtx, "tenant", key, id); !strings.Contains(b.Body, "Edited by the operator.") {
		t.Errorf("the operator's edit was overwritten:\n%s", b.Body)
	}
}

// TestAcceptSchemeProposal_ConcurrentAcceptsAddOneSchemesSection: two scheme proposals
// that each carry the Schemes section, accepted at the same time, put ONE section in
// place, not one each.
func TestAcceptSchemeProposal_ConcurrentAcceptsAddOneSchemesSection(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, classesOnlyOntologyMD)
	var ids []string
	for _, name := range []string{"Alpha", "Beta", "Gamma", "Delta"} {
		raw, _ := json.Marshal(map[string]any{"op": "propose_entity", "section": "schemes", "name": name,
			"concepts": []any{map[string]any{"name": name + " one"}}})
		out, r := docExec(t, d, agentCtx, string(raw))
		if r.IsError {
			t.Fatalf("propose %s: %s", name, refusalText(r))
		}
		ids = append(ids, out["chunk_id"].(string))
	}
	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = d.AcceptSchemeProposal(opCtx, "tenant", id)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("accept %d: %v", i, err)
		}
	}
	out, r := docExec(t, d, opCtx, `{"op":"query_chunks","scope":"tenant","sql":"SELECT id FROM chunks WHERE title = 'Schemes'"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	if rows, _ := out["rows"].([]any); len(rows) != 1 {
		t.Errorf("%d Schemes sections, want 1", len(rows))
	}
	if got := len(readOntologyNow(t, d, opCtx).Schemes); got != len(ids) {
		t.Errorf("%d schemes in force, want %d", got, len(ids))
	}
}

// TestProposeScheme_ViewIsWhatAcceptWrites: the reviewer's view lists each concept's
// notation, alternative labels and exact match, and only the concepts accepting puts
// in force — a chunk filed beside the proposal some other way is not counted.
func TestProposeScheme_ViewIsWhatAcceptWrites(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	schemeID := fileAircraft(t, d, agentCtx)
	read := readOntologyNow(t, d, opCtx)
	createOntologyChunk(t, d, opCtx, read.DocumentID, map[string]any{"parent_id": schemeID,
		"title": "Stray", "status": memrank.OntologyStatusProposed, "body": "No mark."})
	v := schemeProposalNamed(readOntologyNow(t, d, opCtx), "Aircraft systems").Scheme
	if v.Concepts != 3 || len(v.Outline) != 3 {
		t.Fatalf("view = %#v", v)
	}
	by := map[string]memrank.OutlineNode{}
	for _, n := range v.Outline {
		by[n.Name] = n
	}
	if h := by["Hydraulics"]; h.Notation != "29" || strings.Join(h.Alt, ",") != "Hydraulic power" || h.Level != 1 {
		t.Errorf("hydraulics = %#v", h)
	}
	if p := by["Pumps"]; p.Notation != "29-10" || p.Level != 2 {
		t.Errorf("pumps = %#v", p)
	}
	if a := by["Avionics"]; a.ExactMatch != "http://example.org/avionics" {
		t.Errorf("avionics = %#v", a)
	}
	handled, rep, err := d.AcceptSchemeProposal(opCtx, "tenant", schemeID)
	if !handled || err != nil || rep.Concepts != v.Concepts {
		t.Fatalf("accept = %v %#v %v; view said %d concepts", handled, rep, err, v.Concepts)
	}
	for _, c := range schemeNamed(readOntologyNow(t, d, opCtx), "Aircraft systems").Concepts {
		n, ok := by[c.Name]
		if !ok || n.Notation != c.Notation || n.ExactMatch != c.ExactMatch || strings.Join(n.Alt, ",") != strings.Join(c.Alt, ",") {
			t.Errorf("in force %#v, the view showed %#v", c, n)
		}
	}
}
