package main

import (
	"strings"
	"testing"
)

// The relate layer's filters before a verdict call, its vocabulary from the tenant
// ontology, and its honesty about a pass that could not judge (owner decisions
// 2026-11-09, after reading every finding of a measured pass on two corpora by
// hand: the false links came from copies of the document, headings, contents
// lines, legends and quotes of a word or two, not from invented quotes).

// TestKBLinker_TargetTypesKeepOnlyCandidatesOfThoseClasses: with target_types, a
// candidate is judged only when its document is of one of the classes or of a
// subclass of one by the ontology's class tree; any other is counted and never
// reaches the verdict. Without target_types the same candidates are all judged.
func TestKBLinker_TargetTypesKeepOnlyCandidatesOfThoseClasses(t *testing.T) {
	setup := func() *kbFixture {
		f := kbBaseFixture()
		withThirdCandidate(f)
		f.ontology = relateOntology()
		f.ontology["classes"] = []any{
			map[string]any{"name": "Document", "kind": "document"},
			map[string]any{"name": "norm", "parent": "Document", "kind": "document"},
			map[string]any{"name": "law", "parent": "norm", "kind": "document"},
			map[string]any{"name": "memo", "parent": "Document", "kind": "document"},
		}
		f.docTypes["doc-law"] = "law"   // a subclass of norm: kept
		f.docTypes["doc-rule"] = "memo" // not a norm: dropped
		return f
	}
	f := setup()
	rep, _ := runLinker(t, f, `{"document_id":"doc-target","corpus_paths":["/laws"],"layers":["L4"],"target_types":["norm"]}`)

	prompts := f.spawnPrompts()
	if len(prompts) != 1 {
		t.Fatalf("verdict calls = %d, want 1", len(prompts))
	}
	if strings.Contains(prompts[0], "BODYDELTA") || !strings.Contains(prompts[0], "BODYBRAVO") {
		t.Errorf("the verdict was shown a memo, or not shown the law:\n%s", prompts[0])
	}
	if n := relateCount(t, rep, "candidates_not_target_type"); n != 1 {
		t.Errorf("candidates_not_target_type = %v, want 1", n)
	}
	if tt := relateReport(t, rep)["target_types"]; tt == nil {
		t.Error("the report does not say which target types narrowed the pass")
	}

	g := setup()
	runLinker(t, g, `{"document_id":"doc-target","corpus_paths":["/laws"],"layers":["L4"]}`)
	if p := g.spawnPrompts(); len(p) != 1 || !strings.Contains(p[0], "BODYDELTA") {
		t.Errorf("without target_types every corpus document is a candidate; prompts=%q", p)
	}

	h := kbBaseFixture()
	rep, _ = runLinker(t, h, `{"document_id":"doc-target","layers":["L1"],"target_types":["norm"]}`)
	if rep["status"] != "refused" {
		t.Errorf("target_types without L4 = %v, want refused", rep["status"])
	}
}

