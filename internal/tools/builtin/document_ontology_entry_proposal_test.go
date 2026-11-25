package builtin

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// evidenceChunk writes a tenant-scope chunk with body and returns its id.
func (f changeFixture) evidenceChunk(body string) string {
	f.t.Helper()
	doc := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Evidence " + body[:8]})
	return f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": doc["document_id"],
		"title": "passage", "body": body})["id"].(string)
}

// entryMark is the entry_proposal mark stored on a proposal chunk.
func (f changeFixture) entryMark(id string) (memrank.ProposedEntry, bool) {
	f.t.Helper()
	raw, _ := json.Marshal(f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})["fields"])
	var m memrank.ProposedEntry
	return m, proposalMark(raw, entryProposalField, &m)
}

// listedEntry is the read's listing of proposal id.
func (f changeFixture) listed(id string) *memrank.OntologyProposal {
	f.t.Helper()
	for _, p := range f.read().Proposals {
		if p.ChunkID == id {
			p := p
			return &p
		}
	}
	return nil
}

// TestProposeEntry_IsInertUntilAccepted (RFC GQ P6.3): a class and a relation add filed
// with meaning, directives, evidence and a signal change no read and no
// describe_ontology answer; each is listed as a proposal carrying its data, the plan
// as filed and the run that filed it, and the run is told only the risk.
func TestProposeEntry_IsInertUntilAccepted(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	ev := f.evidenceChunk("The contract binds the vendor to quarterly audits.")
	f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "A typed one", "type": "Contract"})
	before := inForce(f.read())
	_, desc := f.do(f.ctx, map[string]any{"op": "describe_ontology"})

	class := f.mustPropose(map[string]any{"name": "Contract", "parent": "Document", "signal": "undeclared_type",
		"meaning": "A binding agreement between parties.", "body": "Typed on 4 chunks.",
		"evidence": []map[string]any{{"chunk_id": ev, "quote": "binds the vendor"}}})
	rel := f.mustPropose(map[string]any{"name": "approves", "section": "relations", "signal": "undeclared_kind",
		"meaning": "One party approves another's document.", "directives": map[string]any{"domain": "Policy", "range": "Memo"}})

	if got := inForce(f.read()); got != before {
		t.Errorf("an entry proposal altered the read:\nbefore %s\nafter  %s", before, got)
	}
	if _, desc2 := f.do(f.ctx, map[string]any{"op": "describe_ontology"}); desc.Text != desc2.Text {
		t.Errorf("describe_ontology changed:\n%s\n%s", desc.Text, desc2.Text)
	}
	for _, out := range []map[string]any{class, rel} {
		if out["risk"] != "none" {
			t.Errorf("risk = %v, want none", out["risk"])
		}
		if _, leaked := out["plan"]; leaked {
			t.Errorf("the run was given the stored plan: %v", out)
		}
	}
	c := f.listed(class["chunk_id"].(string))
	if c == nil || c.Entry == nil || c.Status != memrank.OntologyStatusProposed {
		t.Fatalf("class proposal listed as %+v", c)
	}
	if c.Entry.Kind != planKindClass || c.Entry.Signal != "undeclared_type" || c.Entry.Meaning != "A binding agreement between parties." ||
		len(c.Entry.Evidence) != 1 || c.Entry.Evidence[0].ChunkID != ev {
		t.Errorf("class entry = %+v", c.Entry)
	}
	if c.Entry.FiledBy == nil || c.Entry.FiledBy.RunID != "run-1" {
		t.Errorf("filed_by = %+v, want run-1", c.Entry.FiledBy)
	}
	if c.Entry.Plan == nil || c.Entry.Plan.Risk != "none" || len(c.Entry.Plan.Fingerprint) != 64 || c.Entry.Plan.WouldDeclare["typed_documents"] != 1 {
		t.Errorf("plan as filed = %+v", c.Entry.Plan)
	}
	r := f.listed(rel["chunk_id"].(string))
	want := []memrank.ProposedDirective{{Name: "domain", Value: "Policy"}, {Name: "range", Value: "Memo"}}
	if r == nil || r.Entry == nil || r.Entry.Kind != planKindRelation || !reflect.DeepEqual(r.Entry.Directives, want) {
		t.Errorf("relation proposal listed as %+v", r)
	}
	if _, ok := r.Entry.Plan.WouldDeclare["statements_of_kind"]; !ok {
		t.Errorf("relation plan = %+v, want statements_of_kind", r.Entry.Plan)
	}
}

