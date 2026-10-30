package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// The maintenance layer (L6) of kb/linker, run through the real loop against the
// fixture store. Each check is driven from the store state that should trip it,
// next to a control the same run handles differently, so a check that never
// fires cannot pass.

// maintainCalls returns the maintain_statement calls for one statement, in order.
func maintainCalls(f *kbFixture, id string) []map[string]any {
	var out []map[string]any
	for _, c := range f.callsOf("Document", "maintain_statement") {
		if c.Input["id"] == id {
			out = append(out, c.Input)
		}
	}
	return out
}

func actionsOf(f *kbFixture, id string) string {
	var acts []string
	for _, in := range maintainCalls(f, id) {
		acts = append(acts, in["action"].(string))
	}
	return strings.Join(acts, ",")
}

// maintainFixture is the base store, a confirmed ontology, and a live corpus
// part x1 every statement below points at.
func maintainFixture() *kbFixture {
	f := kbBaseFixture()
	f.ontology = kbOntology()
	return f
}

const maintainInput = `{"mode":"maintain","document_id":"doc-target","write":"propose"}`

func citeStatement(id, from, to string, quotes ...string) *kbStatement {
	s := &kbStatement{ID: id, From: from, To: to, Kind: "cites", Status: "asserted", Method: "rule:citation", CreatedAt: 1}
	for i, q := range quotes {
		s.Evidence = append(s.Evidence, &kbEvidence{ID: fmt.Sprintf("%s-ev%d", id, i+1), ChunkID: from, Quote: q})
	}
	return s
}

// TestKBLinker_PersonStatementIsOnlyVerifiedOrFlagged: whatever check a person's
// own statement (method manual) fails, the pass only flags it, and a healthy one
// is verified — never expired, demoted, re-pointed, staled or moved. The
// machine-made twin of the first case is expired, so the flag is the person rule
// and not a dead check.
func TestKBLinker_PersonStatementIsOnlyVerifiedOrFlagged(t *testing.T) {
	f := maintainFixture()
	f.chunks = append(f.chunks,
		kbChunk{ID: "c-v1", DocumentID: "doc-target", Type: "section", Body: kbBodySelf, InvalidAt: 50, StableID: "art-9"},
		kbChunk{ID: "c-v2", DocumentID: "doc-target", Type: "section", Body: kbBodySelf, StableID: "art-9"})
	f.supersedes["c-v1"] = "c-v2"
	gone := citeStatement("m-gone", "c1", "x-deleted")
	gone.Method = "manual"
	kind := citeStatement("m-kind", "c1", "x1", "report a material incident")
	kind.Method, kind.Kind = "manual", "obsolete_kind"
	sup := citeStatement("m-sup", "c-v1", "x1", "keep a register")
	sup.Method = "manual"
	stale := citeStatement("m-stale", "c1", "x2", "a sentence no part says")
	stale.Method = "manual"
	ok := citeStatement("m-ok", "c1", "x1", "within 24 hours")
	ok.Method, ok.Kind = "manual", "amends"
	machine := citeStatement("r-gone", "c1", "x-deleted")
	f.statements = []*kbStatement{gone, kind, sup, stale, ok, machine}

	rep, _ := runLinker(t, f, maintainInput)

	for _, id := range []string{"m-gone", "m-kind", "m-sup", "m-stale"} {
		if got := actionsOf(f, id); got != "flag" {
			t.Errorf("%s: actions = %q, want flag — a person's statement is only flagged", id, got)
		}
	}
	if got := actionsOf(f, "m-ok"); got != "verify" {
		t.Errorf("m-ok: actions = %q, want verify", got)
	}
	if got := actionsOf(f, "r-gone"); got != "expire" {
		t.Errorf("r-gone: actions = %q, want expire — the machine-made twin is maintained", got)
	}
	for _, p := range proposals(f) {
		t.Errorf("the pass proposed %v on behalf of a person's statement", p)
	}
	noViolations(t, f)
	if rep["status"] != "complete" {
		t.Errorf("status = %v, want complete", rep["status"])
	}
}

// TestKBLinker_MovedQuoteIsRelocatedNotStaled: a quote no longer in its chunk but
// in the live part of the same document with the same stable id moved with the
// part: the evidence is re-pointed there. The control quote is in no part and is
// staled, so relocation is a decision, not a default.
func TestKBLinker_MovedQuoteIsRelocatedNotStaled(t *testing.T) {
	f := maintainFixture()
	f.chunks = append(f.chunks,
		kbChunk{ID: "c-ev-old", DocumentID: "doc-target", Type: "section", Body: "BODYOLD the text was rewritten here entirely.", StableID: "s-7"},
		kbChunk{ID: "c-ev-new", DocumentID: "doc-target", Type: "section", Body: "BODYNEW Every record is kept for five years after it.", StableID: "s-7"})
	s := citeStatement("st-moved", "c1", "x1")
	s.Evidence = []*kbEvidence{
		{ID: "ev-moved", ChunkID: "c-ev-old", Quote: "kept for five years"},
		{ID: "ev-lost", ChunkID: "c-ev-old", Quote: "nowhere in any part"},
	}
	f.statements = []*kbStatement{s}

	runLinker(t, f, maintainInput)

	calls := maintainCalls(f, "st-moved")
	if len(calls) != 2 {
		t.Fatalf("maintain calls = %v, want a relocation and a stale", calls)
	}
	if calls[0]["action"] != "relocate_evidence" || calls[0]["evidence_id"] != "ev-moved" || calls[0]["chunk_id"] != "c-ev-new" {
		t.Errorf("first call = %v, want ev-moved relocated to c-ev-new", calls[0])
	}
	if calls[1]["action"] != "stale_evidence" || calls[1]["evidence_id"] != "ev-lost" {
		t.Errorf("second call = %v, want ev-lost staled", calls[1])
	}
	noViolations(t, f)
}

