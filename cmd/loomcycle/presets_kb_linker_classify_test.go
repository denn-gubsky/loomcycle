package main

import "testing"

// The classify layer (L0) of kb/linker, run through the real loop against the
// fixture store (presets_kb_linker_codejs_test.go). The runtime's classify op owns
// every classification rule; these pin what the linker asks of it and how it
// counts the answer.

// TestKBLinker_ClassifyRunsFirstAndIsOneCountedWrite: in propose mode L0 asks
// classify to write, before the citation layer reads the document (an applied
// class decides the grammar), and counts the answer.
func TestKBLinker_ClassifyRunsFirstAndIsOneCountedWrite(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.classified = map[string]any{"result": "classified", "live": true, "class": "Law",
		"issued_by":  []any{map[string]any{"instance": "Ministry", "quote": "Ministry"}},
		"statements": []any{map[string]any{"id": "st-issuer", "status": "asserted"}}}
	f.citations = []map[string]any{kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")})}

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","layers":["L0","L2"]}`)

	calls := f.callsOf("Document", "classify")
	if len(calls) != 1 || calls[0].Input["write"] != "propose" || calls[0].Input["document_id"] != "doc-target" {
		t.Fatalf("classify calls = %v, want one propose call on doc-target", calls)
	}
	first := map[string]int{}
	for i, c := range f.callsOf("Document", "") {
		if _, seen := first[c.Op]; !seen {
			first[c.Op] = i
		}
	}
	if first["classify"] > first["extract_citations"] {
		t.Errorf("classify ran after extract_citations: %v", first)
	}
	for counter, want := range map[string]float64{"classified": 1, "live": 1, "statements": 1, "issuers": 1} {
		if n := layerCount(t, rep, "L0", counter); n != want {
			t.Errorf("L0 %s = %v, want %v", counter, n, want)
		}
	}
	if w := rep["writes"].(map[string]any)["classify"]; w != float64(1) {
		t.Errorf("writes.classify = %v, want 1", w)
	}
	if got := proposals(f); len(got) != 1 {
		t.Errorf("proposals = %v, want the citation filed after classify", got)
	}
	noViolations(t, f)
}

// TestKBLinker_ReportModeClassifiesWithoutWriting: report mode asks classify for
// its answer only, and counts what propose mode would have written.
func TestKBLinker_ReportModeClassifiesWithoutWriting(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.classified = map[string]any{"result": "proposed", "class": "Law"}

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","layers":["L0"]}`)

	calls := f.callsOf("Document", "classify")
	if len(calls) != 1 || calls[0].Input["write"] != "report" {
		t.Fatalf("classify calls = %v, want one report call", calls)
	}
	assertNoWrites(t, f, rep)
	if ww := rep["would_write"].(map[string]any)["classify"]; ww != float64(1) {
		t.Errorf("would_write.classify = %v, want 1 for a proposed class", ww)
	}
	if n := layerCount(t, rep, "L0", "proposed"); n != 1 {
		t.Errorf("L0 proposed = %v, want 1", n)
	}
}

// TestKBLinker_ClassifyNeedsAConfirmedOntology: with no confirmed ontology there
// are no classes to match, so L0 skips with the reason and never calls classify.
func TestKBLinker_ClassifyNeedsAConfirmedOntology(t *testing.T) {
	f := kbBaseFixture()

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","layers":["L0"]}`)

	if n := len(f.callsOf("Document", "classify")); n != 0 {
		t.Errorf("classify called %d times without an ontology", n)
	}
	L := layerReport(t, rep, "L0")
	if L["status"] != "skipped" || L["reason"] == "" {
		t.Errorf("L0 = %v, want skipped with a reason", L)
	}
}

// TestKBLinker_RefusedClassifyIsCountedAndThePassGoesOn: a refused classify is a
// refusal like any write's; the other layers still run, and a pass with another
// write that went through completes.
func TestKBLinker_RefusedClassifyIsCountedAndThePassGoesOn(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.classifyRefusal = "classify: permission: the access-control rules do not let the caller write this document"
	f.citations = []map[string]any{kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")})}

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","layers":["L0","L2"]}`)

	if rep["status"] != "complete" {
		t.Errorf("status = %v (failure %v), want complete", rep["status"], rep["failure"])
	}
	refusals, _ := rep["refusals"].(map[string]any)
	if _, ok := refusals["classify"]; !ok {
		t.Errorf("refusals = %v, want the classify refusal counted", refusals)
	}
	if got := proposals(f); len(got) != 1 {
		t.Errorf("proposals = %v, want the citation still filed", got)
	}
}
