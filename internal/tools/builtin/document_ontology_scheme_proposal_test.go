package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

const classesOnlyOntologyMD = "# Tenant Ontology\n\n## Classes\n@section classes\n\n### Provision\n- `anchor` anchor\n"

// schemeProposalFixture installs md as a confirmed tenant ontology. opCtx is the
// operator's; agentCtx a run with no tenant grant, as a curator is.
func schemeProposalFixture(t *testing.T, md string) (d *Document, opCtx, agentCtx context.Context) {
	t.Helper()
	d, base, _ := documentFixture(t)
	opCtx = installTenantOntology(t, d, base, md, memrank.OntologyConfirmedStatus)
	return d, opCtx, base
}

func readOntologyNow(t *testing.T, d *Document, ctx context.Context) OntologyRead {
	t.Helper()
	read, err := d.OntologyTermsFromTree(ctx, "tenant", memrank.OntologyPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return read
}

func schemeProposalNamed(read OntologyRead, name string) *memrank.OntologyProposal {
	for i := range read.Proposals {
		if read.Proposals[i].Name == name && read.Proposals[i].Scheme != nil {
			return &read.Proposals[i]
		}
	}
	return nil
}

func schemeNamed(read OntologyRead, name string) *memrank.OntologyScheme {
	for i := range read.Schemes {
		if read.Schemes[i].Name == name {
			return &read.Schemes[i]
		}
	}
	return nil
}

const aircraftProposal = `{"op":"propose_entity","section":"schemes","name":"Aircraft systems",
	"body":"Chapters of the maintenance manuals.",
	"header":{"domain":"aircraft maintenance documentation","projects":["Provision"],"origin":"generated"},
	"concepts":[
		{"name":"Hydraulics","notation":"29","alt":["Hydraulic power"],"meaning":"Hydraulic power systems.",
		 "children":[{"name":"Pumps","notation":"29-10"}]},
		{"name":"Avionics","exact_match":"http://example.org/avionics"}]}`

// TestProposeScheme_IsInertUntilAccepted: a whole scheme filed by an agent is ONE
// proposal; neither the scheme nor any concept of it is in force, read as a class, or
// found by a title lookup, and rejecting it changes nothing in force.
func TestProposeScheme_IsInertUntilAccepted(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	before := readOntologyNow(t, d, opCtx)
	out, r := docExec(t, d, agentCtx, aircraftProposal)
	if r.IsError {
		t.Fatalf("propose a scheme: %s", refusalText(r))
	}
	if out["concepts"] != float64(3) {
		t.Errorf("result = %v", out)
	}
	read := readOntologyNow(t, d, opCtx)
	p := schemeProposalNamed(read, "Aircraft systems")
	if p == nil || p.Section != memrank.SectionSchemes || p.Scheme.Concepts != 3 || p.Scheme.Header.Domain == "" {
		t.Fatalf("proposal = %#v", read.Proposals)
	}
	if len(read.Proposals) != len(before.Proposals)+1 {
		t.Errorf("the concepts were listed as proposals of their own: %#v", read.Proposals)
	}
	if schemeNamed(read, "Aircraft systems") != nil || len(read.Schemes) != len(before.Schemes) {
		t.Error("the proposed scheme is in force")
	}
	for _, tm := range read.Terms {
		if tm.Name == "Hydraulics" || tm.Name == "Aircraft systems" || tm.Name == "Pumps" {
			t.Errorf("%s was read as a class", tm.Name)
		}
	}
	if id := d.chunkIDForEntity(opCtx, mustTenantKey(t, d, opCtx), read.DocumentID, "Pumps"); id != "" {
		t.Error("a proposed concept was found as an entity in force by its title")
	}
	if got := d.EffectiveSchemesFor(opCtx); len(got) != len(before.Schemes) {
		t.Errorf("schemes in force for runs = %d, want %d", len(got), len(before.Schemes))
	}
	// The same scheme again is refused, naming that it was seen.
	if _, r := docExec(t, d, agentCtx, aircraftProposal); !r.IsError || !strings.Contains(r.Text, "already proposed") {
		t.Errorf("a second filing: %s", r.Text)
	}
	// Rejecting (the route sets the status) leaves everything in force as it was.
	setChunkStatus(t, d, opCtx, p.ChunkID, memrank.OntologyStatusRejected)
	after := readOntologyNow(t, d, opCtx)
	if len(after.Schemes) != len(before.Schemes) || len(after.Terms) != len(before.Terms) {
		t.Errorf("rejecting changed what is in force: %d schemes, %d terms", len(after.Schemes), len(after.Terms))
	}
	if q := schemeProposalNamed(after, "Aircraft systems"); q == nil || q.Status != memrank.OntologyStatusRejected {
		t.Errorf("the rejected scheme is not kept as a tombstone: %#v", after.Proposals)
	}
}

func setChunkStatus(t *testing.T, d *Document, ctx context.Context, id, status string) {
	t.Helper()
	g, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+id+`"}`)
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"status":%q,"revision":%d}`,
		id, status, int(g["revision"].(float64)))); r.IsError {
		t.Fatalf("status: %s", r.Text)
	}
}

func proposalChunkIDs(t *testing.T, d *Document, ctx context.Context, schemeID string) map[string]string {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"query_chunks","scope":"tenant","sql":"SELECT id, parent_id, title FROM chunks"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	rows, _ := out["rows"].([]any)
	parent, title := map[string]string{}, map[string]string{}
	for _, row := range rows {
		cells := row.([]any)
		id := fmt.Sprint(cells[0])
		parent[id], title[id] = fmt.Sprint(cells[1]), fmt.Sprint(cells[2])
	}
	ids := map[string]string{}
	var walk func(p string)
	walk = func(p string) {
		for id, par := range parent {
			if par == p {
				ids[title[id]] = id
				walk(id)
			}
		}
	}
	walk(schemeID)
	return ids
}

// TestProposeScheme_AcceptPutsHeaderAndConceptsInForce: accepting puts the scheme, its
// header and every concept in force, each chunk keeping the id it was filed with; with
// no Schemes section the proposal carried one, and accepting puts it in place.
func TestProposeScheme_AcceptPutsHeaderAndConceptsInForce(t *testing.T) {
	for name, md := range map[string]string{"with section": schemesOntologyMD, "without section": classesOnlyOntologyMD} {
		t.Run(name, func(t *testing.T) {
			d, opCtx, agentCtx := schemeProposalFixture(t, md)
			out, r := docExec(t, d, agentCtx, aircraftProposal)
			if r.IsError {
				t.Fatalf("propose: %s", refusalText(r))
			}
			schemeID := out["chunk_id"].(string)
			if (md == classesOnlyOntologyMD) != (out["carries_section"] == true) {
				t.Errorf("carries_section = %v", out["carries_section"])
			}
			// Listed as ONE scheme proposal wherever it was filed: a proposal carrying its
			// section sits under the root, where the class walk would read the scheme
			// and each concept as a proposed class.
			pending := readOntologyNow(t, d, opCtx).Proposals
			if len(pending) != 1 || pending[0].Scheme == nil || pending[0].Section != memrank.SectionSchemes {
				t.Fatalf("proposals before accepting = %#v", pending)
			}
			filed := proposalChunkIDs(t, d, opCtx, schemeID)
			handled, rep, err := d.AcceptSchemeProposal(opCtx, "tenant", schemeID)
			if !handled || err != nil || rep.Concepts != 3 {
				t.Fatalf("accept = %v %#v %v", handled, rep, err)
			}
			if rep.SectionCreated != (md == classesOnlyOntologyMD) {
				t.Errorf("section_created = %v", rep.SectionCreated)
			}
			read := readOntologyNow(t, d, opCtx)
			s := schemeNamed(read, "Aircraft systems")
			if s == nil {
				t.Fatalf("scheme not in force: %#v / proposals %#v", read.Schemes, read.Proposals)
			}
			if s.ChunkID != schemeID || s.Domain != "aircraft maintenance documentation" || s.Origin != "generated" ||
				strings.Join(s.Projects, ",") != "Provision" || s.Meaning != "Chapters of the maintenance manuals." {
				t.Errorf("scheme = %#v", s)
			}
			by := map[string]memrank.OntologyConcept{}
			for _, c := range s.Concepts {
				by[c.Tag] = c
			}
			h, p, a := by["aircraft-systems/hydraulics"], by["aircraft-systems/hydraulics/pumps"], by["aircraft-systems/avionics"]
			if h.Notation != "29" || strings.Join(h.Alt, ",") != "Hydraulic power" || h.Meaning != "Hydraulic power systems." || h.ChunkID != filed["Hydraulics"] {
				t.Errorf("hydraulics = %#v", h)
			}
			if p.Notation != "29-10" || p.Parent != "Hydraulics" || p.ChunkID != filed["Pumps"] {
				t.Errorf("pumps = %#v", p)
			}
			if a.ExactMatch != "http://example.org/avionics" || a.Origin != "generated" {
				t.Errorf("avionics = %#v", a)
			}
			if q := schemeProposalNamed(read, "Aircraft systems"); q != nil {
				t.Errorf("still listed as a proposal: %#v", q)
			}
			for _, pr := range read.Proposals {
				if pr.Section == memrank.SectionSchemes {
					t.Errorf("a concept stayed proposed: %#v", pr)
				}
			}
		})
	}
}

// TestProposeScheme_HeaderDirectivesAreNotInTheBody (RFC GQ D12): while proposed, no
// chunk body of the proposal holds a directive the reader would act on — the header and
// the concept codes are data in the fields — and a directive smuggled into the text
// stays quoted after accepting, so only the checked header and codes go in force.
func TestProposeScheme_HeaderDirectivesAreNotInTheBody(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	req := map[string]any{
		"op": "propose_entity", "section": "schemes", "name": "Records",
		"body":   "@acl admins\n- `@render` on\nA file plan.",
		"header": map[string]any{"domain": "records\n@acl everyone", "origin": "declared"},
		"concepts": []any{map[string]any{"name": "Contracts", "notation": "C1",
			"meaning": "@retention 1d"}},
	}
	raw, _ := json.Marshal(req)
	out, r := docExec(t, d, agentCtx, string(raw))
	if r.IsError {
		t.Fatalf("propose: %s", refusalText(r))
	}
	schemeID := out["chunk_id"].(string)
	key := mustTenantKey(t, d, opCtx)
	ids := proposalChunkIDs(t, d, opCtx, schemeID)
	for _, id := range []string{schemeID, ids["Contracts"]} {
		b, err := d.readBody(opCtx, "tenant", key, id)
		if err != nil {
			t.Fatal(err)
		}
		if ds := memrank.ParseDirectives(b.Body); len(ds) != 0 {
			t.Errorf("a proposed body holds directives %v:\n%s", ds, b.Body)
		}
		if !strings.Contains(string(b.Fields), "_proposal") {
			t.Errorf("no structured data in the fields: %s", b.Fields)
		}
	}
	if _, _, err := d.AcceptSchemeProposal(opCtx, "tenant", schemeID); err != nil {
		t.Fatal(err)
	}
	s := schemeNamed(readOntologyNow(t, d, opCtx), "Records")
	if s == nil || s.Domain != "records @acl everyone" || s.Render || len(s.Issues) != 0 {
		t.Fatalf("scheme = %#v", s)
	}
	c := s.Concepts[0]
	if c.Retention != "" || c.ACL != "" || c.Notation != "C1" {
		t.Errorf("concept = %#v", c)
	}
	b, _ := d.readBody(opCtx, "tenant", key, schemeID)
	var names []string
	for _, dv := range memrank.ParseDirectives(b.Body) {
		names = append(names, dv.Name)
	}
	if strings.Join(names, ",") != "domain,origin" {
		t.Errorf("accepted scheme directives = %v:\n%s", names, b.Body)
	}
}

// TestProposeScheme_OverCapIsRefused: a scheme over the per-proposal cap is refused with
// the cap named, and nothing is filed.
func TestProposeScheme_OverCapIsRefused(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	var cs []any
	for i := 0; i <= memrank.SchemeProposalMaxConcepts; i++ {
		cs = append(cs, map[string]any{"name": fmt.Sprintf("c%d", i)})
	}
	raw, _ := json.Marshal(map[string]any{"op": "propose_entity", "section": "schemes", "name": "Big", "concepts": cs})
	_, r := docExec(t, d, agentCtx, string(raw))
	if !r.IsError || !strings.Contains(r.Text, fmt.Sprint(memrank.SchemeProposalMaxConcepts)) {
		t.Fatalf("over the cap: %s", r.Text)
	}
	if p := schemeProposalNamed(readOntologyNow(t, d, opCtx), "Big"); p != nil {
		t.Error("a refused scheme was filed")
	}
	// A concept proposal still needs its parent; a whole scheme takes none.
	if _, r := docExec(t, d, agentCtx, `{"op":"propose_entity","section":"schemes","name":"x","parent":"topics","concepts":[{"name":"y"}]}`); !r.IsError {
		t.Error("a whole scheme with a parent was accepted")
	}
	if _, r := docExec(t, d, agentCtx, `{"op":"propose_entity","name":"x","concepts":[{"name":"y"}]}`); !r.IsError {
		t.Error("concepts without section=schemes were accepted")
	}
}