// TestKBLinker_AllStaleEvidenceDemotes: when no quote of an asserted statement
// occurs anywhere any more, each row is staled and the statement goes back to
// review. One surviving quote keeps it asserted.
func TestKBLinker_AllStaleEvidenceDemotes(t *testing.T) {
	f := maintainFixture()
	allGone := citeStatement("st-all", "c1", "x1", "a sentence no part says", "nor this one either")
	oneLeft := citeStatement("st-one", "c1", "x2", "report a material incident", "a sentence no part says")
	f.statements = []*kbStatement{allGone, oneLeft}

	rep, _ := runLinker(t, f, maintainInput)

	if got := actionsOf(f, "st-all"); got != "stale_evidence,stale_evidence,demote" {
		t.Errorf("st-all actions = %q, want both rows staled then a demote", got)
	}
	if calls := maintainCalls(f, "st-all"); len(calls) == 3 && calls[2]["reason"] != "all evidence stale" {
		t.Errorf("demote reason = %v", calls[2]["reason"])
	}
	if got := actionsOf(f, "st-one"); got != "stale_evidence" {
		t.Errorf("st-one actions = %q, want only the lost row staled", got)
	}
	if n := layerCount(t, rep, "L6", "checks.evidence"); n != 2 {
		t.Errorf("checks.evidence = %v, want 2", n)
	}
}

// TestKBLinker_RenamedKindIsReproposedThenExpired: a statement whose kind the
// ontology renamed is filed again under the new name, same ends and evidence, and
// only then expired. A kind that is simply gone is demoted instead.
func TestKBLinker_RenamedKindIsReproposedThenExpired(t *testing.T) {
	f := maintainFixture()
	rels := f.ontology["relations"].([]any)
	f.ontology["relations"] = append(rels, map[string]any{"name": "based_on", "source": "tenant", "renamed_from": "grounded_in"})
	renamed := citeStatement("st-renamed", "c1", "x1", "report a material incident")
	renamed.Kind = "grounded_in"
	dropped := citeStatement("st-dropped", "c1", "x2", "within 24 hours")
	dropped.Kind = "obsolete_kind"
	f.statements = []*kbStatement{renamed, dropped}

	runLinker(t, f, maintainInput)

	var order []string
	for _, c := range f.calls {
		if c.Op == "propose_statement" || (c.Op == "maintain_statement" && c.Input["id"] == "st-renamed") {
			order = append(order, c.Op)
		}
	}
	if strings.Join(order, ",") != "propose_statement,maintain_statement" {
		t.Errorf("call order = %v, want the re-proposal before the expiry", order)
	}
	got := proposals(f)
	if len(got) != 1 {
		t.Fatalf("proposals = %v, want one re-proposal", got)
	}
	p := got[0]
	ev := p["evidence"].([]any)
	if p["kind"] != "based_on" || p["from_id"] != "c1" || p["to_id"] != "x1" || p["method"] != "rule:maintain" ||
		len(ev) != 1 || ev[0].(map[string]any)["quote"] != "report a material incident" {
		t.Errorf("re-proposal = %v, want c1 based_on x1 with the same evidence by rule:maintain", p)
	}
	calls := maintainCalls(f, "st-renamed")
	if len(calls) != 1 || calls[0]["action"] != "expire" || calls[0]["reason"] != "renamed to based_on" {
		t.Errorf("st-renamed maintain = %v, want expire \"renamed to based_on\"", calls)
	}
	calls = maintainCalls(f, "st-dropped")
	if len(calls) != 1 || calls[0]["action"] != "demote" || calls[0]["reason"] != "kind no longer declared" {
		t.Errorf("st-dropped maintain = %v, want demote \"kind no longer declared\"", calls)
	}
	noViolations(t, f)
}

// TestKBLinker_SupersededEndpointExpiresAndMovesOnlyWhenQuotesSurvive: a
// statement from a part a newer edition replaced is closed. One the citation
// grammar did not make is filed again on the successor — revision_of the
// original — only when every quote it had on the old part is still in the new
// one. (A citation is re-read by the grammar instead: the next test.)
func TestKBLinker_SupersededEndpointExpiresAndMovesOnlyWhenQuotesSurvive(t *testing.T) {
	f := maintainFixture()
	f.chunks = append(f.chunks,
		kbChunk{ID: "c-a1", DocumentID: "doc-target", Type: "section", Body: "Article 9. The operator shall keep a register of incidents.", InvalidAt: 50, Anchor: "art-9", StableID: "art-9"},
		kbChunk{ID: "c-a2", DocumentID: "doc-target", Type: "section", Body: "Article 9. The operator shall keep a register of incidents online.", Anchor: "art-9", StableID: "art-9"},
		kbChunk{ID: "c-b1", DocumentID: "doc-target", Type: "section", Body: "Article 10. Reports are due within 24 hours.", InvalidAt: 50, Anchor: "art-10", StableID: "art-10"},
		kbChunk{ID: "c-b2", DocumentID: "doc-target", Type: "section", Body: "Article 10. Reports are due within 72 hours.", Anchor: "art-10", StableID: "art-10"})
	f.supersedes["c-a1"] = "c-a2"
	// c-b1 has no supersedes edge: its successor is found by anchor.
	survives := citeStatement("st-survives", "c-a1", "x1", "keep a register of incidents")
	lost := citeStatement("st-lost", "c-b1", "x1", "due within 24 hours")
	survives.Method, lost.Method = "extractor:clauses", "extractor:clauses"
	f.statements = []*kbStatement{survives, lost}

	rep, _ := runLinker(t, f, maintainInput)

	got := proposals(f)
	if len(got) != 1 {
		t.Fatalf("proposals = %v, want only st-survives moved", got)
	}
	p := got[0]
	attrs, _ := p["attributes"].(map[string]any)
	if p["from_id"] != "c-a2" || p["to_id"] != "x1" || p["kind"] != "cites" || attrs["revision_of"] != "st-survives" ||
		p["method"] != "rule:maintain" {
		t.Errorf("successor proposal = %v, want c-a2 cites x1, revision_of st-survives, by rule:maintain", p)
	}
	for _, id := range []string{"st-survives", "st-lost"} {
		calls := maintainCalls(f, id)
		if len(calls) != 1 || calls[0]["action"] != "expire" || calls[0]["reason"] != "endpoint superseded" {
			t.Errorf("%s maintain = %v, want expire \"endpoint superseded\"", id, calls)
		}
	}
	if _, ok := p["valid_at"]; ok {
		t.Errorf("successor proposal carries valid_at %v, but the original had none", p["valid_at"])
	}
	if n := layerCount(t, rep, "L6", "successor_quotes_gone"); n != 1 {
		t.Errorf("successor_quotes_gone = %v, want 1", n)
	}
	noViolations(t, f)
}

