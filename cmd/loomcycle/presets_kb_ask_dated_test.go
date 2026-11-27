package main

import (
	"strings"
	"testing"
)

// kb/ask and questions bounded by a date or a version (presets_kb_ask_test.go
// holds the fake these tests drive).

// datedPairs are minimal pairs: each dated question is bounded by a date or a
// version, and its twin, a word or two apart, is not.
var datedPairs = []struct{ dated, undated string }{
	{"Which definition kinds have we added since v1.0?", "Which definition kinds does v1.0 have?"},
	{"Which RFCs were opened since 9 November 2026?", "What do the November release notes say?"},
	{"What did we write after November 9th?", "What did we write about the November planning?"},
	{"Which documents were created after 2026-11-09?", "Which documents describe the 2026 roadmap?"},
	{"What changed since version 1.2?", "What changed since the tool became read-only?"},
	{"Which designs were added in the last 10 days?", "Which designs were added to the last release?"},
	{"What have we published in the past two weeks?", "What have we published about the past incidents?"},
	{"Which notes were opened since last month?", "Since when do we keep notes?"},
}

// TestKbAsk_DatedQuestionRoutesToTheWalkerWithoutAClassifier: a question
// bounded by a date or a version wants a listing by creation date, which only
// the walker makes. Measured: "which definition kinds since v1.0" failed in
// every run of every arm, and for "RFCs opened since 9 November" the walker
// found all 16 by date where the survey found 8 or 9. The rule in code sends
// it to kb/walker, with a fixed note to list by date, before either classifier
// is asked, also under classifier=agent.
func TestKbAsk_DatedQuestionRoutesToTheWalkerWithoutAClassifier(t *testing.T) {
	for _, p := range datedPairs {
		for name, metadata := range map[string]map[string]any{"cascade": nil, "classifier=agent": {"classifier": "agent"}} {
			t.Run(name+"/"+p.dated, func(t *testing.T) {
				f := &askFake{score: 0.9, answers: askAnswers("YES", "partial")}
				out := runAsk(t, f, p.dated, metadata)
				if f.decCalls != 0 {
					t.Errorf("decision calls = %d, want 0: the rule answers first", f.decCalls)
				}
				wantSpawns(t, f, "kb/walker")
				wantRouteLine(t, out, "ROUTE: walker | SCORE: none | CLASSIFIER: dated | RUNS: r1:completed")
				if pr, _ := f.inputs[0]["prompt"].(string); !strings.Contains(pr, "\n\nDATED. ") || !strings.Contains(pr, "created_after") {
					t.Errorf("the walker's prompt lacks the dated note:\n%s", pr)
				}
				if !strings.HasPrefix(out, askWalkerReturned+"\n\nROUTE:") {
					t.Errorf("want the walker's answer:\n%s", out)
				}
			})
		}
	}
}

// TestKbAsk_UndatedTwinOfADatedQuestionStaysWithTheCascade: the twin of each
// dated question names a date or a version without bounding the question by
// it, or says "since" with no date after it, and goes to the classifiers as
// before.
func TestKbAsk_UndatedTwinOfADatedQuestionStaysWithTheCascade(t *testing.T) {
	for _, p := range datedPairs {
		t.Run(p.undated, func(t *testing.T) {
			f := &askFake{score: 0.9, answers: askAnswers("YES", "complete")}
			out := runAsk(t, f, p.undated, nil)
			if f.decCalls != 1 {
				t.Errorf("decision calls = %d, want 1", f.decCalls)
			}
			wantSpawns(t, f, "kb/survey")
			wantRouteLine(t, out, "ROUTE: survey | SCORE: 0.9 | CLASSIFIER: decision | RUNS: r1:completed")
		})
	}
}

// TestKbAsk_DateInASituationBeforeTheQuestionStaysWithTheCascade: a question
// that tells a situation before it asks (ConditionalQA's shape) states when
// something happened; that date does not bound what is asked. A date bound
// counts only after a list head, in that head's sentence. Measured on
// ConditionalQA: of 17 questions the dated rules took, 15 matched only a
// period in the situation. Each of these goes to the classifiers like any
// undated question.
func TestKbAsk_DateInASituationBeforeTheQuestionStaysWithTheCascade(t *testing.T) {
	for _, q := range []string{
		"I moved to the UK in 2019 and have worked here since. Can I claim the benefit?",
		"My husband died after 2019-03-01. What support can I get?",
		"I started work in March 2021, and my employer has not paid me since last month. Am I entitled to holiday pay?",
		"The office moved in 2019.",
		"We have rented the flat since 2018, what can the landlord charge us for?",
	} {
		wantCascade(t, q)
	}
}

// TestKbAsk_DatedClauseAfterASituationStillRoutesToTheWalker: a situation
// with a date of its own does not stop a list-headed clause after it from
// being dated: the bound in that clause still sends the question to the
// walker with its dated note.
func TestKbAsk_DatedClauseAfterASituationStillRoutesToTheWalker(t *testing.T) {
	wantWalkerRoute(t, "I moved to the UK in 2019. Which benefits were introduced since 2020?", "dated", datedNote, false)
	wantWalkerRoute(t, "We started the project in 2024. Which plans did we write in November 2026?", "dated", datedRangeNote, true)
}