// TestKBLinker_TargetTypesDefaultToTheOntologyNorms: with no target_types input, the
// relate layer judges only candidates of the classes describe_ontology reports in
// norm_classes (the ontology's @norm classes, subclasses included); an explicit
// target_types still wins; a runtime that does not report norm_classes, or reports
// none, leaves the whole corpus. The report says where the target types came from.
func TestKBLinker_TargetTypesDefaultToTheOntologyNorms(t *testing.T) {
	setup := func(norms any) *kbFixture {
		f := kbBaseFixture()
		withThirdCandidate(f)
		f.ontology = relateOntology()
		f.ontology["classes"] = []any{
			map[string]any{"name": "Document", "kind": "document"},
			map[string]any{"name": "norm", "parent": "Document", "kind": "document"},
			map[string]any{"name": "law", "parent": "norm", "kind": "document"},
			map[string]any{"name": "memo", "parent": "Document", "kind": "document"},
		}
		if norms != nil {
			f.ontology["norm_classes"] = norms
		}
		f.docTypes["doc-law"] = "law"
		f.docTypes["doc-rule"] = "memo"
		return f
	}
	from := func(rep map[string]any) any { return relateReport(t, rep)["target_types_from"] }

	f := setup([]any{"law", "norm"})
	rep, _ := runLinker(t, f, relateReportIn)
	if p := f.spawnPrompts(); len(p) != 1 || strings.Contains(p[0], "BODYDELTA") || !strings.Contains(p[0], "BODYBRAVO") {
		t.Errorf("the ontology's norms should narrow the pass to the law; prompts=%q", p)
	}
	if got := from(rep); got != "ontology" {
		t.Errorf("target_types_from = %v, want ontology", got)
	}
	if n := relateCount(t, rep, "candidates_not_target_type"); n != 1 {
		t.Errorf("candidates_not_target_type = %v, want 1", n)
	}

	g := setup([]any{"norm"})
	rep, _ = runLinker(t, g, `{"document_id":"doc-target","corpus_paths":["/laws"],"layers":["L4"],"target_types":["memo"]}`)
	if p := g.spawnPrompts(); len(p) != 1 || !strings.Contains(p[0], "BODYDELTA") || strings.Contains(p[0], "BODYBRAVO") {
		t.Errorf("an explicit target_types should win over the ontology's norms; prompts=%q", p)
	}
	if got := from(rep); got != "input" {
		t.Errorf("target_types_from = %v, want input", got)
	}

	for name, norms := range map[string]any{"older runtime": nil, "no norms": []any{}} {
		h := setup(norms)
		rep, _ = runLinker(t, h, relateReportIn)
		if p := h.spawnPrompts(); len(p) != 1 || !strings.Contains(p[0], "BODYDELTA") || !strings.Contains(p[0], "BODYBRAVO") {
			t.Errorf("%s: the whole corpus should be judged; prompts=%q", name, p)
		}
		if got := from(rep); got != "none" {
			t.Errorf("%s: target_types_from = %v, want none", name, got)
		}
	}

	// A partial read may be missing a norm: it does not narrow the pass.
	k := setup([]any{"norm"})
	k.ontology["partial"] = true
	rep, _ = runLinker(t, k, relateReportIn)
	if got := from(rep); got != "none" {
		t.Errorf("partial read: target_types_from = %v, want none", got)
	}
}

// TestKBLinker_CopyFamilyIsNeverACandidate: a document that is a copy or version
// of the passage's own document — by a duplicate_of, alternate_of or revises
// statement either way, asserted or proposed, or by the same title once copy
// markers are stripped — is never sent to the verdict. An unrelated document with
// a different title still is.
func TestKBLinker_CopyFamilyIsNeverACandidate(t *testing.T) {
	f := kbBaseFixture()
	f.related["c1"] = nil
	add := func(chunk, doc, title, body string) {
		f.others[chunk] = kbChunk{ID: chunk, DocumentID: doc, Type: "section", Title: "TITLE-" + chunk, Body: body}
		f.docTypes[doc] = "act"
		f.docTitles[doc] = title
		f.roots[doc] = "root-" + doc
		f.others["root-"+doc] = kbChunk{ID: "root-" + doc, DocumentID: doc, Type: "act", Title: title}
		f.related["c1"] = append(f.related["c1"], map[string]any{"chunk_id": chunk, "score": 0.9, "type": "section", "document_id": doc})
	}
	add("d1", "doc-dup", "Some other name", kbBodyClaim) // duplicate_of, asserted, copy → target
	add("d2", "doc-rev", "Second name", kbBodyClaim)     // revises, proposed, target → it
	add("d3", "doc-copyof", "Copy of TITLE-OF-THE-TARGET (2)", kbBodyClaim)
	add("d4", "doc-other", "An unrelated act", kbBodyLaw) // judged
	f.statements = []*kbStatement{
		{ID: "s-dup", From: "root-doc-dup", To: "c-root", Kind: "duplicate_of", Status: "asserted", Method: "manual", CreatedAt: 1},
		{ID: "s-rev", From: "c-root", To: "root-doc-rev", Kind: "revises", Status: "proposed", Method: "manual", CreatedAt: 1},
		// A cites statement is no copy relation.
		{ID: "s-cite", From: "c1", To: "d4", Kind: "cites", Status: "asserted", Method: "rule:citation", CreatedAt: 1},
	}

	rep, _ := runLinker(t, f, relateReportIn)

	prompts := f.spawnPrompts()
	if len(prompts) != 1 {
		t.Fatalf("verdict calls = %d, want 1", len(prompts))
	}
	if strings.Count(prompts[0], "BODYALPHA") != 1 || !strings.Contains(prompts[0], "BODYBRAVO") {
		t.Errorf("a copy reached the verdict, or the unrelated act did not:\n%s", prompts[0])
	}
	if n := relateCount(t, rep, "candidates_copy_family"); n != 3 {
		t.Errorf("candidates_copy_family = %v, want 3", n)
	}
	if n, _ := relateReport(t, rep)["copy_family_documents"].(float64); n != 2 {
		t.Errorf("copy_family_documents = %v, want 2 (by statement)", n)
	}
}