// TestKBLinker_ImportedStatementsAreMaintainedLikeMachineOnes: a statement the
// import, sync or canvas path wrote is expired, demoted and moved like the
// linker's own (owner decision 2026-10-30); only a person's own is left to flag.
// A moved statement keeps the date its fact took effect, but not the old
// statement's confidence.
func TestKBLinker_ImportedStatementsAreMaintainedLikeMachineOnes(t *testing.T) {
	f := maintainFixture()
	f.chunks = append(f.chunks,
		kbChunk{ID: "c-a1", DocumentID: "doc-target", Type: "section", Body: "Article 9. The operator shall keep a register of incidents.", InvalidAt: 50, Anchor: "art-9", StableID: "art-9"},
		kbChunk{ID: "c-a2", DocumentID: "doc-target", Type: "section", Body: "Article 9. The operator shall keep a register of incidents online.", Anchor: "art-9", StableID: "art-9"})
	f.supersedes["c-a1"] = "c-a2"
	imp := citeStatement("st-import", "c1", "x-deleted")
	imp.Method = "import"
	syn := citeStatement("st-sync", "c1", "x1", "a sentence no part says", "nor this one either")
	syn.Method, syn.Attributes = "sync", map[string]any{"origin_method": "extractor:clauses"}
	can := citeStatement("st-canvas", "c-a1", "x1", "keep a register of incidents")
	can.Method, can.ValidAt = "canvas", 1704067200000000000
	f.statements = []*kbStatement{imp, syn, can}

	runLinker(t, f, maintainInput)

	if got := actionsOf(f, "st-import"); got != "expire" {
		t.Errorf("st-import actions = %q, want expire", got)
	}
	if got := actionsOf(f, "st-sync"); got != "stale_evidence,stale_evidence,demote" {
		t.Errorf("st-sync actions = %q, want both rows staled then a demote", got)
	}
	if got := actionsOf(f, "st-canvas"); got != "expire" {
		t.Errorf("st-canvas actions = %q, want expire after the move", got)
	}
	got := proposals(f)
	if len(got) != 1 {
		t.Fatalf("proposals = %v, want st-canvas moved to its successor", got)
	}
	p := got[0]
	if p["from_id"] != "c-a2" || p["method"] != "rule:maintain" {
		t.Errorf("successor proposal = %v, want from c-a2 by rule:maintain", p)
	}
	if v, _ := p["valid_at"].(float64); int64(v) != 1704067200000000000 {
		t.Errorf("successor valid_at = %v, want the original's 1704067200000000000", p["valid_at"])
	}
	if _, ok := p["confidence"]; ok {
		t.Errorf("successor proposal carries confidence %v; it starts unjudged", p["confidence"])
	}
	noViolations(t, f)
}

// TestKBLinker_RuntimeWrittenStatementIsNeverTouched: the edition chain's own
// supersedes edge points at a superseded part by design. Maintaining it would
// expire every such edge, so a runtime-written statement is left alone — as is a
// reserved kind written any other way.
func TestKBLinker_RuntimeWrittenStatementIsNeverTouched(t *testing.T) {
	f := maintainFixture()
	f.chunks = append(f.chunks,
		kbChunk{ID: "c-a1", DocumentID: "doc-target", Type: "section", Body: kbBodySelf, InvalidAt: 50, StableID: "art-9"},
		kbChunk{ID: "c-a2", DocumentID: "doc-target", Type: "section", Body: kbBodySelf, StableID: "art-9"})
	edge := &kbStatement{ID: "st-edge", From: "c-a2", To: "c-a1", Kind: "supersedes", Status: "asserted", Method: "runtime", CreatedAt: 1}
	mention := &kbStatement{ID: "st-mention", From: "c-a1", To: "x1", Kind: "mentions", Status: "asserted", Method: "rule:legacy", CreatedAt: 2}
	f.statements = []*kbStatement{edge, mention}

	rep, _ := runLinker(t, f, maintainInput)

	if n := len(f.callsOf("Document", "maintain_statement")); n != 0 {
		t.Errorf("maintain_statement called %d times on runtime-owned statements", n)
	}
	if n := layerCount(t, rep, "L6", "skipped_runtime"); n != 1 {
		t.Errorf("skipped_runtime = %v, want 1", n)
	}
	if n := layerCount(t, rep, "L6", "skipped_reserved_kind"); n != 1 {
		t.Errorf("skipped_reserved_kind = %v, want 1", n)
	}

	// Without a usable ontology nothing says supersedes is reserved: the method
	// alone must keep the edge out of the validity check that would expire it.
	g := maintainFixture()
	g.ontology = nil
	g.chunks = f.chunks
	g.statements = []*kbStatement{{ID: "st-edge", From: "c-a2", To: "c-a1", Kind: "supersedes", Status: "asserted",
		Method: "runtime", CreatedAt: 1}}
	runLinker(t, g, maintainInput)
	if n := len(g.callsOf("Document", "maintain_statement")); n != 0 {
		t.Errorf("without an ontology, maintain_statement called %d times on the runtime's own edge", n)
	}
}

// TestKBLinker_DuplicatesUnderARemovedDiscriminatorMerge: once a kind declares
// no discriminator, statements that differed only by one are the same relation;
// the newer is merged into the oldest, and only the oldest is verified.
func TestKBLinker_DuplicatesUnderARemovedDiscriminatorMerge(t *testing.T) {
	f := maintainFixture()
	older := citeStatement("st-older", "c1", "x1", "report a material incident")
	older.Discriminator, older.CreatedAt = "art-1", 10
	newer := citeStatement("st-newer", "c1", "x1", "within 24 hours")
	newer.Discriminator, newer.CreatedAt = "art-2", 20
	f.statements = []*kbStatement{newer, older}

	runLinker(t, f, maintainInput)

	calls := maintainCalls(f, "st-newer")
	if len(calls) != 1 || calls[0]["action"] != "merge" || calls[0]["survivor_id"] != "st-older" {
		t.Errorf("st-newer maintain = %v, want merge into st-older", calls)
	}
	if got := actionsOf(f, "st-older"); got != "verify" {
		t.Errorf("st-older actions = %q, want verify", got)
	}
	noViolations(t, f)
}

