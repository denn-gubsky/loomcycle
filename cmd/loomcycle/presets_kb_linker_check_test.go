package main

import (
	"strings"
	"testing"
	"time"
)

// L5 check-against (RFC FP Phase D, owner decision D3: a report document, no
// statements).

const checkInput = `{"document_id":"doc-target","layers":["L5"],"check_against":["/laws"],"write":"propose"}`

func checkFixture() *kbFixture {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.defaultVerdict = `[
	  {"candidate": 1, "kind": "complies_with", "direction": "this_to_candidate",
	   "quote_from": "report a material incident to the regulator", "quote_to": "in any case within 72 hours", "confidence": 0.7},
	  {"candidate": 2, "kind": "possible_gap", "direction": "this_to_candidate",
	   "quote_from": "within 12 hours", "quote_to": "names the systems affected", "confidence": 0.6}
	]`
	return f
}

func checkReportOf(t *testing.T, rep map[string]any) map[string]any {
	t.Helper()
	L, _ := rep["layers"].(map[string]any)["L5"].(map[string]any)
	if L == nil {
		t.Fatalf("no L5 report: %v", rep)
	}
	return L
}

// TestKBLinker_CheckAgainstWritesAReportDocument: a passage is compared with its norms
// and the finding whose quotes occur goes into a report document under
// /reports/check-against, marked as not a legal opinion; the one with an invented
// quote does not. No statement is filed, and the run report carries ids, not text.
func TestKBLinker_CheckAgainstWritesAReportDocument(t *testing.T) {
	f := checkFixture()

	rep, raw := runLinker(t, f, checkInput)

	if got := proposals(f); len(got) != 0 {
		t.Errorf("proposals = %v, want none: check-against files no statement", got)
	}
	calls := f.callsOf("Document", "import_md")
	day := time.Now().UTC().Format("2006-01-02")
	if len(calls) != 1 || calls[0].Input["path"] != "/reports/check-against/TITLE-OF-THE-TARGET-"+day {
		t.Fatalf("import_md calls = %v, want one report at /reports/check-against/TITLE-OF-THE-TARGET-%s", calls, day)
	}
	md := f.importedMD[0]
	for _, want := range []string{"# Check against: TITLE-OF-THE-TARGET", "Not a legal opinion", "## TITLE-C1",
		"**complies_with**", "TITLE-OF-doc-law", "«report a material incident to the regulator»", "«in any case within 72 hours»"} {
		if !strings.Contains(md, want) {
			t.Errorf("report markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "possible_gap") {
		t.Errorf("the finding with an invented quote is in the report:\n%s", md)
	}
	L := checkReportOf(t, rep)
	if c := L["counts"].(map[string]any); c["findings"] != float64(1) || c["quotes_rejected"] != float64(1) {
		t.Errorf("counts = %v, want 1 finding, 1 quote rejected", c)
	}
	if rd, _ := L["report_document"].(map[string]any); rd["status"] != "written" || rd["document_id"] != "doc-report" {
		t.Errorf("report_document = %v, want written doc-report", rd)
	}
	if strings.Contains(raw, "report a material incident") || strings.Contains(raw, "TITLE-C1") {
		t.Errorf("the run report carries store text:\n%s", raw)
	}
}

// TestKBLinker_CheckAgainstReportPassWritesNothing: a report pass compares and counts,
// and says it would write the report.
func TestKBLinker_CheckAgainstReportPassWritesNothing(t *testing.T) {
	f := checkFixture()
	rep, _ := runLinker(t, f, `{"document_id":"doc-target","layers":["L5"],"check_against":["/laws"]}`)
	assertNoWrites(t, f, rep)
	if rd, _ := checkReportOf(t, rep)["report_document"].(map[string]any); rd["status"] != "would_write" {
		t.Errorf("report_document = %v, want would_write", rd)
	}
}

// TestKBLinker_CheckAgainstAsksTheCitedWorksFirst: a passage that already cites a work
// (a live statement into doc-law) is first compared with that work's parts, through
// related limited to the cited documents.
func TestKBLinker_CheckAgainstAsksTheCitedWorksFirst(t *testing.T) {
	f := checkFixture()
	f.statements = []*kbStatement{citeStatement("st-cite", "c1", "x2", "to the regulator")}

	rep, _ := runLinker(t, f, checkInput)

	rel := f.callsOf("Document", "related")
	if len(rel) == 0 {
		t.Fatal("no related call")
	}
	docs, _ := rel[0].Input["document_ids"].([]any)
	if len(docs) != 1 || docs[0] != "doc-law" {
		t.Errorf("first related = %v, want it limited to the cited doc-law", rel[0].Input)
	}
	if c := checkReportOf(t, rep)["counts"].(map[string]any); c["norms_from_citations"] != float64(2) {
		t.Errorf("norms_from_citations = %v, want 2", c["norms_from_citations"])
	}
}

// TestKBLinker_CheckAgainstReportCannotBeRestructuredByAQuote: a quote that crosses a
// line starting with "#" is written on one line inside «», so it never becomes a
// heading of the report; and a report name already taken gets a numbered suffix.
func TestKBLinker_CheckAgainstReportCannotBeRestructuredByAQuote(t *testing.T) {
	f := checkFixture()
	f.chunks[1].Body = kbBodyClaim + "\n# Injected heading that must stay a quote."
	f.defaultVerdict = `[{"candidate": 1, "kind": "addresses", "direction": "this_to_candidate",
	   "quote_from": "detecting it.\n# Injected heading", "quote_to": "in any case within 72 hours", "confidence": 0.5}]`
	day := time.Now().UTC().Format("2006-01-02")
	f.docTypes["doc-old-report"] = "report"
	f.docPaths["doc-old-report"] = "/reports/check-against/TITLE-OF-THE-TARGET-" + day

	runLinker(t, f, checkInput)

	calls := f.callsOf("Document", "import_md")
	if len(calls) != 1 || calls[0].Input["path"] != "/reports/check-against/TITLE-OF-THE-TARGET-"+day+"-2" {
		t.Fatalf("import_md calls = %v, want the -2 name", calls)
	}
	for _, line := range strings.Split(f.importedMD[0], "\n") {
		if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "# Check against:") && !strings.HasPrefix(line, "## ") {
			t.Errorf("a quote became a heading: %q", line)
		}
		if strings.HasPrefix(line, "# Injected") || strings.HasPrefix(line, "## Injected") {
			t.Errorf("a quote became a heading: %q", line)
		}
	}
	if !strings.Contains(f.importedMD[0], "Injected heading") {
		t.Error("the quote is missing from the report")
	}
}

// TestKBLinker_CheckAgainstInputIsChecked: L5 and check_against come together; its
// options belong to L5.
func TestKBLinker_CheckAgainstInputIsChecked(t *testing.T) {
	for _, in := range []string{
		`{"document_id":"doc-target","layers":["L5"]}`,
		`{"document_id":"doc-target","layers":["L2"],"check_against":["/laws"]}`,
		`{"document_id":"doc-target","layers":["L2"],"max_norms":3}`,
		`{"document_id":"doc-target","layers":["L5"],"check_against":["laws"]}`,
		`{"document_id":"doc-target","layers":["L5"],"check_against":["/laws"],"report_root":"/a b"}`,
	} {
		f := checkFixture()
		rep, _ := runLinker(t, f, in)
		if rep["status"] != "refused" || len(f.calls) != 0 {
			t.Errorf("%s: status %v after %d calls, want refused before any", in, rep["status"], len(f.calls))
		}
	}
}