// TestProposeEntry_DirectivesAreNotInTheBody (D12): the directives travel in the mark;
// no directive line of the meaning or of the evidence is live in the proposal's body.
func TestProposeEntry_DirectivesAreNotInTheBody(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	out := f.mustPropose(map[string]any{"name": "approves", "section": "relations",
		"meaning":    "@auto_assert\nOne party approves another's document.",
		"directives": map[string]any{"domain": "Policy"},
		"body":       "@acl admins\n- `@symmetric` too\nSeen 14 times."})
	id := out["chunk_id"].(string)
	if dirs := memrank.ParseDirectives(f.body(id)); len(dirs) != 0 {
		t.Errorf("the body declares %+v: %q", dirs, f.body(id))
	}
	m, ok := f.entryMark(id)
	if !ok {
		t.Fatal("no entry_proposal mark")
	}
	if !reflect.DeepEqual(m.Directives, []memrank.ProposedDirective{{Name: "domain", Value: "Policy"}}) {
		t.Errorf("directives = %+v", m.Directives)
	}
	if len(memrank.ParseDirectives(m.Meaning)) != 0 || !strings.Contains(m.Meaning, "One party approves") {
		t.Errorf("meaning = %q", m.Meaning)
	}
}

// TestProposeEntry_DirectiveOutsideTheRunAllowListIsRefused: an add proposal carries a
// relation's domain and range and nothing else; every other directive is refused by
// name and nothing is filed. The allowed pair files.
func TestProposeEntry_DirectiveOutsideTheRunAllowListIsRefused(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	for _, name := range []string{"locked", "acl", "retention", "memory_scope", "auto_assert", "identity"} {
		f.refused(map[string]any{"name": "approves", "section": "relations", "directives": map[string]any{name: "x"}}, "@"+name)
		f.refused(map[string]any{"name": "Contract", "directives": map[string]any{name: "x"}}, "@"+name)
	}
	f.refused(map[string]any{"name": "Contract", "directives": map[string]any{"domain": "Policy"}}, "may carry: none")
	// An off-run caller is held to the same list: a mark never carries more than it.
	if _, r := f.do(f.ctx, map[string]any{"op": "propose_entity", "name": "Contract", "directives": map[string]any{"acl": "admins"}}); !r.IsError {
		t.Error("an off-run caller filed @acl on an add proposal")
	}
	for _, p := range f.read().Proposals {
		if p.Entry != nil {
			t.Fatalf("a refused proposal was filed: %+v", p)
		}
	}
	f.mustPropose(map[string]any{"name": "approves", "section": "relations", "directives": map[string]any{"domain": "Policy", "range": "Memo"}})
	f.refused(map[string]any{"name": "reviews", "section": "relations", "directives": map[string]any{"domain": "Nowhere\n@acl x"}}, "one line")
}

