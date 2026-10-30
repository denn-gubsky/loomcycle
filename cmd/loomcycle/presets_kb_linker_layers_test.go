package main

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// The citation, term and anchor layers of kb/linker, run through the real loop
// against the fixture store (presets_kb_linker_codejs_test.go). Every shape the
// fixture answers is listed in kbOpContracts.

// kbOntology is a confirmed tenant ontology: a writable seed kind, uses_term (a
// kind the tenant declares, as L3 needs), a tenant kind the runtime
// auto-asserts, reserved kinds, and one concept scheme.
func kbOntology() map[string]any {
	return map[string]any{
		"exists": true, "status": "confirmed", "confirmed": true, "version": 7,
		"sections": []any{"classes", "relations", "schemes"},
		"classes":  []any{},
		"relations": []any{
			map[string]any{"name": "cites", "source": "base"},
			map[string]any{"name": "uses_term", "source": "tenant"},
			map[string]any{"name": "amends", "source": "tenant", "auto_assert": true, "dated": true},
			map[string]any{"name": "mentions", "source": "reserved"},
			map[string]any{"name": "supersedes", "source": "reserved"},
		},
		"grammars": []any{},
		"schemes": []any{map[string]any{"name": "topics", "concepts": []any{
			map[string]any{"name": "incident report", "alt": []any{"incident notification"}, "chunk_id": "k-incident",
				"tag": "topics/incident-report", "path": []any{"incident report"}},
			map[string]any{"name": "regulator", "chunk_id": "k-regulator", "tag": "topics/regulator", "path": []any{"regulator"}},
		}}},
	}
}