// TestKBLinker_OperatorAcceptedStatementIsOnlyVerifiedOrFlagged: a rule's
// statement an operator accepted carries that operator's judgement, so like a
// person's own it is only flagged when a check fails. Its unjudged twin is
// expired, so the flag is that rule and not a dead check.
func TestKBLinker_OperatorAcceptedStatementIsOnlyVerifiedOrFlagged(t *testing.T) {
	f := maintainFixture()
	accepted := citeStatement("st-accepted", "c1", "x-deleted")
	accepted.OperatorRuledAt = 1700000000000000000
	twin := citeStatement("st-twin", "c1", "x-deleted")
	twin.Kind = "amends"
	f.statements = []*kbStatement{accepted, twin}

	runLinker(t, f, maintainInput)

	if got := actionsOf(f, "st-accepted"); got != "flag" {
		t.Errorf("st-accepted actions = %q, want flag", got)
	}
	if got := actionsOf(f, "st-twin"); got != "expire" {
		t.Errorf("st-twin actions = %q, want expire", got)
	}
	noViolations(t, f)
}

// TestKBLinker_DuplicateOfAnotherStatusIsFlaggedNotMerged: the runtime merges
// only into a survivor of the same status, so a proposed duplicate of an
// asserted statement is left for review.
func TestKBLinker_DuplicateOfAnotherStatusIsFlaggedNotMerged(t *testing.T) {
	f := maintainFixture()
	older := citeStatement("st-older", "c1", "x1", "report a material incident")
	older.Discriminator, older.CreatedAt = "art-1", 10
	newer := citeStatement("st-newer", "c1", "x1", "within 24 hours")
	newer.Discriminator, newer.CreatedAt, newer.Status = "art-2", 20, "proposed"
	f.statements = []*kbStatement{newer, older}

	runLinker(t, f, maintainInput)

	if got := actionsOf(f, "st-newer"); got != "flag" {
		t.Errorf("st-newer actions = %q, want flag — never merged across statuses", got)
	}
	noViolations(t, f)
}

// TestKBLinker_SyncedStatementIsJudgedByTheMethodThatMadeIt: sync stamps its own
// method on the receiving side and keeps the first one in
// attributes.origin_method. A person's statement that arrived by sync is still a
// person's, so it is only flagged, and so is one synced before origin_method
// existed, which may be a person's; one a rule made is maintained as before.
func TestKBLinker_SyncedStatementIsJudgedByTheMethodThatMadeIt(t *testing.T) {
	f := maintainFixture()
	person := citeStatement("s-person", "c1", "x-deleted")
	person.Method, person.Attributes = "sync", map[string]any{"origin_method": "manual"}
	rule := citeStatement("s-rule", "c1", "x-deleted")
	rule.Method, rule.Attributes = "sync", map[string]any{"origin_method": "rule:citation"}
	unknown := citeStatement("s-unknown", "c1", "x-deleted")
	unknown.Method = "sync"
	f.statements = []*kbStatement{person, unknown, rule}

	runLinker(t, f, maintainInput)

	if got := actionsOf(f, "s-person"); got != "flag" {
		t.Errorf("s-person actions = %q, want flag — a person's statement stays one after sync", got)
	}
	if got := actionsOf(f, "s-unknown"); got != "flag" {
		t.Errorf("s-unknown actions = %q, want flag — with no origin_method it may be a person's", got)
	}
	if got := actionsOf(f, "s-rule"); got != "expire" {
		t.Errorf("s-rule actions = %q, want expire", got)
	}
}

// TestKBLinker_SelectionCoversEndsAndEvidenceAcrossPages: the touched set is
// every live statement with an end in the document or evidence citing it, of
// either status, over every page, each once.
func TestKBLinker_SelectionCoversEndsAndEvidenceAcrossPages(t *testing.T) {
	f := maintainFixture()
	f.statementsPage = 4 // 9 asserted statements span three pages
	// amends declares a discriminator here, so the 9 statements below that
	// differ only by it are distinct relations, not duplicates to merge.
	for _, r := range f.ontology["relations"].([]any) {
		if r.(map[string]any)["name"] == "amends" {
			r.(map[string]any)["discriminator"] = "anchor"
		}
	}
	f.chunks = append(f.chunks, kbChunk{ID: "c-cited", DocumentID: "doc-target", Type: "section", Body: kbBodySelf})
	f.docTypes["doc-other"] = "act"
	f.others["o1"] = kbChunk{ID: "o1", DocumentID: "doc-other", Type: "section", Body: kbBodyGloss}
	for i := 0; i < 9; i++ {
		f.statements = append(f.statements, citeStatement(fmt.Sprintf("st-%03d", i), "c1", "x1", "report a material incident"))
		f.statements[i].Discriminator = fmt.Sprintf("d%d", i) // distinct, so none merges
		f.statements[i].Kind = "amends"
	}
	byEvidence := citeStatement("st-ev", "o1", "x2")
	byEvidence.Evidence = []*kbEvidence{{ID: "ev-x", ChunkID: "c-cited", Quote: "keep a register"}}
	proposed := citeStatement("st-prop", "c1", "x2", "to the regulator")
	proposed.Status = "proposed"
	expired := citeStatement("st-expired", "c1", "x2", "to the regulator")
	expired.Expired = true
	f.statements = append(f.statements, byEvidence, proposed, expired)

	rep, _ := runLinker(t, f, `{"mode":"maintain","document_id":"doc-target","write":"propose","max_writes":1000}`)

	if n := layerCount(t, rep, "L6", "statements"); n != 11 {
		t.Errorf("statements = %v, want 11 (9 paged + one by evidence + one proposed, the expired one excluded)", n)
	}
	if got := actionsOf(f, "st-ev"); got != "verify" {
		t.Errorf("st-ev actions = %q, want verify — selected through its evidence", got)
	}
	if got := actionsOf(f, "st-expired"); got != "" {
		t.Errorf("st-expired actions = %q, want none", got)
	}
	verified := 0
	for _, c := range f.callsOf("Document", "maintain_statement") {
		if c.Input["action"] == "verify" {
			verified++
		}
	}
	if verified != 11 {
		t.Errorf("verified %d, want 11 — each statement exactly once", verified)
	}
}