// TestKBLinker_CopyFamilyUnreadableStopsThePass: without the copy family the layer
// would file the very links it exists to prevent, so a failed read stops it.
func TestKBLinker_CopyFamilyUnreadableStopsThePass(t *testing.T) {
	f := kbBaseFixture()
	f.failOps["list_statements"] = "list_statements: store unavailable"

	rep, _ := runLinker(t, f, relateReportIn)

	if rep["status"] != "failed" || !strings.Contains(rep["failure"].(string), "copies") {
		t.Errorf("status = %v failure = %v, want failed naming the copies", rep["status"], rep["failure"])
	}
	if n := len(f.callsOf("Agent", "")); n != 0 {
		t.Errorf("verdict calls = %d, want 0", n)
	}
}

// Passage shapes the measured passes judged and got wrong. Each clears
// min_body_chars (80), so the length check alone lets it through.
const (
	kbHeadingOnly = "## 2.1 Definitions of the terms used in this policy\n## 2.2 Scope of application of this policy"
	kbContents    = "1. General provisions .......... 3\n2. Definitions .......... 4\n3. Risk appetite .......... 7\n4. Final provisions .......... 9"
	kbLegend      = "| Level | Meaning |\n|---|---|\n| Achieved | All requirements of a quality metric are met |\n| Partial | Some requirements of the quality metric are met |\n| Missing | None of the requirements is met at all |"
	kbFewWords    = "Supervisory board: composition, competences, powers, delegated authorities, committees."
	// A price table: two cells per row, but figures in them. Not a legend.
	kbPriceTable = "| Phase | Fixed cost |\n|---|---|\n| Discovery and design | $12,400 |\n| Build and launch of the platform | $44,440 |"
)

