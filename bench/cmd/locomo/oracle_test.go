package main

import (
	"strings"
	"testing"
)

func oracleConv() Conversation {
	return Conversation{
		SampleID: "t1",
		Turns: []Turn{
			{DiaID: "D1:1", Session: 1, Speaker: "Caroline", Text: "I went to the support group.", DateTime: "1:56 pm on 8 May, 2023"},
			{DiaID: "D1:2", Session: 1, Speaker: "Melanie", Text: "For walking or running?", DateTime: "1:57 pm on 8 May, 2023"},
			{DiaID: "D1:3", Session: 1, Speaker: "Caroline", Text: "Running.", DateTime: "1:58 pm on 8 May, 2023"},
			{DiaID: "D1:4", Session: 1, Speaker: "Melanie", Text: "Nice.", DateTime: "1:59 pm on 8 May, 2023"},
			// A second session: the window must not walk across the boundary.
			{DiaID: "D2:1", Session: 2, Speaker: "Caroline", Text: "Unrelated later talk.", DateTime: "2:00 pm on 9 May, 2023"},
		},
	}
}

// TestAttachOracleEvidence_RendersOnlyTheSupportingTurns at window 0.
//
// The oracle is a CEILING measurement: it hands the reader what a perfect retriever
// would have found. Handing it the whole conversation would measure long-context
// reading instead, and handing it the wrong turns would measure nothing at all.
// Width 0 is kept precisely so LoCoMo's own annotation stays measurable.
func TestAttachOracleEvidence_RendersOnlyTheSupportingTurns(t *testing.T) {
	qs := []Query{{Question: "When did Caroline go?", Expected: []string{"D1:1"}, Answer: "8 May 2023"}}
	got, cov := AttachOracleEvidence(oracleConv(), qs, 0)
	if cov.Unresolved != 0 || cov.Partial != 0 {
		t.Fatalf("coverage = %+v, want nothing unresolved or partial", cov)
	}
	ev := got[0].Evidence
	if !strings.Contains(ev, "I went to the support group.") {
		t.Errorf("evidence does not carry the supporting turn: %q", ev)
	}
	if strings.Contains(ev, "For walking or running?") || strings.Contains(ev, "Nice.") {
		t.Errorf("window 0 carried turns that are NOT the answer key — that measures "+
			"long-context reading, not the annotation's ceiling: %q", ev)
	}
	// The DATE must survive: the temporal slice is the category the whole plan turns
	// on, and a turn without its stamp cannot answer "when".
	if !strings.Contains(ev, "8 May, 2023") {
		t.Errorf("evidence dropped the turn's timestamp — temporal questions become "+
			"unanswerable for a reason that is not the reader: %q", ev)
	}
}

// TestAttachOracleEvidence_WindowRestoresTheAnswerTurn is the regression for what the
// first instrument check caught on the real corpus: LoCoMo annotates the turn that
// ASKS ("for walking or running?") and not the one that ANSWERS ("running"), so a
// width-0 oracle hands the reader a prompt that provably cannot answer the question
// and a faithful reader must abstain. deepseek scored 0.6747 that way — BELOW its own
// 0.7877 traces arm — while reading correctly.
func TestAttachOracleEvidence_WindowRestoresTheAnswerTurn(t *testing.T) {
	qs := []Query{{Question: "What are the new shoes for?", Expected: []string{"D1:2"}, Answer: "Running"}}

	narrow, _ := AttachOracleEvidence(oracleConv(), qs, 0)
	if strings.Contains(narrow[0].Evidence, "Running.") {
		t.Fatal("the fixture no longer reproduces the off-by-one: the annotated turn " +
			"already holds the answer, so this test proves nothing")
	}

	wide, _ := AttachOracleEvidence(oracleConv(), qs, 1)
	if !strings.Contains(wide[0].Evidence, "Running.") {
		t.Errorf("window 1 did not reach the answering turn: %q", wide[0].Evidence)
	}
	if !strings.Contains(wide[0].Evidence, "For walking or running?") {
		t.Errorf("window 1 dropped the annotated turn itself: %q", wide[0].Evidence)
	}
}

// TestAttachOracleEvidence_WindowStopsAtTheSessionBoundary. Sessions are days apart;
// walking across one would pull in unrelated conversation and inflate the ceiling
// with context a retriever anchored on this turn would never return.
func TestAttachOracleEvidence_WindowStopsAtTheSessionBoundary(t *testing.T) {
	qs := []Query{{Question: "q", Expected: []string{"D1:4"}}}
	got, _ := AttachOracleEvidence(oracleConv(), qs, 3)
	if strings.Contains(got[0].Evidence, "Unrelated later talk.") {
		t.Errorf("the window crossed into the next session: %q", got[0].Evidence)
	}
	if !strings.Contains(got[0].Evidence, "I went to the support group.") {
		t.Errorf("the window did not reach back within its own session: %q", got[0].Evidence)
	}
}