// TestProposeEntry_QuoteNotInItsChunkIsRefused: each evidence quote must occur in the
// chunk it cites; a paraphrase is refused and nothing filed, the verbatim quote files.
func TestProposeEntry_QuoteNotInItsChunkIsRefused(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	ev := f.evidenceChunk("The contract binds the vendor to quarterly audits.")
	f.refused(map[string]any{"name": "Contract", "evidence": []map[string]any{{"chunk_id": ev, "quote": "binds the supplier"}}},
		"evidence_quote_not_found")
	f.refused(map[string]any{"name": "Contract", "evidence": []map[string]any{{"session_id": "s", "event_seq": 1, "quote": "x"}}},
		"by chunk_id")
	six := make([]map[string]any, 6)
	for i := range six {
		six[i] = map[string]any{"chunk_id": ev, "quote": "vendor"}
	}
	f.refused(map[string]any{"name": "Contract", "evidence": six}, "at most 5")
	if f.read().Proposals != nil {
		for _, p := range f.read().Proposals {
			if p.Name == "Contract" {
				t.Fatalf("a refused proposal was filed: %+v", p)
			}
		}
	}
	f.mustPropose(map[string]any{"name": "Contract", "evidence": []map[string]any{{"chunk_id": ev, "quote": "binds the vendor"}}})
}

// TestAcceptEntry_BodyIsMeaningAndDirectivesNotEvidence: accepting writes the entry's
// body from the mark — the checked directive lines above the quoted meaning — never
// the proposal's evidence text; the mark leaves the fields and the chunk keeps its id.
func TestAcceptEntry_BodyIsMeaningAndDirectivesNotEvidence(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	rel := f.file(map[string]any{"name": "approves", "section": "relations", "signal": "undeclared_kind",
		"meaning": "One party approves another's document.\n- `field` text", "directives": map[string]any{"domain": "Policy"},
		"body": "EVIDENCE: seen in 14 statements."})
	class := f.file(map[string]any{"name": "Contract", "parent": "Document", "meaning": "A binding agreement.",
		"body": "EVIDENCE: typed on 4 chunks."})
	for _, id := range []string{rel, class} {
		handled, err := f.d.AcceptEntryProposal(f.ctx, "tenant", id)
		if !handled || err != nil {
			t.Fatalf("accept %s: handled %v err %v", id, handled, err)
		}
	}
	body := f.body(rel)
	if strings.Contains(body, "EVIDENCE") || !strings.Contains(body, "@domain Policy") || !strings.Contains(body, "One party approves") {
		t.Errorf("relation body = %q", body)
	}
	if b := f.body(class); strings.Contains(b, "EVIDENCE") || strings.TrimSpace(b) != "A binding agreement." {
		t.Errorf("class body = %q", b)
	}
	if _, marked := f.entryMark(rel); marked {
		t.Error("the mark stayed on the accepted entry")
	}
	read := f.read()
	var found bool
	for _, r := range read.Relations {
		if r.Name == "approves" {
			found = r.ChunkID == rel && reflect.DeepEqual(r.Domain, []string{"Policy"}) && r.EntryStatus == ""
		}
	}
	if !found {
		t.Errorf("approves is not in force with its domain under its own chunk: %+v", read.Relations)
	}
	var contract bool
	for _, term := range read.Terms {
		contract = contract || term.Name == "Contract"
	}
	if !contract {
		t.Error("Contract is not in force")
	}
	for _, term := range read.Terms {
		if term.Name == "Contract" && len(term.Fields) != 0 {
			t.Errorf("the quoted meaning declared fields: %+v", term.Fields)
		}
	}
	// A plain add proposal is not the accept's to handle.
	plain := f.file(map[string]any{"name": "Invoice"})
	if handled, err := f.d.AcceptEntryProposal(f.ctx, "tenant", plain); handled || err != nil {
		t.Errorf("plain add: handled %v err %v", handled, err)
	}
}