// TestKBLinker_StructuralPassagesAreSkippedBeforeTheVerdict: a heading-only
// passage, a contents list, a legend and a passage of a few words spend no verdict
// call, each counted by name; the same shapes as candidates are dropped and
// counted. A price table, which has figures, is judged.
func TestKBLinker_StructuralPassagesAreSkippedBeforeTheVerdict(t *testing.T) {
	f := kbBaseFixture()
	f.related["c1"] = nil
	for i, body := range []string{kbHeadingOnly, kbContents, kbLegend, kbFewWords} {
		id := "s" + string(rune('1'+i))
		f.chunks = append(f.chunks, kbChunk{ID: id, DocumentID: "doc-target", Type: "section", Title: "TITLE-" + id, Body: body})
		f.related[id] = []map[string]any{{"chunk_id": "x1", "score": 0.9, "type": "section", "document_id": "doc-law"}}
	}
	f.chunks = append(f.chunks, kbChunk{ID: "s-price", DocumentID: "doc-target", Type: "section", Title: "Prices", Body: kbPriceTable})
	f.related["s-price"] = []map[string]any{{"chunk_id": "x1", "score": 0.9, "type": "section", "document_id": "doc-law"}}
	// c1's candidates: the same shapes in the corpus, and one real passage.
	for i, body := range []string{kbHeadingOnly, kbContents, kbLegend, kbFewWords} {
		id := "y" + string(rune('1'+i))
		f.others[id] = kbChunk{ID: id, DocumentID: "doc-law", Type: "section", Title: "TITLE-" + id, Body: body}
		f.related["c1"] = append(f.related["c1"], map[string]any{"chunk_id": id, "score": 0.9, "type": "section", "document_id": "doc-law"})
	}
	f.related["c1"] = append(f.related["c1"], map[string]any{"chunk_id": "x2", "score": 0.8, "type": "section", "document_id": "doc-law"})

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","corpus_paths":["/laws"],"layers":["L4"],"min_passage_words":10}`)

	prompts := f.spawnPrompts()
	if len(prompts) != 2 {
		t.Fatalf("verdict calls = %d, want 2 (c1 and the price table)", len(prompts))
	}
	for _, p := range prompts {
		for _, shown := range []string{"Definitions of the terms", "General provisions", "All requirements", "Supervisory board"} {
			if strings.Contains(p, shown) {
				t.Errorf("the verdict was shown a structural text (%q):\n%s", shown, p)
			}
		}
	}
	if !strings.Contains(prompts[1], "$44,440") {
		t.Errorf("the price table was not judged:\n%s", prompts[1])
	}
	for _, kind := range []string{"heading", "toc", "legend", "few_words"} {
		if n := relateCount(t, rep, "chunks_skipped_"+kind); n != 1 {
			t.Errorf("chunks_skipped_%s = %v, want 1", kind, n)
		}
		if n := relateCount(t, rep, "candidates_"+kind); n != 1 {
			t.Errorf("candidates_%s = %v, want 1", kind, n)
		}
	}
}

// TestKBLinker_ShortOrHeadingQuoteIsNoFinding: a quote that occurs is still no
// evidence when it has fewer than min_quote_words words, or is a heading line or
// the chunk's title, or a legend row. Each is counted; a full clause on the other
// candidate stands.
func TestKBLinker_ShortOrHeadingQuoteIsNoFinding(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = relateOntology()
	x1 := f.others["x1"]
	x1.Body = "## Reporting of incidents to the regulator\n\n" + kbBodyLaw + "\n\n" + kbBodyRule + "\n\n| Achieved | All requirements are met |\n| Missing | No requirement is met |"
	x1.Title = "Reporting of incidents"
	f.others["x1"] = x1
	f.others["x3"] = kbChunk{ID: "x3", DocumentID: "doc-law", Type: "section", Title: "TITLE-X3", Body: kbBodyDef}
	f.related["c1"] = append(f.related["c1"], map[string]any{"chunk_id": "x3", "score": 0.6, "type": "section", "document_id": "doc-law"})
	x4 := kbChunk{ID: "x4", DocumentID: "doc-law", Type: "section", Title: "TITLE-X4", Body: kbBodyGloss}
	f.others["x4"] = x4
	f.related["c1"] = append(f.related["c1"], map[string]any{"chunk_id": "x4", "score": 0.6, "type": "section", "document_id": "doc-law"})
	f.defaultVerdict = `[
	  {"candidate": 1, "kind": "answers", "direction": "this_to_candidate",
	   "quote_from": "report a material incident to the regulator", "quote_to": "Reporting of incidents to the regulator", "confidence": 0.9},
	  {"candidate": 2, "kind": "answers", "direction": "this_to_candidate",
	   "quote_from": "Every operator", "quote_to": "names the systems affected", "confidence": 0.9},
	  {"candidate": 3, "kind": "answers", "direction": "this_to_candidate",
	   "quote_from": "report a material incident to the regulator", "quote_to": "disrupts a critical service for more than two hours", "confidence": 0.9},
	  {"candidate": 4, "kind": "answers", "direction": "this_to_candidate",
	   "quote_from": "report a material incident to the regulator", "quote_to": "any unplanned interruption or reduction", "confidence": 0.9}
	]`
	// Candidate 1 again, quoting its legend row instead of its heading.
	g := kbBaseFixture()
	g.ontology = relateOntology()
	g.others["x1"] = x1
	g.defaultVerdict = `[{"candidate": 1, "kind": "answers", "direction": "this_to_candidate",
	   "quote_from": "report a material incident to the regulator", "quote_to": "Achieved | All requirements are met", "confidence": 0.9}]`

	rep, _ := runLinker(t, f, relateReportIn)
	repG, _ := runLinker(t, g, relateReportIn)

	if n := relateCount(t, rep, "quotes_heading"); n != 1 {
		t.Errorf("quotes_heading = %v, want 1", n)
	}
	if n := relateCount(t, rep, "quotes_too_short"); n != 1 {
		t.Errorf("quotes_too_short = %v, want 1", n)
	}
	if n := relateCount(t, repG, "quotes_toc_or_legend"); n != 1 {
		t.Errorf("quotes_toc_or_legend = %v, want 1", n)
	}
	findings := reportFindings(t, rep)
	if len(findings) != 2 {
		t.Errorf("findings = %v, want the two full-clause findings", findings)
	}
	checkBuckets(t, rep)
}