// TestKBLinker_AutoLinkWhoseTextIsGoneIsCountedNotTouched: an auto statement — a
// [[name]] link the runtime keeps — whose source no longer contains its text is
// the one case a later version may delete by rule. This version counts it and
// touches nothing; one whose text is still there is not counted.
func TestKBLinker_AutoLinkWhoseTextIsGoneIsCountedNotTouched(t *testing.T) {
	f := maintainFixture()
	gone := &kbStatement{ID: "st-auto-gone", From: "c1", To: "x1", Kind: "references", Status: "asserted",
		Method: "wikilink", Auto: true, CreatedAt: 1,
		Evidence: []*kbEvidence{{ID: "ev-a", ChunkID: "c1", Quote: "[[a link removed from the text]]"}}}
	kept := &kbStatement{ID: "st-auto-kept", From: "c1", To: "x2", Kind: "mentions", Status: "asserted",
		Method: "runtime", Auto: true, CreatedAt: 2,
		Evidence: []*kbEvidence{{ID: "ev-b", ChunkID: "c1", Quote: "the regulator"}}}
	f.statements = []*kbStatement{gone, kept}

	rep, _ := runLinker(t, f, maintainInput)

	if n := layerCount(t, rep, "L6", "auto_rule_a_candidates"); n != 1 {
		t.Errorf("auto_rule_a_candidates = %v, want 1 (the link whose text is gone)", n)
	}
	if n := len(f.callsOf("Document", "maintain_statement")); n != 0 {
		t.Errorf("maintain_statement called %d times on auto statements", n)
	}
}

// hitOn is one extract_citations hit on chunk chunkID, at the rune span of quote
// in body.
func hitOn(chunkID, body, quote string, resolved map[string]any) map[string]any {
	at := strings.Index(body, quote)
	if at < 0 {
		panic("quote not in body: " + quote)
	}
	start := utf8.RuneCountInString(body[:at])
	return map[string]any{"chunk_id": chunkID, "quote": quote, "char_start": start,
		"char_end": start + utf8.RuneCountInString(quote), "kind": "cites", "resolved": resolved}
}

// TestKBLinker_SupersededCitationIsReReadByTheGrammar: a citation whose passage a
// new edition replaced is filed on the successor when the grammar still finds it
// there, with the hit's own quotes — not when its old quotes happen to survive.
//   - partial: the number form is gone, the title form stays: still cites x1;
//   - renamed: the same words now name a newer act: no successor, and the copy the
//     re-import made (its quote held) is expired before it can be verified;
//   - reflow: the line break moved, so no old quote survives: still cites x1;
//   - repoint: point 5 of an act became point 7: not a move, L2 files the new one.
func TestKBLinker_SupersededCitationIsReReadByTheGrammar(t *testing.T) {
	const (
		p1 = "The operator follows Resolution No 64 of 11 June 2018, the Risk Regulation, in full."
		p2 = "The operator follows the Risk Regulation in full."
		r1 = "Audits follow the Audit Regulation approved by Resolution No 311 of 2016."
		r2 = "Audits follow the Audit Regulation approved by Resolution No 151 of 2025."
		f1 = "Reports go to the regulator under the Law\non Banks and its rules."
		f2 = "Reports go to the regulator under the Law on\nBanks and its rules."
		g1 = "Limits are set by point 5 of the Law on Banks for every desk."
		g2 = "Limits are set by point 7 of the Law on Banks for every desk."
	)
	f := maintainFixture()
	f.chunks = append(f.chunks,
		kbChunk{ID: "c-p1", DocumentID: "doc-target", Type: "section", Body: p1, InvalidAt: 50, Anchor: "art-2", StableID: "art-2"},
		kbChunk{ID: "c-p2", DocumentID: "doc-target", Type: "section", Body: p2, Anchor: "art-2", StableID: "art-2"},
		kbChunk{ID: "c-r1", DocumentID: "doc-target", Type: "section", Body: r1, InvalidAt: 50, Anchor: "art-3", StableID: "art-3"},
		kbChunk{ID: "c-r2", DocumentID: "doc-target", Type: "section", Body: r2, Anchor: "art-3", StableID: "art-3"},
		kbChunk{ID: "c-f1", DocumentID: "doc-target", Type: "section", Body: f1, InvalidAt: 50, Anchor: "art-4", StableID: "art-4"},
		kbChunk{ID: "c-f2", DocumentID: "doc-target", Type: "section", Body: f2, Anchor: "art-4", StableID: "art-4"},
		kbChunk{ID: "c-g1", DocumentID: "doc-target", Type: "section", Body: g1, InvalidAt: 50, Anchor: "art-5", StableID: "art-5"},
		kbChunk{ID: "c-g2", DocumentID: "doc-target", Type: "section", Body: g2, Anchor: "art-5", StableID: "art-5"})
	f.supersedes["c-p1"], f.supersedes["c-r1"], f.supersedes["c-f1"], f.supersedes["c-g1"] = "c-p2", "c-r2", "c-f2", "c-g2"
	f.others["x-audit16"] = kbChunk{ID: "x-audit16", DocumentID: "doc-audit-2016", Type: "act", Body: kbBodyRule}
	partial := citeStatement("st-partial", "c-p1", "x1", "Resolution No 64 of 11 June 2018", "the Risk Regulation")
	renamed := citeStatement("st-renamed", "c-r1", "x-audit16", "the Audit Regulation", "Resolution No 311 of 2016")
	copied := citeStatement("st-copy", "c-r2", "x-audit16", "the Audit Regulation")
	// Both proposed, the copy newer: it is listed before the statement it copies.
	renamed.Status = "proposed"
	copied.Status, copied.CreatedAt, copied.Attributes = "proposed", 9, map[string]any{"revision_of": "st-renamed"}
	reflow := citeStatement("st-reflow", "c-f1", "x1", "the Law\non Banks")
	// Point 5 of an act, cited exactly; the new edition cites point 7 of it.
	repoint := citeStatement("st-repoint", "c-g1", "x2", "point 5 of the Law on Banks")
	f.statements = []*kbStatement{copied, partial, renamed, reflow, repoint}
	law := map[string]any{"document_id": "doc-law", "exact": false, "placeholder": false}
	newer := map[string]any{"document_id": "doc-audit-2025", "exact": false, "placeholder": false}
	f.citations = []map[string]any{
		hitOn("c-p2", p2, "the Risk Regulation", law),
		hitOn("c-r2", r2, "the Audit Regulation", newer),
		hitOn("c-r2", r2, "Resolution No 151 of 2025", newer),
		hitOn("c-f2", f2, "the Law on\nBanks", law),
		hitOn("c-g2", g2, "point 7 of the Law on Banks", map[string]any{"chunk_id": "x1", "document_id": "doc-law", "exact": true, "placeholder": false}),
	}

	rep, _ := runLinker(t, f, maintainInput)

	got := map[string]map[string]any{}
	for _, p := range proposals(f) {
		got[p["from_id"].(string)] = p
	}
	if len(got) != 2 || got["c-p2"] == nil || got["c-f2"] == nil {
		t.Fatalf("proposals = %v, want st-partial on c-p2 and st-reflow on c-f2 only", proposals(f))
	}
	for from, want := range map[string]struct{ rev, quote string }{
		"c-p2": {"st-partial", "the Risk Regulation"}, "c-f2": {"st-reflow", "the Law on\nBanks"},
	} {
		p := got[from]
		attrs, _ := p["attributes"].(map[string]any)
		ev, _ := p["evidence"].([]any)
		if p["to_id"] != "x1" || p["method"] != "rule:citation" || attrs["revision_of"] != want.rev ||
			len(ev) != 1 || ev[0].(map[string]any)["quote"] != want.quote {
			t.Errorf("successor from %s = %v, want → x1 by rule:citation, revision_of %s, quoting %q", from, p, want.rev, want.quote)
		}
	}
	for _, id := range []string{"st-partial", "st-renamed", "st-reflow", "st-repoint"} {
		if got := actionsOf(f, id); got != "expire" {
			t.Errorf("%s actions = %q, want expire", id, got)
		}
	}
	if got := actionsOf(f, "st-copy"); got != "expire" {
		t.Errorf("st-copy actions = %q, want expire only (never verified)", got)
	}
	if c := maintainCalls(f, "st-copy"); len(c) == 1 && c[0]["reason"] != "the new edition no longer cites this work" {
		t.Errorf("st-copy reason = %v", c[0]["reason"])
	}
	for counter, want := range map[string]float64{"successor_citation_gone": 2, "successor_copies_expired": 1, "successor_quotes_gone": 0} {
		if n := layerCount(t, rep, "L6", counter); n != want {
			t.Errorf("%s = %v, want %v", counter, n, want)
		}
	}
	noViolations(t, f)
}

