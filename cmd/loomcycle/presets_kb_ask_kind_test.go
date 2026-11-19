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