// TestKBLinker_TenantKindsReachTheVerdict: the verdict prompt offers the kinds the
// tenant ontology declares itself, with their meaning and inverse, beside the
// built-in ones; a kind the ontology reserves or seeds is not added; a caller's
// relations list still replaces the whole vocabulary.
func TestKBLinker_TenantKindsReachTheVerdict(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = relateOntology()

	rep, _ := runLinker(t, f, relateReportIn)

	prompts := f.spawnPrompts()
	if len(prompts) != 1 {
		t.Fatalf("verdict calls = %d, want 1", len(prompts))
	}
	if !strings.Contains(prompts[0], "- answers: responds to a question or need the other states (read from the other end: answered_by)") {
		t.Errorf("the tenant kind is not offered with its meaning and inverse:\n%s", prompts[0])
	}
	if !strings.Contains(prompts[0], "- elaborates:") {
		t.Errorf("the built-in kinds are no longer offered:\n%s", prompts[0])
	}
	for _, notOffered := range []string{"- mentions:", "- supersedes:", "- cites:"} {
		if strings.Contains(prompts[0], notOffered) {
			t.Errorf("a reserved or seed kind (%s) was offered to the verdict", notOffered)
		}
	}
	if src := relateReport(t, rep)["vocabulary_source"]; src != "default+tenant" {
		t.Errorf("vocabulary_source = %v, want default+tenant", src)
	}

	g := kbBaseFixture()
	g.ontology = relateOntology()
	runLinker(t, g, `{"document_id":"doc-target","corpus_paths":["/laws"],"layers":["L4"],
	  "relations":[{"name":"supersedes_rule","meaning":"replaces the other outright"}]}`)
	if p := g.spawnPrompts(); len(p) != 1 || strings.Contains(p[0], "- answers:") {
		t.Errorf("a caller's relations list must replace the tenant kinds too; prompts=%q", p)
	}
}

// relateManyFixture is a document with n claim-bearing passages, each with c1's
// candidates.
func relateManyFixture(n int) *kbFixture {
	f := kbBaseFixture()
	for i := 0; i < n-1; i++ {
		id := "m" + string(rune('1'+i))
		f.chunks = append(f.chunks, kbChunk{ID: id, DocumentID: "doc-target", Type: "section", Title: "TITLE-" + id, Body: kbBodyClaim})
		f.related[id] = f.related["c1"]
	}
	return f
}