// TestAttachOracleEvidence_OverlappingWindowsRenderATurnOnce. Two adjacent
// annotations produce overlapping windows, and the same turn printed twice reads to
// the model as two separate statements of the same thing.
func TestAttachOracleEvidence_OverlappingWindowsRenderATurnOnce(t *testing.T) {
	qs := []Query{{Question: "q", Expected: []string{"D1:2", "D1:3"}}}
	got, cov := AttachOracleEvidence(oracleConv(), qs, 1)
	if n := strings.Count(got[0].Evidence, "Running."); n != 1 {
		t.Errorf("the overlapping turn was rendered %d times: %q", n, got[0].Evidence)
	}
	if cov.Turns != strings.Count(strings.TrimSpace(got[0].Evidence), "\n") {
		t.Errorf("reported %d turns rendered, but the block holds %d lines of turns",
			cov.Turns, strings.Count(strings.TrimSpace(got[0].Evidence), "\n"))
	}
}

// TestAttachOracleEvidence_RendersChronologically. The annotation's order is
// arbitrary; a "[date] Speaker:" sequence out of order reads as a different
// conversation from the one that happened.
func TestAttachOracleEvidence_RendersChronologically(t *testing.T) {
	qs := []Query{{Question: "q", Expected: []string{"D1:3", "D1:1"}}}
	got, _ := AttachOracleEvidence(oracleConv(), qs, 0)
	ev := got[0].Evidence
	if strings.Index(ev, "I went to the support group.") > strings.Index(ev, "Running.") {
		t.Errorf("evidence is not in conversation order: %q", ev)
	}
}

// TestOraclePrompt_QuestionComesLast. Order is not cosmetic: a reader takes the last
// thing it reads as the instruction, and burying the question above several turns of
// transcript is how a small model answers about the wrong entity.
func TestOraclePrompt_QuestionComesLast(t *testing.T) {
	q := Query{Question: "When did Caroline go?", Evidence: "Conversation turns:\n[x] Caroline: hi\n"}
	p := oraclePrompt(q)
	if strings.Index(p, "Caroline: hi") > strings.Index(p, "When did Caroline go?") {
		t.Errorf("the question does not come last:\n%s", p)
	}
}

// TestOraclePrompt_IsInertWithoutEvidence. Every other mode shares this path, so the
// oracle must cost exactly nothing when it is not the arm being run.
func TestOraclePrompt_IsInertWithoutEvidence(t *testing.T) {
	q := Query{Question: "plain question"}
	if got := oraclePrompt(q); got != "plain question" {
		t.Errorf("oraclePrompt altered a non-oracle prompt: %q", got)
	}
}

// TestAttachOracleEvidence_CountsWhatItCannotResolve. A question whose evidence will
// not resolve must be REPORTED, not answered from an empty block — that would read as
// a reading failure when it is a dataset one.
func TestAttachOracleEvidence_CountsWhatItCannotResolve(t *testing.T) {
	qs := []Query{{Question: "q", Expected: []string{"D9:99"}}}
	got, cov := AttachOracleEvidence(oracleConv(), qs, 2)
	if cov.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1", cov.Unresolved)
	}
	if got[0].Evidence != "" {
		t.Errorf("an unresolvable question got an evidence block: %q", got[0].Evidence)
	}
}

// TestAttachOracleEvidence_CountsPartialResolutionSeparately.
//
// ⚠️ This is the counter that did not exist. A question whose 3 ids resolve to 1 was
// reported as fully attached while the reader held a third of the answer key — the
// same silent-truncation shape as a seed list cut by a limit, and indistinguishable
// in the report from a reading failure.
func TestAttachOracleEvidence_CountsPartialResolutionSeparately(t *testing.T) {
	qs := []Query{{Question: "q", Expected: []string{"D1:1", "D9:99"}}}
	got, cov := AttachOracleEvidence(oracleConv(), qs, 0)
	if cov.Partial != 1 {
		t.Errorf("Partial = %d, want 1", cov.Partial)
	}
	if cov.Unresolved != 0 {
		t.Errorf("Unresolved = %d: a question that resolved SOME ids is not unresolved", cov.Unresolved)
	}
	if !strings.Contains(got[0].Evidence, "I went to the support group.") {
		t.Errorf("the resolvable half was dropped too: %q", got[0].Evidence)
	}
}
