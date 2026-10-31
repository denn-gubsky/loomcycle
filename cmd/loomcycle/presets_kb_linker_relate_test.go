package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The relate layer (L4) in a propose pass (RFC FP Phase C, owner decisions
// 2026-10-30): only elaborates is filed, on both exact quotes and no confidence
// floor, at most one proposal per passage and target document, and nothing is drawn
// from a paragraph the class marks as boilerplate.

// relateOntology declares elaborates and stricter_than, so the per-kind switch, not
// the vocabulary, is what keeps stricter_than out.
func relateOntology() map[string]any {
	o := kbOntology()
	o["relations"] = append(o["relations"].([]any),
		map[string]any{"name": "elaborates", "source": "base"},
		map[string]any{"name": "stricter_than", "source": "base"})
	return o
}

const relatePropose = `{"document_id":"doc-target","corpus_paths":["/laws"],"layers":["L4"],"write":"propose"}`
const relateReportIn = `{"document_id":"doc-target","corpus_paths":["/laws"],"layers":["L4"]}`

func relateCount(t *testing.T, rep map[string]any, name string) float64 {
	t.Helper()
	n, _ := relateReport(t, rep)["counts"].(map[string]any)[name].(float64)
	return n
}

// runesOf is the rune span of quote in body.
func runesOf(body, quote string) (float64, float64) {
	at := strings.Index(body, quote)
	s := utf8.RuneCountInString(body[:at])
	return float64(s), float64(s + utf8.RuneCountInString(quote))
}

// TestKBLinker_ProposePassFilesOnlyElaborates: two findings, a stricter_than and an
// elaborates; the propose pass files the elaborates (direction respected, both quotes
// as evidence in their own chunks, by extractor:relate, with the verdict's
// confidence) and counts the other as switched off. A report pass counts the same
// one as would_propose and writes nothing.
func TestKBLinker_ProposePassFilesOnlyElaborates(t *testing.T) {
	verdict := `[
	  {"candidate": 1, "kind": "stricter_than", "direction": "this_to_candidate",
	   "quote_from": "within 24 hours of detecting it", "quote_to": "in any case within 72 hours", "confidence": 0.9},
	  {"candidate": 2, "kind": "elaborates", "direction": "candidate_to_this",
	   "quote_from": "report a material incident to the regulator", "quote_to": "names the systems affected", "confidence": 0.55}
	]`
	f := kbBaseFixture()
	f.ontology = relateOntology()
	f.defaultVerdict = verdict

	rep, _ := runLinker(t, f, relatePropose)

	got := proposals(f)
	if len(got) != 1 {
		t.Fatalf("proposals = %v, want only the elaborates finding", got)
	}
	p := got[0]
	if p["from_id"] != "x2" || p["to_id"] != "c1" || p["kind"] != "elaborates" || p["method"] != "extractor:relate" ||
		p["confidence"] != 0.55 {
		t.Errorf("proposal = %v, want x2 elaborates c1 (candidate_to_this) by extractor:relate at 0.55", p)
	}
	ev, _ := p["evidence"].([]any)
	if len(ev) != 2 {
		t.Fatalf("evidence = %v, want one quote in each chunk", ev)
	}
	for i, w := range []struct{ chunk, body, quote string }{
		{"c1", kbBodyClaim, "report a material incident to the regulator"},
		{"x2", kbBodyRule, "names the systems affected"},
	} {
		e := ev[i].(map[string]any)
		s, en := runesOf(w.body, w.quote)
		if e["chunk_id"] != w.chunk || e["quote"] != w.quote || e["char_start"] != s || e["char_end"] != en {
			t.Errorf("evidence[%d] = %v, want %s %q at runes [%v,%v)", i, e, w.chunk, w.quote, s, en)
		}
	}
	if n := relateCount(t, rep, "proposed"); n != 1 {
		t.Errorf("proposed = %v, want 1", n)
	}
	if n := relateCount(t, rep, "propose_kind_off"); n != 1 {
		t.Errorf("propose_kind_off = %v, want 1 (stricter_than)", n)
	}
	if len(reportFindings(t, rep)) != 2 {
		t.Errorf("findings = %v, want both kept in the report", reportFindings(t, rep))
	}
	noViolations(t, f)

	g := kbBaseFixture()
	g.ontology = relateOntology()
	g.defaultVerdict = verdict
	dry, _ := runLinker(t, g, relateReportIn)
	assertNoWrites(t, g, dry)
	if n := relateCount(t, dry, "would_propose"); n != 1 {
		t.Errorf("report pass would_propose = %v, want 1", n)
	}
}

// TestKBLinker_RelateFilesOnlyAKindTheOntologyDeclares: with no ontology, or one
// that does not declare elaborates, the finding stays in the report and nothing is
// filed.
func TestKBLinker_RelateFilesOnlyAKindTheOntologyDeclares(t *testing.T) {
	for name, onto := range map[string]map[string]any{"no ontology": nil, "elaborates undeclared": kbOntology()} {
		t.Run(name, func(t *testing.T) {
			f := kbBaseFixture()
			f.ontology = onto
			f.defaultVerdict = `[{"candidate": 2, "kind": "elaborates", "direction": "this_to_candidate",
			   "quote_from": "report a material incident to the regulator", "quote_to": "names the systems affected", "confidence": 0.7}]`

			rep, _ := runLinker(t, f, relatePropose)

			if got := proposals(f); len(got) != 0 {
				t.Errorf("proposals = %v, want none", got)
			}
			if n := relateCount(t, rep, "propose_kind_not_declared"); n != 1 {
				t.Errorf("propose_kind_not_declared = %v, want 1", n)
			}
			if len(reportFindings(t, rep)) != 1 {
				t.Errorf("the finding must stay in the report: %v", reportFindings(t, rep))
			}
		})
	}
}

// TestKBLinker_OneProposalPerPassageAndTargetDocument: two candidates of the same
// act both elaborate the passage; only the one the verdict is surer of is filed, and
// the other is counted as capped. Both stay in the report.
func TestKBLinker_OneProposalPerPassageAndTargetDocument(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = relateOntology()
	f.defaultVerdict = `[
	  {"candidate": 1, "kind": "elaborates", "direction": "this_to_candidate",
	   "quote_from": "within 24 hours of detecting it", "quote_to": "in any case within 72 hours", "confidence": 0.6},
	  {"candidate": 2, "kind": "elaborates", "direction": "this_to_candidate",
	   "quote_from": "report a material incident to the regulator", "quote_to": "names the systems affected", "confidence": 0.8}
	]`

	rep, _ := runLinker(t, f, relatePropose)

	got := proposals(f)
	if len(got) != 1 || got[0]["to_id"] != "x2" {
		t.Fatalf("proposals = %v, want only c1 → x2, the surer of two in doc-law", got)
	}
	if n := relateCount(t, rep, "propose_capped"); n != 1 {
		t.Errorf("propose_capped = %v, want 1", n)
	}
	if len(reportFindings(t, rep)) != 2 {
		t.Errorf("findings = %v, want both", reportFindings(t, rep))
	}
}

// TestKBLinker_BoilerplateIsNeitherShownNorQuoted: a boilerplate paragraph of the
// passage is left out of what the verdict sees, and a quote from it is no evidence
// even though it occurs; a candidate that is all boilerplate is not sent; a passage
// that is all boilerplate is skipped.
func TestKBLinker_BoilerplateIsNeitherShownNorQuoted(t *testing.T) {
	const (
		boiler = "This act enters into force on the day of its official publication."
		body   = kbBodyClaim + "\n\n" + boiler
	)
	bs, be := runesOf(body, boiler)
	f := kbBaseFixture()
	f.ontology = relateOntology()
	f.chunks[1].Body = body
	f.chunks[1].Boilerplate = [][2]int{{int(bs), int(be)}}
	// x1 is all boilerplate.
	x1 := f.others["x1"]
	x1.Boilerplate = [][2]int{{0, utf8.RuneCountInString(x1.Body)}}
	f.others["x1"] = x1
	// Another passage that is all boilerplate.
	f.chunks = append(f.chunks, kbChunk{ID: "c-boiler", DocumentID: "doc-target", Type: "section",
		Body: kbBodyGloss, Boilerplate: [][2]int{{0, utf8.RuneCountInString(kbBodyGloss)}}})
	f.related["c-boiler"] = f.related["c1"]
	f.defaultVerdict = `[{"candidate": 1, "kind": "elaborates", "direction": "this_to_candidate",
	   "quote_from": "enters into force on the day", "quote_to": "names the systems affected", "confidence": 0.9}]`

	rep, _ := runLinker(t, f, relatePropose)

	agents := f.callsOf("Agent", "")
	if len(agents) != 1 {
		t.Fatalf("verdict calls = %d, want 1 (c1 only; c-boiler is skipped)", len(agents))
	}
	prompt, _ := agents[0].Input["prompt"].(string)
	if strings.Contains(prompt, "enters into force") || strings.Contains(prompt, "BODYBRAVO") {
		t.Errorf("the verdict was shown boilerplate:\n%s", prompt)
	}
	if !strings.Contains(prompt, "BODYALPHA") || !strings.Contains(prompt, "BODYCHARLIE") {
		t.Errorf("the verdict was not shown the passage and the remaining candidate:\n%s", prompt)
	}
	// c-boiler is skipped before its candidates are read, so only c1's x1 counts.
	for counter, want := range map[string]float64{"quotes_in_boilerplate": 1, "candidates_boilerplate": 1, "chunks_skipped_boilerplate": 1} {
		if n := relateCount(t, rep, counter); n != want {
			t.Errorf("%s = %v, want %v", counter, n, want)
		}
	}
	if got := proposals(f); len(got) != 0 {
		t.Errorf("proposals = %v, want none", got)
	}
}