// kbCiteHit is one extract_citations hit on chunk c1 of the base fixture, at the
// rune span of quote in kbBodyClaim.
func kbCiteHit(quote string, extra map[string]any) map[string]any {
	at := strings.Index(kbBodyClaim, quote)
	if at < 0 {
		panic("quote not in kbBodyClaim: " + quote)
	}
	start := utf8.RuneCountInString(kbBodyClaim[:at])
	h := map[string]any{"chunk_id": "c1", "quote": quote, "char_start": start,
		"char_end": start + utf8.RuneCountInString(quote), "resolved": map[string]any{"exact": false, "placeholder": false}}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func resolvedTo(chunkID string) map[string]any {
	return map[string]any{"chunk_id": chunkID, "document_id": "doc-law", "exact": true, "placeholder": false}
}

// proposals returns every propose_statement the pass sent.
func proposals(f *kbFixture) []map[string]any {
	var out []map[string]any
	for _, c := range f.callsOf("Document", "propose_statement") {
		out = append(out, c.Input)
	}
	return out
}

func noViolations(t *testing.T, f *kbFixture) {
	t.Helper()
	for _, v := range f.violations {
		t.Errorf("the pass sent what the runtime refuses: %s", v)
	}
}

// TestKBLinker_GrammarKindOutsideTheVocabularyIsFiledAsCites: the grammar names
// a kind, but only the ontology decides what may be written. A declared tenant
// kind is used; an undeclared one and a reserved one both become cites; a hit
// with no kind is cites.
func TestKBLinker_GrammarKindOutsideTheVocabularyIsFiledAsCites(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.autoAssert["amends"] = true
	f.citations = []map[string]any{
		kbCiteHit("report a material incident", map[string]any{"kind": "amends", "resolved": resolvedTo("x1")}),
		kbCiteHit("to the regulator", map[string]any{"kind": "repeals", "resolved": resolvedTo("x2")}),
		kbCiteHit("within 24 hours", map[string]any{"kind": "mentions", "resolved": resolvedTo("x1")}),
		kbCiteHit("of detecting it", map[string]any{"resolved": resolvedTo("x2")}),
	}

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","layers":["L2"]}`)

	got := proposals(f)
	var kinds []string
	for _, p := range got {
		kinds = append(kinds, p["kind"].(string))
		if p["method"] != "rule:citation" {
			t.Errorf("a citation was proposed with method %v, want rule:citation", p["method"])
		}
		if p["from_id"] != "c1" {
			t.Errorf("a citation was proposed from %v, want the citing passage c1", p["from_id"])
		}
	}
	if strings.Join(kinds, ",") != "amends,cites,cites,cites" {
		t.Errorf("proposed kinds = %v, want [amends cites cites cites] — only a declared, writable kind is used", kinds)
	}
	if n := layerCount(t, rep, "L2", "kind_from_grammar"); n != 1 {
		t.Errorf("kind_from_grammar = %v, want 1", n)
	}
	if n := layerCount(t, rep, "L2", "kind_defaulted"); n != 2 {
		t.Errorf("kind_defaulted = %v, want 2 (the undeclared and the reserved kind)", n)
	}
	// The runtime decided the assertion, and the report says what it decided.
	if n := layerCount(t, rep, "L2", "asserted"); n != 1 {
		t.Errorf("asserted = %v, want 1 (amends is auto-asserted by the runtime)", n)
	}
	// The hit's own span is passed through untouched: it is already in runes.
	want := kbCiteHit("report a material incident", nil)
	ev := got[0]["evidence"].([]any)[0].(map[string]any)
	if ev["quote"] != want["quote"] || ev["char_start"] != float64(want["char_start"].(int)) ||
		ev["char_end"] != float64(want["char_end"].(int)) || ev["chunk_id"] != "c1" {
		t.Errorf("evidence = %v, want the hit's quote and span %v", ev, want)
	}
	if rep["ontology_version"].(float64) != 7 {
		t.Errorf("ontology_version = %v, want 7 stamped from the one ontology read", rep["ontology_version"])
	}
	if n := len(f.callsOf("Document", "describe_ontology")); n != 1 {
		t.Errorf("describe_ontology called %d times, want once per pass", n)
	}
	noViolations(t, f)
}

// TestKBLinker_ReRunOverAnAcceptedCitationIsNotARefusal: a second pass finds a
// citation an operator already accepted. The runtime refuses the proposal
// (statement_asserted), and the report counts it as already asserted, not as a
// refused write. A rejected relation stays a refusal; the control hit proves the
// proposing path is live.
func TestKBLinker_ReRunOverAnAcceptedCitationIsNotARefusal(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.statements = append(f.statements,
		&kbStatement{ID: "st-held", From: "c1", To: "x1", Kind: "cites", Status: "asserted", Method: "manual"},
		&kbStatement{ID: "st-no", From: "c1", To: "x2", Kind: "cites", Status: "rejected", Method: "rule:citation"})
	f.citations = []map[string]any{
		kbCiteHit("report a material incident", map[string]any{"resolved": resolvedTo("x1")}),
		kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")}),
		kbCiteHit("within 24 hours", map[string]any{"resolved": resolvedTo("x3")}),
	}

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","layers":["L2"]}`)

	if n := layerCount(t, rep, "L2", "already_asserted"); n != 1 {
		t.Errorf("already_asserted = %v, want 1 (the accepted citation)", n)
	}
	if n := layerCount(t, rep, "L2", "refused"); n != 1 {
		t.Errorf("refused = %v, want 1 (only the rejected relation)", n)
	}
	if n := layerCount(t, rep, "L2", "proposed"); n != 1 {
		t.Errorf("proposed = %v, want 1 (the control hit)", n)
	}
	if got := len(f.statementByID("st-held").Evidence); got != 0 {
		t.Errorf("the accepted statement gained %d evidence rows from a proposal", got)
	}
	noViolations(t, f)
}

// TestKBLinker_PassWhoseEveryWriteIsRefusedFails: when the runtime refuses every
// write a pass makes, that is a configuration fault (here a missing method
// grant), and the pass fails rather than report complete. A relation that is
// already accepted or was rejected is the expected answer on a re-run, so a pass
// that met only those stays complete.
func TestKBLinker_PassWhoseEveryWriteIsRefusedFails(t *testing.T) {
	hits := func() []map[string]any {
		return []map[string]any{
			kbCiteHit("report a material incident", map[string]any{"resolved": resolvedTo("x1")}),
			kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")}),
		}
	}
	t.Run("not granted", func(t *testing.T) {
		f := kbBaseFixture()
		f.ontology = kbOntology()
		f.refuseKind["cites"] = "propose_statement: method_not_granted: this agent may not stamp rule:citation"
		f.citations = hits()
		rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","layers":["L2"]}`)
		if rep["status"] != "failed" {
			t.Errorf("status = %v, want failed", rep["status"])
		}
		if fail, _ := rep["failure"].(string); !strings.Contains(fail, "method_not_granted") {
			t.Errorf("failure = %q, want it to name method_not_granted", fail)
		}
	})
	t.Run("only accepted or rejected relations", func(t *testing.T) {
		f := kbBaseFixture()
		f.ontology = kbOntology()
		f.statements = append(f.statements,
			&kbStatement{ID: "st-held", From: "c1", To: "x1", Kind: "cites", Status: "asserted", Method: "manual"},
			&kbStatement{ID: "st-no", From: "c1", To: "x2", Kind: "cites", Status: "rejected", Method: "rule:citation"})
		f.citations = hits()
		rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","layers":["L2"]}`)
		if rep["status"] != "complete" {
			t.Errorf("status = %v (failure %v), want complete", rep["status"], rep["failure"])
		}
	})
}

// TestKBLinker_UnreadableCitationIsUnresolved: extract_citations answers a work
// the caller may not read exactly as one the scope does not hold, so as not to
// reveal that it exists. The linker therefore asks for a placeholder, a caller
// confined to part of the scope is refused it, and the citation is unresolved
// with nothing proposed. The control hit proves the proposing path is live.
func TestKBLinker_UnreadableCitationIsUnresolved(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.placeholderRefusal = "resolve_anchor: a work key is claimed for the whole scope, and the caller's access-control rules do not let it write the whole scope"
	f.citations = []map[string]any{
		kbCiteHit("report a material incident", map[string]any{"work_key": "law:secret", "anchor": "article-3",
			"resolved": map[string]any{"exact": false, "placeholder": false}}),
		kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")}),
	}

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","layers":["L2"]}`)

	if rep["status"] != "complete" {
		t.Errorf("status = %v (failure %v), want complete", rep["status"], rep["failure"])
	}
	got := proposals(f)
	if len(got) != 1 || got[0]["to_id"] != "x2" {
		t.Errorf("proposals = %v, want only the readable citation to x2", got)
	}
	if n := layerCount(t, rep, "L2", "placeholders_refused"); n != 1 {
		t.Errorf("placeholders_refused = %v, want 1", n)
	}
	if n := layerCount(t, rep, "L2", "hits_unresolved"); n != 1 {
		t.Errorf("hits_unresolved = %v, want 1", n)
	}
}

// TestKBLinker_UnheldWorkGetsAPlaceholderTarget: a citation to a work the scope
// does not hold is filed against a placeholder, and one to a held work whose part
// is missing against the work's root.
func TestKBLinker_UnheldWorkGetsAPlaceholderTarget(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.heldWorks["law:held"] = "held-root"
	f.citations = []map[string]any{
		kbCiteHit("report a material incident", map[string]any{"work_key": "law:new", "anchor": "article-9"}),
		kbCiteHit("to the regulator", map[string]any{"work_key": "law:held", "anchor": "article-99",
			"resolved": map[string]any{"document_id": "doc-of-law:held", "exact": false, "placeholder": false}}),
	}

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","layers":["L2"],"as_of":1735689600000000000}`)

	// The input's date reaches both reads that resolve as of a moment: a first
	// edition's parts carry no date of their own.
	for _, op := range []string{"extract_citations", "resolve_anchor"} {
		for _, c := range f.callsOf("Document", op) {
			if c.Input["as_of"] != float64(1735689600000000000) {
				t.Errorf("%s carried as_of=%v, want the input's 1735689600000000000", op, c.Input["as_of"])
			}
		}
	}
	got := proposals(f)
	if len(got) != 2 || got[0]["to_id"] != "ph-root-law:new" || got[1]["to_id"] != "held-root" {
		t.Fatalf("proposals = %v, want c1 → the placeholder root, then c1 → the held work's root", got)
	}
	ra := f.callsOf("Document", "resolve_anchor")
	if len(ra) != 2 || ra[0].Input["create_placeholder"] != true || ra[1].Input["create_placeholder"] != nil {
		t.Errorf("resolve_anchor calls = %v, want a placeholder only for the unheld work", ra)
	}
	if n := layerCount(t, rep, "L2", "placeholders"); n != 1 {
		t.Errorf("placeholders = %v, want 1", n)
	}
}

// TestKBLinker_DocumentCitingItsOwnWorkIsSelf: an act names itself (an import
// header's "Resolution No 311" inside resolution 311), and the grammar resolves
// that to the document being linked. It is a self-hit in both modes: never a
// statement from the act to its own root, and never a resolve_anchor call.
func TestKBLinker_DocumentCitingItsOwnWorkIsSelf(t *testing.T) {
	for _, mode := range []string{"report", "propose"} {
		t.Run(mode, func(t *testing.T) {
			f := kbBaseFixture()
			f.ontology = kbOntology()
			f.heldWorks["act:self"] = "c-root"
			f.heldWorks["law:held"] = "held-root"
			f.citations = []map[string]any{
				kbCiteHit("report a material incident", map[string]any{"work_key": "act:self",
					"resolved": map[string]any{"document_id": "doc-target", "exact": false, "placeholder": false}}),
				kbCiteHit("to the regulator", map[string]any{"work_key": "law:held",
					"resolved": map[string]any{"document_id": "doc-of-law:held", "exact": false, "placeholder": false}}),
			}

			rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"`+mode+`","layers":["L2"]}`)

			if n := layerCount(t, rep, "L2", "hits_self"); n != 1 {
				t.Errorf("hits_self = %v, want 1", n)
			}
			if n := layerCount(t, rep, "L2", "targets_work_root"); n != 1 {
				t.Errorf("targets_work_root = %v, want 1 (the other work only)", n)
			}
			for _, c := range f.callsOf("Document", "resolve_anchor") {
				if c.Input["work_key"] == "act:self" {
					t.Errorf("resolve_anchor was asked for the document's own work: %v", c.Input)
				}
			}
			if mode == "report" {
				if n := layerCount(t, rep, "L2", "would_propose"); n != 1 {
					t.Errorf("would_propose = %v, want 1", n)
				}
				return
			}
			got := proposals(f)
			if len(got) != 1 || got[0]["to_id"] != "held-root" {
				t.Errorf("proposals = %v, want only c1 → the held work's root", got)
			}
		})
	}
}

// TestKBLinker_RefusedPlaceholderIsUnresolvedNotAFailure: a caller that may not
// write the whole scope is refused every placeholder, held or not, in the same
// words. Each such citation is unresolved, and a pass that met only those still
// completes: it is the answer that caller gets, not a fault in the setup.
func TestKBLinker_RefusedPlaceholderIsUnresolvedNotAFailure(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.placeholderRefusal = "resolve_anchor: a work key is claimed for the whole scope, and the caller's access-control rules do not let it write the whole scope"
	f.citations = []map[string]any{
		kbCiteHit("report a material incident", map[string]any{"work_key": "law:a", "anchor": "article-9"}),
		kbCiteHit("to the regulator", map[string]any{"work_key": "law:b"}),
	}

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","layers":["L2"]}`)

	if rep["status"] != "complete" {
		t.Errorf("status = %v (failure %v), want complete", rep["status"], rep["failure"])
	}
	if n := layerCount(t, rep, "L2", "hits_unresolved"); n != 2 {
		t.Errorf("hits_unresolved = %v, want 2", n)
	}
	if n := layerCount(t, rep, "L2", "placeholders_refused"); n != 2 {
		t.Errorf("placeholders_refused = %v, want 2", n)
	}
	if got := proposals(f); len(got) != 0 {
		t.Errorf("proposals = %v, want none", got)
	}
}

// TestKBLinker_TruncatedCitationListIsReadPartByPart: extract_citations has a
// limit and no cursor, so a cut document listing is read again one part at a
// time, and every citation is still proposed exactly once.
func TestKBLinker_TruncatedCitationListIsReadPartByPart(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.citeTruncateDoc = true
	f.citations = []map[string]any{
		kbCiteHit("report a material incident", map[string]any{"resolved": resolvedTo("x1")}),
		kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")}),
	}

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","layers":["L2"]}`)

	var byPart []string
	for _, c := range f.callsOf("Document", "extract_citations") {
		if id, ok := c.Input["id"].(string); ok {
			byPart = append(byPart, id)
		}
	}
	if strings.Join(byPart, ",") != "c1,c-short" {
		t.Errorf("parts read one by one = %v, want every live non-root part [c1 c-short]", byPart)
	}
	if got := proposals(f); len(got) != 2 {
		t.Errorf("proposals = %d, want 2 — each citation once", len(got))
	}
	if n := layerCount(t, rep, "L2", "read_by_part"); n != 1 {
		t.Errorf("read_by_part = %v, want 1", n)
	}
}

// TestKBLinker_UnconfirmedOntologySkipsCiteAndTerms: a draft ontology governs
// nothing, so the layers that need its vocabulary skip and say why, while the
// anchor report and the maintenance checks that need no vocabulary still run.
func TestKBLinker_UnconfirmedOntologySkipsCiteAndTerms(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.ontology["confirmed"] = false
	f.ontology["status"] = "draft"
	f.citations = []map[string]any{kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")})}
	// A kind no draft declares: the kind check would demote it, so a verify
	// proves the check did not run.
	f.statements = []*kbStatement{{ID: "st-1", From: "c1", To: "x1", Kind: "obsolete_kind", Status: "asserted",
		Method: "rule:citation", CreatedAt: 1,
		Evidence: []*kbEvidence{{ID: "ev-1", ChunkID: "c1", Quote: "report a material incident"}}}}

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","scope":"tenant","terms_scheme":"topics"}`)

	if n := len(f.callsOf("Document", "extract_citations")); n != 0 {
		t.Errorf("extract_citations called %d times under a draft ontology", n)
	}
	if got := proposals(f); len(got) != 0 {
		t.Errorf("proposals = %v under a draft ontology, want none", got)
	}
	for _, l := range []string{"L2", "L3"} {
		lr := layerReport(t, rep, l)
		if lr["status"] != "skipped" || lr["reason"] != "the ontology is not confirmed" {
			t.Errorf("%s = %v, want skipped because the ontology is not confirmed", l, lr)
		}
	}
	if lr := layerReport(t, rep, "L1"); lr["status"] != "run" {
		t.Errorf("L1 = %v, want run — it needs no vocabulary", lr)
	}
	m := f.callsOf("Document", "maintain_statement")
	if len(m) != 1 || m[0].Input["action"] != "verify" {
		t.Errorf("maintain calls = %v, want one verify — the kind check needs a confirmed ontology", m)
	}
	if n := layerCount(t, rep, "L6", "kind_unchecked"); n != 1 {
		t.Errorf("kind_unchecked = %v, want 1", n)
	}
	if rep["status"] != "complete" {
		t.Errorf("status = %v, want complete — a skipped layer is not a failure", rep["status"])
	}
}

// TestKBLinker_TermsOutsideTheTenantScopeAreCountedNotWritten: concepts live in
// the tenant scope and a statement cannot cross scopes, so a user-scope pass
// counts its term matches and writes none. The tenant run is the control.
func TestKBLinker_TermsOutsideTheTenantScopeAreCountedNotWritten(t *testing.T) {
	body := "BODYTERMS Every operator shall send an incident report to the regulator, and keep the incident notification on file."
	build := func() *kbFixture {
		f := kbBaseFixture()
		f.ontology = kbOntology()
		f.chunks[1].Body = body
		return f
	}

	user := build()
	rep, _ := runLinker(t, user, `{"document_id":"doc-target","write":"propose","layers":["L3"],"terms_scheme":"topics"}`)
	if got := proposals(user); len(got) != 0 {
		t.Errorf("a user-scope pass proposed %v — it cannot reach tenant concepts", got)
	}
	if n := layerCount(t, rep, "L3", "terms_skipped_scope"); n != 2 {
		t.Errorf("terms_skipped_scope = %v, want 2 (incident report, regulator)", n)
	}

	tenant := build()
	rep, _ = runLinker(t, tenant, `{"document_id":"doc-target","write":"propose","scope":"tenant","layers":["L3"],"terms_scheme":"topics"}`)
	got := proposals(tenant)
	if len(got) != 2 {
		t.Fatalf("tenant proposals = %v, want one per concept", got)
	}
	if got[0]["to_id"] != "k-incident" || got[0]["kind"] != "uses_term" || got[0]["method"] != "rule:terms" {
		t.Errorf("first proposal = %v, want c1 uses_term k-incident by rule:terms", got[0])
	}
	// The name and its alternative label both point at k-incident: one statement,
	// two evidence rows.
	if ev := got[0]["evidence"].([]any); len(ev) != 2 {
		t.Errorf("k-incident evidence = %v, want both the name and the alternative label", ev)
	}
	if n := layerCount(t, rep, "L3", "terms_skipped_scope"); n != 0 {
		t.Errorf("terms_skipped_scope = %v at tenant scope, want 0", n)
	}
	noViolations(t, tenant)
}

// kbDefinitionsBody is a definitions passage: two defined terms, each starting a
// line after a list marker, and two terms used mid-line.
const kbDefinitionsBody = "BODYDEFS In this Act the following terms are used:\n" +
	"1) incident report — a notice an operator sends to the regulator within 24 hours;\n" +
	"а) «договір» — угода сторін, яку оператор подає разом з incident notification."

// definitionsFixture has c1 as a definitions passage (or an ordinary one, with
// definitional false) and a scheme holding a Cyrillic term too. withDefines
// declares the defines kind.
func definitionsFixture(definitional, withDefines bool) *kbFixture {
	f := kbBaseFixture()
	o := kbOntology()
	if withDefines {
		o["relations"] = append(o["relations"].([]any), map[string]any{"name": "defines", "source": "base"})
	}
	scheme := o["schemes"].([]any)[0].(map[string]any)
	scheme["concepts"] = append(scheme["concepts"].([]any),
		map[string]any{"name": "договір", "chunk_id": "k-contract", "tag": "topics/contract", "path": []any{"договір"}})
	f.ontology = o
	f.chunks[1].Body = kbDefinitionsBody
	f.chunks[1].Definitional = definitional
	return f
}

// TestKBLinker_DefinitionsPassageDefinesTheTermThatStartsALine: in a passage the
// runtime marks definitional, a term that starts a line (after a list marker or an
// opening quote) is defined there, so the statement is defines, by
// rule:definitions, with that occurrence first in its evidence. A term used
// mid-line stays uses_term. The same text in an ordinary passage defines nothing:
// the runtime's mark decides, not the layout.
func TestKBLinker_DefinitionsPassageDefinesTheTermThatStartsALine(t *testing.T) {
	in := `{"document_id":"doc-target","write":"propose","scope":"tenant","layers":["L3"],"terms_scheme":"topics"}`

	f := definitionsFixture(true, true)
	rep, _ := runLinker(t, f, in)
	got := map[string]map[string]any{}
	for _, p := range proposals(f) {
		got[p["to_id"].(string)] = p
	}
	if len(got) != 3 {
		t.Fatalf("proposals = %v, want one per concept (incident report, regulator, договір)", proposals(f))
	}
	for _, c := range []string{"k-incident", "k-contract"} {
		if got[c]["kind"] != "defines" || got[c]["method"] != "rule:definitions" {
			t.Errorf("%s: proposal = %v, want defines by rule:definitions", c, got[c])
		}
	}
	if got["k-regulator"]["kind"] != "uses_term" || got["k-regulator"]["method"] != "rule:terms" {
		t.Errorf("k-regulator: proposal = %v, want uses_term by rule:terms — it is used mid-line", got["k-regulator"])
	}
	// k-incident's name defines it; its alternative label is only used later on.
	ev := got["k-incident"]["evidence"].([]any)
	if len(ev) != 2 || ev[0].(map[string]any)["quote"] != "incident report" {
		t.Errorf("k-incident evidence = %v, want the defining occurrence first, then the use", ev)
	}
	if n := layerCount(t, rep, "L3", "definienda"); n != 2 {
		t.Errorf("definienda = %v, want 2", n)
	}
	if n := layerCount(t, rep, "L3", "definitional_parts"); n != 1 {
		t.Errorf("definitional_parts = %v, want 1", n)
	}
	noViolations(t, f)

	plain := definitionsFixture(false, true)
	runLinker(t, plain, in)
	for _, p := range proposals(plain) {
		if p["kind"] != "uses_term" {
			t.Errorf("ordinary passage proposed %v, want uses_term only", p)
		}
	}
}

// TestKBLinker_DefinedTermWithoutADefinesKindIsUsed: when the ontology does not
// declare defines, a term a definitions passage defines is still a term it uses,
// and the report says how many could not be filed as defines.
func TestKBLinker_DefinedTermWithoutADefinesKindIsUsed(t *testing.T) {
	f := definitionsFixture(true, false)
	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","scope":"tenant","layers":["L3"],"terms_scheme":"topics"}`)
	got := proposals(f)
	if len(got) != 3 {
		t.Fatalf("proposals = %v, want three", got)
	}
	for _, p := range got {
		if p["kind"] != "uses_term" {
			t.Errorf("proposal %v, want uses_term — defines is not declared", p)
		}
	}
	if n := layerCount(t, rep, "L3", "defines_not_declared"); n != 2 {
		t.Errorf("defines_not_declared = %v, want 2", n)
	}
}

// TestKBLinker_EvidenceSpansAreRunesOnNonASCIIText: JS indexes strings in UTF-16
// units, evidence spans are code points. A body that starts with a character
// outside the BMP (two units, one rune) and continues in Cyrillic makes the two
// differ; the fixture checks each span slices the body's runes to the quote.
// The match is also normalised (case and a doubled space), so the quote must be
// the body's own text, and Cyrillic word boundaries must hold ("регулятор" is not
// inside "регулятору").
func TestKBLinker_EvidenceSpansAreRunesOnNonASCIIText(t *testing.T) {
	body := "😀 Оператор надсилає ПОВІДОМЛЕННЯ  про інцидент регулятору протягом доби з моменту його виявлення."
	f := kbBaseFixture()
	onto := kbOntology()
	onto["schemes"] = []any{map[string]any{"name": "topics", "concepts": []any{
		map[string]any{"name": "повідомлення про інцидент", "chunk_id": "k-incident", "tag": "topics/incident"},
		map[string]any{"name": "регулятор", "chunk_id": "k-regulator", "tag": "topics/regulator"},
	}}}
	f.ontology = onto
	f.chunks[1].Body = body

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","write":"propose","scope":"tenant","layers":["L3"],"terms_scheme":"topics"}`)

	got := proposals(f)
	if len(got) != 1 || got[0]["to_id"] != "k-incident" {
		t.Fatalf("proposals = %v, want exactly c1 uses_term k-incident (no match inside регулятору)", got)
	}
	ev := got[0]["evidence"].([]any)[0].(map[string]any)
	quote := "ПОВІДОМЛЕННЯ  про інцидент"
	at := strings.Index(body, quote)
	wantStart := utf8.RuneCountInString(body[:at])
	wantEnd := wantStart + utf8.RuneCountInString(quote)
	if ev["quote"] != quote || ev["char_start"] != float64(wantStart) || ev["char_end"] != float64(wantEnd) {
		t.Errorf("evidence = %v, want quote %q at runes [%d,%d)", ev, quote, wantStart, wantEnd)
	}
	if n := layerCount(t, rep, "L3", "matches_normalised"); n != 1 {
		t.Errorf("matches_normalised = %v, want 1", n)
	}
	noViolations(t, f)
}

// TestKBLinker_SuccessorEvidenceSpansAreRunes: the maintenance layer computes a
// span too, when it moves a statement onto a newer edition of a part. The same
// rune rule holds there.
func TestKBLinker_SuccessorEvidenceSpansAreRunes(t *testing.T) {
	quote := "подає звіт регулятору"
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.chunks = append(f.chunks,
		kbChunk{ID: "c-v1", DocumentID: "doc-target", Type: "section", Body: "Оператор " + quote + " щодня.", InvalidAt: 50, StableID: "art-9", Anchor: "art-9"},
		kbChunk{ID: "c-v2", DocumentID: "doc-target", Type: "section", Body: "🛈 Оператор 𝔞 " + quote + " щотижня.", StableID: "art-9", Anchor: "art-9"})
	f.supersedes["c-v1"] = "c-v2"
	f.statements = []*kbStatement{{ID: "st-v", From: "c-v1", To: "x1", Kind: "cites", Status: "asserted",
		Method: "rule:citation", CreatedAt: 1, Evidence: []*kbEvidence{{ID: "ev-v", ChunkID: "c-v1", Quote: quote}}}}

	runLinker(t, f, `{"mode":"maintain","document_id":"doc-target","write":"propose"}`)

	got := proposals(f)
	if len(got) != 1 || got[0]["from_id"] != "c-v2" {
		t.Fatalf("proposals = %v, want the statement moved to c-v2", got)
	}
	v2, _ := f.chunkByID("c-v2")
	at := strings.Index(v2.Body, quote)
	start := utf8.RuneCountInString(v2.Body[:at])
	ev := got[0]["evidence"].([]any)[0].(map[string]any)
	if ev["chunk_id"] != "c-v2" || ev["char_start"] != float64(start) || ev["char_end"] != float64(start+utf8.RuneCountInString(quote)) {
		t.Errorf("successor evidence = %v, want c-v2 at runes [%d,%d)", ev, start, start+utf8.RuneCountInString(quote))
	}
	noViolations(t, f)
}

// TestKBLinker_AnchorReportCountsCollisionsByID: L1 reads the anchors and stable
// ids of the parts in force, reports duplicates by id only, and writes nothing.
func TestKBLinker_AnchorReportCountsCollisionsByID(t *testing.T) {
	f := kbBaseFixture()
	f.chunks = append(f.chunks,
		kbChunk{ID: "c-dup", DocumentID: "doc-target", Type: "section", Title: "TITLE-DUP", Body: kbBodySelf, Anchor: "notes~2", StableID: "notes~2"},
		kbChunk{ID: "c-bare", DocumentID: "doc-target", Type: "section", Title: "TITLE-BARE", Body: kbBodyRule},
		kbChunk{ID: "c-old", DocumentID: "doc-target", Type: "section", Body: kbBodyDef, Anchor: "old~3", InvalidAt: 9})

	rep, text := runLinker(t, f, `{"document_id":"doc-target","layers":["L1"]}`)

	l1 := layerReport(t, rep, "L1")
	if n := layerCount(t, rep, "L1", "parts"); n != 4 {
		t.Errorf("parts = %v, want 4 live non-root parts (the superseded one excluded)", n)
	}
	if n := layerCount(t, rep, "L1", "duplicate_anchors"); n != 1 {
		t.Errorf("duplicate_anchors = %v, want 1", n)
	}
	if n := layerCount(t, rep, "L1", "without_anchor"); n != 1 {
		t.Errorf("without_anchor = %v, want 1", n)
	}
	if n := layerCount(t, rep, "L1", "without_stable_id"); n != 1 {
		t.Errorf("without_stable_id = %v, want 1", n)
	}
	if c, _ := l1["collisions"].([]any); len(c) != 1 || c[0] != "c-dup" {
		t.Errorf("collisions = %v, want [c-dup]", l1["collisions"])
	}
	if strings.Contains(text, "notes~2") || strings.Contains(text, "TITLE-") {
		t.Errorf("the anchor report carries store text:\n%s", text)
	}
	assertNoWrites(t, f, rep)
}

// TestKBLinker_ReportModeWritesNothing: every layer runs against a store where
// each one has something to write, and the default write mode issues reads only,
// takes no lease, files no placeholder — and counts what it would have written.
func TestKBLinker_ReportModeWritesNothing(t *testing.T) {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	f.chunks[1].Body = kbBodyClaim + " The incident report goes to the regulator."
	f.citations = []map[string]any{
		kbCiteHit("to the regulator", map[string]any{"resolved": resolvedTo("x2")}),
		kbCiteHit("report a material incident", map[string]any{"work_key": "law:new"}),
	}
	f.chunks = append(f.chunks, kbChunk{ID: "c-gone-ev", DocumentID: "doc-target", Type: "section", Body: kbBodySelf, StableID: "s-1"})
	f.statements = []*kbStatement{
		{ID: "st-ok", From: "c1", To: "x1", Kind: "cites", Status: "asserted", Method: "rule:citation", CreatedAt: 1,
			Evidence: []*kbEvidence{{ID: "ev-ok", ChunkID: "c1", Quote: "report a material incident"}}},
		{ID: "st-stale", From: "c1", To: "x2", Kind: "cites", Status: "asserted", Method: "rule:citation", CreatedAt: 2,
			Evidence: []*kbEvidence{{ID: "ev-stale", ChunkID: "c-gone-ev", Quote: "a sentence no part says"}}},
		{ID: "st-gone", From: "c1", To: "x-deleted", Kind: "cites", Status: "proposed", Method: "rule:citation", CreatedAt: 3},
	}
	f.related["c1"] = nil

	rep, _ := runLinker(t, f, `{"document_id":"doc-target","scope":"tenant","terms_scheme":"topics","corpus_paths":["/laws"]}`)

	assertNoWrites(t, f, rep)
	ww := rep["would_write"].(map[string]any)
	for _, op := range []string{"propose_statement", "maintain_statement", "resolve_anchor"} {
		if ww[op].(float64) == 0 {
			t.Errorf("would_write.%s = 0 — the fixture gives this op something to do, so the count is the proof report mode reached it", op)
		}
	}
	if n := layerCount(t, rep, "L6", "actions.expire"); n != 1 {
		t.Errorf("L6 actions.expire = %v, want 1 counted (the statement whose end is gone)", n)
	}
	if rep["status"] != "complete" {
		t.Errorf("status = %v, want complete", rep["status"])
	}
}

// TestKBLinker_EveryOpTheBodyNamesHasAContract keeps kbOpContracts complete: an
// op the body starts calling without a row there is an op nobody checks against
// the runtime when it merges.
func TestKBLinker_EveryOpTheBodyNamesHasAContract(t *testing.T) {
	body := knowledgeBaseConfig(t).Agents["kb/linker"].Code
	known := map[string]bool{}
	for _, c := range kbOpContracts {
		known[c.Tool+"."+c.Op] = true
	}
	named := map[string]bool{}
	for _, m := range regexp.MustCompile(`op:\s*"([a-z_]+)"`).FindAllStringSubmatch(body, -1) {
		named["Document."+m[1]] = true
	}
	for _, m := range regexp.MustCompile(`\b(Channel|Agent)\.([a-z_]+)\(`).FindAllStringSubmatch(body, -1) {
		named[m[1]+"."+m[2]] = true
	}
	if len(named) < 10 {
		t.Fatalf("found only %d ops in the body — the scan is broken: %v", len(named), named)
	}
	for op := range named {
		if !known[op] {
			t.Errorf("the body calls %s, which kbOpContracts does not describe", op)
		}
	}
	for op := range known {
		if !named[op] {
			t.Errorf("kbOpContracts describes %s, which the body no longer calls", op)
		}
	}
}
