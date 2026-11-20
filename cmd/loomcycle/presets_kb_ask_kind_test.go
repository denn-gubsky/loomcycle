package main

import (
	"strings"
	"testing"
)

// kb/ask and questions that ask for every document of a kind
// (presets_kb_ask_test.go holds the fake these tests drive,
// presets_kb_ask_period_test.go the route helpers).

// listingTypeNote is the kind rule's fixed note, pinned word for word.
const listingTypeNote = "LISTING. The question asks for every document of a kind. List them with query_documents " +
	"filtered by type, with limit 1000. " + listingClasses + " Name every document listed, each with a link; never " +
	"pick a sample. Write COVERAGE: complete only when every class of the kind was listed and no listing returned its " +
	"limit."

// typePairs are minimal pairs: each question asks for every document of a
// kind; its twin names the kind with a topic, or asks what one kind says.
var typePairs = []struct{ pos, twin string }{
	{"Which implementation plans do we have?", "Which design notes cover the scheduler?"},
	{"Which design notes do we have?", "Which plans do we have for speech?"},
	{"List all design notes.", "What do our guides say about tenancy?"},
	{"What RFCs do we hold?", "Which security RFCs do we have?"},
	{"Which research papers are there?", "What are release notes?"},
	{"Show me all our guides.", "Which research shaped the routing design?"},
}

// TestKbAsk_KindListingRoutesToTheWalkerWithTheTypeNote: a question asking
// for every document of a kind goes to the walker with a note to list by
// type, include the sibling classes of the kind, and name every row.
// Measured: implementation plans, walker 30 to 34 of 35, survey 24 to 25;
// design notes, walker 13 of 13, survey 8.
func TestKbAsk_KindListingRoutesToTheWalkerWithTheTypeNote(t *testing.T) {
	for _, p := range typePairs {
		wantWalkerRoute(t, p.pos, "listing", listingTypeNote, false)
	}
}

// TestKbAsk_KindWithATopicStaysWithTheCascade: a kind narrowed by a topic, or
// a question about what one kind says, is a question about the topic.
func TestKbAsk_KindWithATopicStaysWithTheCascade(t *testing.T) {
	for _, p := range typePairs {
		wantCascade(t, p.twin)
	}
}

// TestKbAsk_QuickKindListingKeepsEveryRow: "briefly" on a listing by kind
// gets the listing's quick line, as a listing by status does.
func TestKbAsk_QuickKindListingKeepsEveryRow(t *testing.T) {
	f := &askFake{score: 0.9, answers: askAnswers("YES", "partial")}
	out := runAsk(t, f, "Briefly, which design notes do we have?", nil)
	wantSpawns(t, f, "kb/walker")
	if pr, _ := f.inputs[0]["prompt"].(string); !strings.HasSuffix(pr, "\n\n"+listingTypeNote+"\n\n"+listingQuickNote) {
		t.Errorf("the walker's prompt lacks the kind note and the listing quick line:\n%s", pr)
	}
	wantRouteLine(t, out, "ROUTE: walker | SCORE: none | CLASSIFIER: listing | RUNS: r1:completed | DEPTH: quick (wording)")
}

// authoredPairs are minimal pairs for a kind asked for by what we wrote:
// "have we written", "did we write", "we wrote", or the passive "have been
// written". The dev check of v1.163.0 (2026-11-20) sent the first to the
// survey, which found 32 to 33 of 34 implementation plans, while "which
// implementation plans do we have" listed all 34. Each twin keeps the verb
// but narrows the kind by a topic, names another subject, or has no list
// head.
var authoredPairs = []struct{ pos, twin string }{
	{"Which implementation plans have we written?", "Which plans have we written for speech?"},
	{"Which design notes did we write?", "Which design notes have we written about the scheduler?"},
	{"What RFCs have we drafted so far?", "Which plans has the owner written?"},
	{"List the guides we have written.", "Which guides did the team publish?"},
	{"Which research papers did we publish?", "Have we written any RFCs?"},
	{"Which proposals have we produced in total?", "We have written plans for speech."},
	{"Which runbooks have been written?", "Which runbooks have been written for the scheduler?"},
	{"What specs have we authored yet?", "How have we written our specs?"},
	{"Which reports we wrote?", "Which reports did they write?"},
	{"Which measurements has been created?", "Which measurements have we created from the soak?"},
}

// TestKbAsk_AuthoredKindListingRoutesToTheWalkerWithTheTypeNote: a kind
// asked for by what "we" wrote, made, created, drafted, produced, authored or
// published (or by the passive "have been written") is every document of
// that kind, as "do we have" is, and goes to the walker with the type note.
func TestKbAsk_AuthoredKindListingRoutesToTheWalkerWithTheTypeNote(t *testing.T) {
	for _, p := range authoredPairs {
		wantWalkerRoute(t, p.pos, "listing", listingTypeNote, false)
	}
}

// TestKbAsk_AuthoredKindTwinStaysWithTheCascade: the same verbs with a topic
// after the kind, another subject than "we", or no list head stay with the
// classifiers.
func TestKbAsk_AuthoredKindTwinStaysWithTheCascade(t *testing.T) {
	for _, p := range authoredPairs {
		wantCascade(t, p.twin)
	}
}

// TestKbAsk_AuthoredKindListingNamesTheOntologyClasses: with a confirmed
// ontology the authored form resolves the kind to its classes, as "do we
// have" does, and a topic after it still stays with the cascade.
func TestKbAsk_AuthoredKindListingNamesTheOntologyClasses(t *testing.T) {
	o := kindsOntology(true, "Runbook", "playbook")
	for _, tc := range []struct {
		q       string
		classes []string
	}{
		{"Which implementation plans have we written?", []string{"ImplementationPlan"}},
		{"Which playbooks did we write?", []string{"Runbook"}},
		{"Which design notes have been published?", []string{"DesignNote"}},
	} {
		wantOntologyWalk(t, o, tc.q, classesNote(listingTypeNote, tc.classes...))
	}
	wantOntologyCascade(t, o, "Which design notes have we written about the scheduler?")
}

// TestKbAsk_DatedAuthoredKindStaysDatedOnly: a kind asked for by what we
// wrote in a period is a dated question, not a listing of the whole kind:
// the period after the verb is not the end of the clause.
func TestKbAsk_DatedAuthoredKindStaysDatedOnly(t *testing.T) {
	wantWalkerRoute(t, "Which plans did we write in November 2026?", "dated", datedRangeNote, true)
}
