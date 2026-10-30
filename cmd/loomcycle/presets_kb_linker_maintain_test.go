package main

import (
	"fmt"
	"strings"
	"testing"
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
// statement from a part a newer edition replaced is closed. It is filed again on
// the successor — revision_of the original — only when every quote it had on the
// old part is still in the new one.
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