// TestKBLinker_SupersededCitationWithoutAGrammarFallsBackToQuotes: when the
// document's grammar cannot be read, a citation is moved by the quote rule.
func TestKBLinker_SupersededCitationWithoutAGrammarFallsBackToQuotes(t *testing.T) {
	f := maintainFixture()
	f.grammarNull = true
	f.chunks = append(f.chunks,
		kbChunk{ID: "c-a1", DocumentID: "doc-target", Type: "section", Body: "Article 9. The operator shall keep a register of incidents.", InvalidAt: 50, Anchor: "art-9", StableID: "art-9"},
		kbChunk{ID: "c-a2", DocumentID: "doc-target", Type: "section", Body: "Article 9. The operator shall keep a register of incidents online.", Anchor: "art-9", StableID: "art-9"})
	f.supersedes["c-a1"] = "c-a2"
	f.statements = []*kbStatement{citeStatement("st-cite", "c-a1", "x1", "keep a register of incidents")}

	runLinker(t, f, maintainInput)

	got := proposals(f)
	if len(got) != 1 || got[0]["from_id"] != "c-a2" || got[0]["method"] != "rule:maintain" {
		t.Errorf("proposals = %v, want st-cite moved to c-a2 by the quote rule", got)
	}
	noViolations(t, f)
}

// TestKBLinker_ReflowedQuoteIsFoundAndQuotedVerbatim: a statement the grammar did
// not make moves when its quote's words survive with new line breaks, and the
// successor quotes the new part's own text. One surviving quote of two does not
// move a statement.
func TestKBLinker_ReflowedQuoteIsFoundAndQuotedVerbatim(t *testing.T) {
	const (
		a1 = "Article 9. The operator shall keep a register\nof incidents for five years."
		a2 = "Article 9. The operator shall keep a register of\nincidents for three years."
	)
	f := maintainFixture()
	f.chunks = append(f.chunks,
		kbChunk{ID: "c-a1", DocumentID: "doc-target", Type: "section", Body: a1, InvalidAt: 50, Anchor: "art-9", StableID: "art-9"},
		kbChunk{ID: "c-a2", DocumentID: "doc-target", Type: "section", Body: a2, Anchor: "art-9", StableID: "art-9"})
	f.supersedes["c-a1"] = "c-a2"
	moved := citeStatement("st-moved", "c-a1", "x1", "a register\nof incidents")
	half := citeStatement("st-half", "c-a1", "x2", "a register\nof incidents", "for five years")
	moved.Method, half.Method = "extractor:clauses", "extractor:clauses"
	f.statements = []*kbStatement{moved, half}

	rep, _ := runLinker(t, f, maintainInput)

	got := proposals(f)
	if len(got) != 1 || got[0]["from_id"] != "c-a2" || got[0]["to_id"] != "x1" {
		t.Fatalf("proposals = %v, want only st-moved on c-a2", got)
	}
	ev := got[0]["evidence"].([]any)[0].(map[string]any)
	want := "a register of\nincidents"
	start := utf8.RuneCountInString(a2[:strings.Index(a2, want)])
	if ev["quote"] != want || ev["char_start"] != float64(start) || ev["char_end"] != float64(start+utf8.RuneCountInString(want)) {
		t.Errorf("successor evidence = %v, want %q at runes [%d,%d)", ev, want, start, start+utf8.RuneCountInString(want))
	}
	for counter, want := range map[string]float64{"successor_quotes_loose": 1, "successor_quotes_gone": 1} {
		if n := layerCount(t, rep, "L6", counter); n != want {
			t.Errorf("%s = %v, want %v", counter, n, want)
		}
	}
	noViolations(t, f)
}