// TestKBLinker_FailingVerdictCallsFailThePass: when every verdict call fails, or
// more than half of at least three, the pass ends failed with the reason and its
// counts, not complete with no findings. One failure in three is not a failed pass.
func TestKBLinker_FailingVerdictCallsFailThePass(t *testing.T) {
	f := relateManyFixture(3)
	f.verdictFailAll = true
	rep, _ := runLinker(t, f, relateReportIn)
	if rep["status"] != "failed" || !strings.Contains(rep["failure"].(string), "3 of 3") {
		t.Errorf("all calls failing: status = %v failure = %v, want failed naming 3 of 3", rep["status"], rep["failure"])
	}
	if n := relateCount(t, rep, "verdict_failures"); n != 3 {
		t.Errorf("verdict_failures = %v, want 3 (the report keeps its counts)", n)
	}
	if relateReport(t, rep)["verdicts_failed"] != true {
		t.Error("the L4 block does not say its verdicts failed")
	}

	g := relateManyFixture(3)
	g.verdictFails = map[int]bool{1: true, 3: true}
	if rep, _ := runLinker(t, g, relateReportIn); rep["status"] != "failed" {
		t.Errorf("two of three failing: status = %v, want failed", rep["status"])
	}

	h := relateManyFixture(3)
	h.verdictFails = map[int]bool{2: true}
	if rep, _ := runLinker(t, h, relateReportIn); rep["status"] != "complete" {
		t.Errorf("one of three failing: status = %v (%v), want complete", rep["status"], rep["failure"])
	}
}

// TestKBLinker_TimeBudgetStopsBetweenVerdictCalls: the clock is read before each
// verdict call; once time_budget_seconds is spent the pass stops with what it
// judged so far, budget_stopped and the reason, instead of running into the run's
// own cap and losing the report. Without the Context tool the budget cannot be
// read: the pass completes and says the clock was unavailable.
func TestKBLinker_TimeBudgetStopsBetweenVerdictCalls(t *testing.T) {
	verdict := `[{"candidate": 1, "kind": "answers", "direction": "this_to_candidate",
	   "quote_from": "within 24 hours of detecting it", "quote_to": "in any case within 72 hours", "confidence": 0.6}]`
	in := `{"document_id":"doc-target","corpus_paths":["/laws"],"layers":["L4"],"time_budget_seconds":60}`

	f := relateManyFixture(3)
	f.ontology = relateOntology()
	f.defaultVerdict = verdict
	f.withClock = true
	f.clockMs = []float64{1000, 59000, 61000}
	rep, _ := runLinker(t, f, in)

	if n := len(f.callsOf("Agent", "")); n != 2 {
		t.Errorf("verdict calls = %d, want 2 (the third passage found the budget spent)", n)
	}
	if rep["status"] != "budget_stopped" {
		t.Errorf("status = %v, want budget_stopped", rep["status"])
	}
	l4 := relateReport(t, rep)
	if l4["stop_reason"] != "time_budget_seconds" || l4["budget_stopped"] != true || l4["clock"] != "context" {
		t.Errorf("L4 stop_reason=%v budget_stopped=%v clock=%v", l4["stop_reason"], l4["budget_stopped"], l4["clock"])
	}
	if b := rep["budget"].(map[string]any); b["stopped_by"] != "time_budget_seconds" {
		t.Errorf("budget.stopped_by = %v", b["stopped_by"])
	}
	if len(reportFindings(t, rep)) != 2 {
		t.Errorf("findings = %v, want the two judged before the stop", reportFindings(t, rep))
	}
	if n := relateCount(t, rep, "chunks_unvisited"); n != 1 {
		t.Errorf("chunks_unvisited = %v, want 1", n)
	}

	g := relateManyFixture(3)
	g.ontology = relateOntology()
	g.defaultVerdict = verdict
	rep, _ = runLinker(t, g, in)
	if rep["status"] != "complete" || relateReport(t, rep)["clock"] != "unavailable" {
		t.Errorf("without Context: status = %v clock = %v, want complete / unavailable", rep["status"], relateReport(t, rep)["clock"])
	}
	if n := len(g.callsOf("Agent", "")); n != 3 {
		t.Errorf("verdict calls without a clock = %d, want 3", n)
	}
}