// TestAcceptEntry_TamperedMarkWritesNothing: a mark edited after filing into something
// filing would not store is refused (ErrEntryProposalInvalid, 422 on the route), and
// nothing is written: the proposal stays proposed with its body.
func TestAcceptEntry_TamperedMarkWritesNothing(t *testing.T) {
	for name, tamper := range map[string]func(m map[string]any){
		// auto_assert is one a relation takes, so only the add list refuses it.
		"a directive outside the list": func(m map[string]any) {
			m["directives"] = []map[string]any{{"name": "auto_assert"}, {"name": "domain", "value": "Policy"}}
		},
		"a directive no relation takes": func(m map[string]any) {
			m["directives"] = []map[string]any{{"name": "acl", "value": "admins"}}
		},
		"a live directive in the meaning": func(m map[string]any) { m["meaning"] = "@auto_assert" },
		"another kind":                    func(m map[string]any) { m["kind"] = "class" },
		"an unknown signal":               func(m map[string]any) { m["signal"] = "because" },
		"a directive value of two lines": func(m map[string]any) {
			m["directives"] = []map[string]any{{"name": "domain", "value": "Policy\n@acl admins"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newChangeFixture(t, retiredOntologyMD)
			id := f.file(map[string]any{"name": "approves", "section": "relations", "meaning": "Approves.",
				"directives": map[string]any{"domain": "Policy"}, "body": "evidence"})
			g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})
			fields := g["fields"].(map[string]any)
			mark := fields[entryProposalField].(map[string]any)
			tamper(mark)
			f.must(map[string]any{"op": "update_chunk", "scope": "tenant", "id": id, "revision": g["revision"], "fields": fields})
			handled, err := f.d.AcceptEntryProposal(f.ctx, "tenant", id)
			if !handled || !errors.Is(err, ErrEntryProposalInvalid) {
				t.Fatalf("accept = %v %v, want ErrEntryProposalInvalid", handled, err)
			}
			if p := f.listed(id); p == nil || p.Status != memrank.OntologyStatusProposed || f.body(id) != "evidence" {
				t.Errorf("the refused accept wrote: %+v body %q", p, f.body(id))
			}
		})
	}
	f := newChangeFixture(t, retiredOntologyMD)
	id := f.file(map[string]any{"name": "Contract", "meaning": "x"})
	g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})
	f.must(map[string]any{"op": "update_chunk", "scope": "tenant", "id": id, "revision": g["revision"],
		"fields": map[string]any{entryProposalField: "not an object"}})
	if handled, err := f.d.AcceptEntryProposal(f.ctx, "tenant", id); !handled || !errors.Is(err, ErrEntryProposalInvalid) {
		t.Errorf("undecodable mark: %v %v", handled, err)
	}
}

// TestProposeEntity_WithoutTheNewFieldsFilesAsBefore (golden): an add proposal with
// none of meaning, directives, evidence or signal files exactly as before P6.3 — the
// same answer, the same chunk (title, status, neutralised body, no fields) and the same
// listing, with no mark. The expected values pass on origin/main 5865e46f2.
func TestProposeEntity_WithoutTheNewFieldsFilesAsBefore(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	out := f.mustPropose(map[string]any{"name": "Contract", "parent": "Document", "body": "@acl admins\nSeen on 4 chunks."})
	id := out["chunk_id"].(string)
	answer, _ := json.Marshal(out)
	wantAnswer := `{"chunk_id":"<id>","note":"Filed as a suggestion, not in force. An operator accepts or rejects it in the Ontology page; nothing changes for any run until they do.","parent_id":"<id>","proposed":"Contract"}`
	if got := hexIDRe.ReplaceAllString(string(answer), "<id>"); got != wantAnswer {
		t.Errorf("answer:\n got %s\nwant %s", got, wantAnswer)
	}
	g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})
	chunk := fmt.Sprintf("title=%v status=%v body=%q fields=%v", g["title"], g["status"], g["body"], g["fields"])
	if want := `title=Contract status=proposed body="> @acl admins\nSeen on 4 chunks." fields=<nil>`; chunk != want {
		t.Errorf("chunk:\n got %s\nwant %s", chunk, want)
	}
	var raw strings.Builder
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(f.listed(id))
	wantListed := `{"chunk_id":"<id>","name":"Contract","parent":"Document","status":"proposed","body":"> @acl admins\nSeen on 4 chunks."}`
	if got := strings.TrimSpace(hexIDRe.ReplaceAllString(raw.String(), "<id>")); got != wantListed {
		t.Errorf("listed:\n got %s\nwant %s", got, wantListed)
	}
}