// TestKBLinker_RuledCopyOfAGoneCitationIsFlagged: the re-import carries an
// operator's ruling over to its copy. When the new edition no longer makes the
// citation, that copy is flagged for the operator, never expired.
func TestKBLinker_RuledCopyOfAGoneCitationIsFlagged(t *testing.T) {
	const (
		r1 = "Audits follow the Audit Regulation approved by Resolution No 311 of 2016."
		r2 = "Audits follow the Audit Regulation approved by Resolution No 151 of 2025."
	)
	f := maintainFixture()
	f.chunks = append(f.chunks,
		kbChunk{ID: "c-r1", DocumentID: "doc-target", Type: "section", Body: r1, InvalidAt: 50, Anchor: "art-3", StableID: "art-3"},
		kbChunk{ID: "c-r2", DocumentID: "doc-target", Type: "section", Body: r2, Anchor: "art-3", StableID: "art-3"})
	f.supersedes["c-r1"] = "c-r2"
	f.others["x-audit16"] = kbChunk{ID: "x-audit16", DocumentID: "doc-audit-2016", Type: "act", Body: kbBodyRule}
	orig := citeStatement("st-orig", "c-r1", "x-audit16", "the Audit Regulation")
	orig.Status = "proposed"
	ruled := citeStatement("st-ruled-copy", "c-r2", "x-audit16", "the Audit Regulation")
	ruled.OperatorRuledAt, ruled.Attributes = 7, map[string]any{"revision_of": "st-orig"}
	f.statements = []*kbStatement{orig, ruled}
	f.citations = []map[string]any{hitOn("c-r2", r2, "the Audit Regulation",
		map[string]any{"document_id": "doc-audit-2025", "exact": false, "placeholder": false})}

	runLinker(t, f, maintainInput)

	if got := actionsOf(f, "st-orig"); got != "expire" {
		t.Errorf("st-orig actions = %q, want expire", got)
	}
	if got := actionsOf(f, "st-ruled-copy"); got != "flag" {
		t.Errorf("st-ruled-copy actions = %q, want flag only (no expire, and no verify over the flag)", got)
	}
	if len(proposals(f)) != 0 {
		t.Errorf("proposals = %v, want none", proposals(f))
	}
	noViolations(t, f)
}

// relateStatement is a model-made statement from c1, last verified at 100.
func relateStatement(id, to, kind, method, quoteFrom string) *kbStatement {
	return &kbStatement{ID: id, From: "c1", To: to, Kind: kind, Status: "proposed", Method: method, CreatedAt: 1,
		VerifiedAt: 100, Evidence: []*kbEvidence{{ID: id + "-ev1", ChunkID: "c1", Quote: quoteFrom}}}
}

// rejudgeFixture holds three model-made statements from c1 and a person's:
//   - st-still (→ x1): nothing changed since it was verified;
//   - st-same (→ x2, whose body changed): the verdict finds the same relation;
//   - st-gone (→ x3, changed): the verdict finds none;
//   - st-person (manual, → x4, changed): a person's, never re-judged.
func rejudgeFixture() *kbFixture {
	f := maintainFixture()
	f.ontology = relateOntology()
	x2 := f.others["x2"]
	x2.RevisedAt = 200
	f.others["x2"] = x2
	f.others["x3"] = kbChunk{ID: "x3", DocumentID: "doc-law", Type: "section", Body: kbBodyDef, RevisedAt: 300}
	f.others["x4"] = kbChunk{ID: "x4", DocumentID: "doc-law", Type: "section", Body: kbBodyGloss, RevisedAt: 400}
	f.statements = []*kbStatement{
		relateStatement("st-still", "x1", "elaborates", "extractor:relate", "report a material incident"),
		relateStatement("st-same", "x2", "elaborates", "extractor:relate", "report a material incident"),
		relateStatement("st-gone", "x3", "elaborates", "extractor:relate", "report a material incident"),
		relateStatement("st-person", "x4", "elaborates", "manual", "report a material incident"),
	}
	f.verdictByNeedle = []needleReply{
		{Needle: "BODYCHARLIE", Reply: `[{"candidate": 1, "kind": "elaborates", "direction": "this_to_candidate",
		  "quote_from": "report a material incident to the regulator", "quote_to": "names the systems affected", "confidence": 0.7}]`},
		{Needle: "BODYDELTA", Reply: `[{"candidate": 1, "kind": "none"}]`},
	}
	return f
}

// TestKBLinker_RejudgeAsksAgainOnlyWhereTheTextChanged: with rejudge on, a
// model-made statement whose target changed since it was verified is judged again
// — verified when the relation is found again, flagged when it is not — and one
// whose text did not change is verified without a model call. A person's statement
// is never re-judged.
func TestKBLinker_RejudgeAsksAgainOnlyWhereTheTextChanged(t *testing.T) {
	f := rejudgeFixture()

	rep, _ := runLinker(t, f, `{"mode":"maintain","document_id":"doc-target","write":"propose","rejudge":true}`)

	if n := len(f.callsOf("Agent", "")); n != 2 {
		t.Errorf("verdict calls = %d, want 2 (st-same and st-gone)", n)
	}
	for id, want := range map[string]string{"st-still": "verify", "st-same": "verify", "st-gone": "flag", "st-person": "verify"} {
		if got := actionsOf(f, id); got != want {
			t.Errorf("%s actions = %q, want %q", id, got, want)
		}
	}
	for counter, want := range map[string]float64{"rejudged_same": 1, "rejudged_none": 1, "rejudge_unchanged": 1, "rejudged_different": 0} {
		if n := layerCount(t, rep, "L6", counter); n != want {
			t.Errorf("%s = %v, want %v", counter, n, want)
		}
	}
	if len(proposals(f)) != 0 {
		t.Errorf("proposals = %v, want none", proposals(f))
	}
	noViolations(t, f)

	// Without rejudge, no model call at all: the same store, every statement verified.
	g := rejudgeFixture()
	runLinker(t, g, maintainInput)
	if n := len(g.callsOf("Agent", "")); n != 0 {
		t.Errorf("verdict calls without rejudge = %d, want 0", n)
	}
}

// TestKBLinker_RejudgeProposesTheNewKindAsARevision: a model-made statement of a kind
// the relate layer may not file (stricter_than) is re-judged after its target changed
// and now reads as elaborates: the new kind is proposed as a revision of it (by id,
// with the new quotes), and it is flagged, never expired.
func TestKBLinker_RejudgeProposesTheNewKindAsARevision(t *testing.T) {
	f := rejudgeFixture()
	f.statements = []*kbStatement{relateStatement("st-strict", "x2", "stricter_than", "extractor:clauses", "within 24 hours")}

	rep, _ := runLinker(t, f, `{"mode":"maintain","document_id":"doc-target","write":"propose","rejudge":true}`)

	got := proposals(f)
	if len(got) != 1 {
		t.Fatalf("proposals = %v, want one revision", got)
	}
	p := got[0]
	ev, _ := p["evidence"].([]any)
	if p["id"] != "st-strict" || p["kind"] != "elaborates" || p["method"] != "extractor:relate" || len(ev) != 2 {
		t.Errorf("proposal = %v, want a revision of st-strict as elaborates with two quotes by extractor:relate", p)
	}
	if got := actionsOf(f, "st-strict"); got != "flag" {
		t.Errorf("st-strict actions = %q, want flag", got)
	}
	if n := layerCount(t, rep, "L6", "rejudged_different"); n != 1 {
		t.Errorf("rejudged_different = %v, want 1", n)
	}
	noViolations(t, f)
}

// TestKBLinker_RejudgingAnUnchangedCorpusProposesNothing: the Phase C gate — over a
// store where no text changed since its statements were verified, a rejudge pass
// makes no model call and proposes nothing.
func TestKBLinker_RejudgingAnUnchangedCorpusProposesNothing(t *testing.T) {
	f := rejudgeFixture()
	for _, id := range []string{"x2", "x3", "x4"} {
		c := f.others[id]
		c.RevisedAt = 50
		f.others[id] = c
	}

	rep, _ := runLinker(t, f, `{"mode":"maintain","document_id":"doc-target","write":"propose","rejudge":true}`)

	if n := len(f.callsOf("Agent", "")); n != 0 {
		t.Errorf("verdict calls = %d, want 0", n)
	}
	if len(proposals(f)) != 0 {
		t.Errorf("proposals = %v, want none", proposals(f))
	}
	if n := layerCount(t, rep, "L6", "rejudge_unchanged"); n != 3 {
		t.Errorf("rejudge_unchanged = %v, want 3 (the person's statement is not counted)", n)
	}
}

// subjectFixture: the target document stands for a subject (its root c-root is the
// subject), merged into doc-surv whose root is v-root. Three statements touch it:
//   - st-mach (rule:terms, x1 → c-root): moves to v-root, then expires;
//   - st-person (manual, x2 → c-root): flagged, nothing else;
//   - st-inner (rule:citation, c1 → x1): does not touch the subject itself, and is
//     checked as any statement is (verified).
func subjectFixture() *kbFixture {
	f := maintainFixture()
	f.docTypes["doc-surv"] = "subject"
	f.roots["doc-surv"] = "v-root"
	f.others["v-root"] = kbChunk{ID: "v-root", DocumentID: "doc-surv", Type: "subject", Body: ""}
	mach := &kbStatement{ID: "st-mach", From: "x1", To: "c-root", Kind: "uses_term", Status: "proposed", Method: "rule:terms",
		CreatedAt: 1, ValidAt: 1704067200000000000, Evidence: []*kbEvidence{{ID: "st-mach-ev1", ChunkID: "x1", Quote: "material incidents"}}}
	person := &kbStatement{ID: "st-person", From: "x2", To: "c-root", Kind: "uses_term", Status: "proposed", Method: "manual",
		CreatedAt: 1, Evidence: []*kbEvidence{{ID: "st-person-ev1", ChunkID: "x2", Quote: "incident report"}}}
	inner := citeStatement("st-inner", "c1", "x1", "report a material incident")
	f.statements = []*kbStatement{mach, person, inner}
	return f
}

// TestKBLinker_SubjectMergeMovesEveryStatementToTheSurvivor: in subject mode with
// merged_into, a machine statement with the subject as an end is filed again with the
// survivor in its place (same kind, evidence and date, revision_of the original) and
// then expired; a person's is flagged; a statement that does not end at the subject is
// only checked.
func TestKBLinker_SubjectMergeMovesEveryStatementToTheSurvivor(t *testing.T) {
	f := subjectFixture()

	rep, _ := runLinker(t, f, `{"mode":"subject","document_id":"doc-target","merged_into":"doc-surv","write":"propose"}`)

	got := proposals(f)
	if len(got) != 1 {
		t.Fatalf("proposals = %v, want st-mach moved", got)
	}
	p := got[0]
	attrs, _ := p["attributes"].(map[string]any)
	ev, _ := p["evidence"].([]any)
	if p["from_id"] != "x1" || p["to_id"] != "v-root" || p["kind"] != "uses_term" || attrs["revision_of"] != "st-mach" ||
		len(ev) != 1 || ev[0].(map[string]any)["quote"] != "material incidents" {
		t.Errorf("proposal = %v, want x1 uses_term v-root with st-mach's evidence, revision_of st-mach", p)
	}
	if v, _ := p["valid_at"].(float64); int64(v) != 1704067200000000000 {
		t.Errorf("valid_at = %v, want the original's", p["valid_at"])
	}
	for id, want := range map[string]string{"st-mach": "expire", "st-person": "flag", "st-inner": "verify"} {
		if got := actionsOf(f, id); got != want {
			t.Errorf("%s actions = %q, want %q", id, got, want)
		}
	}
	for counter, want := range map[string]float64{"merge_touched": 2, "merge_repointed": 1} {
		if n := layerCount(t, rep, "L6", counter); n != want {
			t.Errorf("%s = %v, want %v", counter, n, want)
		}
	}
	noViolations(t, f)
}

// TestKBLinker_SubjectModeWithoutAMergeOnlyChecks: subject mode without merged_into is
// the maintenance checks over the statements touching the subject, nothing moved.
func TestKBLinker_SubjectModeWithoutAMergeOnlyChecks(t *testing.T) {
	f := subjectFixture()

	runLinker(t, f, `{"mode":"subject","document_id":"doc-target","write":"propose"}`)

	if got := proposals(f); len(got) != 0 {
		t.Errorf("proposals = %v, want none", got)
	}
	for _, id := range []string{"st-mach", "st-person", "st-inner"} {
		if strings.Contains(actionsOf(f, id), "expire") {
			t.Errorf("%s was expired without a merge", id)
		}
	}
}

// TestKBLinker_MergedIntoOutsideSubjectModeIsRefused: merged_into means nothing to the
// other modes, and the subject cannot be merged into itself.
func TestKBLinker_MergedIntoOutsideSubjectModeIsRefused(t *testing.T) {
	for _, in := range []string{
		`{"mode":"maintain","document_id":"doc-target","merged_into":"doc-surv"}`,
		`{"mode":"subject","document_id":"doc-target","merged_into":"doc-target"}`,
		`{"mode":"subject","document_id":"doc-target","layers":["L2"]}`,
	} {
		f := subjectFixture()
		rep, _ := runLinker(t, f, in)
		if rep["status"] != "refused" || len(f.calls) != 0 {
			t.Errorf("%s: status %v after %d calls, want refused before any", in, rep["status"], len(f.calls))
		}
	}
}