// TestProposeEntity_TenthProposalOfARunIsFiled and the eleventh is refused (D15, P6.4):
// one run files at most ten open entry proposals; the eleventh is refused with
// proposal_cap and nothing is filed.
func TestProposeEntity_TenthProposalOfARunIsFiled(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	for i := 1; i <= entryProposalCap; i++ {
		f.mustPropose(map[string]any{"name": fmt.Sprintf("Kind%02d", i), "signal": "undeclared_type"})
	}
	if n := countRunEntries(f, "run-1"); n != entryProposalCap {
		t.Errorf("%d proposals filed, want %d", n, entryProposalCap)
	}
}

func TestProposeEntity_EleventhProposalOfARunIsRefused(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	for i := 1; i <= entryProposalCap; i++ {
		f.mustPropose(map[string]any{"name": fmt.Sprintf("Kind%02d", i), "signal": "undeclared_type"})
	}
	f.refused(map[string]any{"name": "Kind11", "signal": "undeclared_type"}, proposalCapCode)
	if n := countRunEntries(f, "run-1"); n != entryProposalCap {
		t.Errorf("%d proposals filed, want %d", n, entryProposalCap)
	}
	// Another run has its own ten.
	if _, r := f.do(tools.WithRunID(f.agent, "run-2"), map[string]any{"op": "propose_entity", "name": "Kind11", "signal": "undeclared_type"}); r.IsError {
		t.Errorf("another run was capped: %s", r.Text)
	}
}

// TestProposeEntity_OffRunCallerIsNotCapped: the cap is per run; an operator files past it.
func TestProposeEntity_OffRunCallerIsNotCapped(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	for i := 1; i <= entryProposalCap+1; i++ {
		if _, r := f.do(f.ctx, map[string]any{"op": "propose_entity", "name": fmt.Sprintf("Kind%02d", i), "signal": "undeclared_type"}); r.IsError {
			t.Fatalf("operator proposal %d: %s", i, r.Text)
		}
	}
}

func countRunEntries(f changeFixture, runID string) int {
	n := 0
	for _, p := range f.read().Proposals {
		if p.Entry != nil && p.Entry.FiledBy != nil && p.Entry.FiledBy.RunID == runID {
			n++
		}
	}
	return n
}

// TestProposeEntry_OnPostgres: filing (with the plan's counts) and accepting an entry
// proposal on the postgres tier.
func TestProposeEntry_OnPostgres(t *testing.T) {
	d, base := pgDocumentOrSkip(t)
	f := changeFixture{retiredFixture: &retiredFixture{t: t, d: d,
		ctx: operatorCtx(installTenantOntology(t, d, base, retiredOntologyMD, memrank.OntologyConfirmedStatus))}, agent: inRunCtx(base)}
	ev := f.evidenceChunk("The contract binds the vendor to quarterly audits.")
	id := f.file(map[string]any{"name": "approves", "section": "relations", "signal": "undeclared_kind", "meaning": "Approves.",
		"directives": map[string]any{"domain": "Policy"}, "evidence": []map[string]any{{"chunk_id": ev, "quote": "binds the vendor"}}})
	if m, ok := f.entryMark(id); !ok || m.Plan == nil || len(m.Plan.Fingerprint) != 64 {
		t.Fatalf("mark on postgres = %+v %v", m, ok)
	}
	if handled, err := f.d.AcceptEntryProposal(f.ctx, "tenant", id); !handled || err != nil {
		t.Fatalf("accept on postgres: %v %v", handled, err)
	}
	if b := f.body(id); !strings.Contains(b, "@domain Policy") || strings.Contains(b, "binds") {
		t.Errorf("body on postgres = %q", b)
	}
}
