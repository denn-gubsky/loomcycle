package main

import (
	"regexp"
	"strings"
	"testing"
)

// kb/ask and questions that name a period by both its ends
// (presets_kb_ask_test.go holds the fake these tests drive; the helpers here
// serve presets_kb_ask_listing_test.go too).

// datedRangeNote is the period rule's fixed note, pinned word for word: it is
// trusted text the walker acts on, and must never carry a word of the
// question.
const datedRangeNote = "DATED. The question names a period with two ends (a month, a year, or between two dates). " +
	"List the documents created in it with query_documents: created_after the start of the period and created_before " +
	"the first instant after it (for a month, the start of the next month; for a year, 1 January of the next year), " +
	"and answer from that listing, then read what you need of each; a search alone does not reach every document of " +
	"a period. A month given without a year is the latest such month that has begun by TODAY (below): say which year " +
	"you took."

var todayRE = regexp.MustCompile(`^TODAY: \d{4}-\d{2}-\d{2} \(UTC\)\.$`)

// datedRangePairs are minimal pairs: each question names a period by both
// ends; its twin, a word or two apart, names a month or a year without
// bounding by it, or uses a month name as an ordinary word (may, march).
var datedRangePairs = []struct{ pos, twin string }{
	{"Which plans did we write in November 2026?", "What do the November 2026 release notes say?"},
	{"What did we publish during March?", "What did we learn from the march of versions?"},
	{"Which designs were added in May?", "Which designs may change?"},
	{"Which documents did we write in 2026?", "What changed in v2026?"},
	{"What did we write between 1 May and 1 June 2026?", "What is the difference between the May plan and the June plan?"},
	{"Which notes were opened in Oct 2026 and why?", "What is in May release notes?"},
	{"What was decided in June, and by whom?", "What is decided in the June planning?"},
}

var askMetadatas = map[string]map[string]any{"cascade": nil, "classifier=agent": {"classifier": "agent"}}

// wantWalkerRoute runs one question through kb/ask, both with the cascade and
// with classifier=agent, and checks it went to the walker with no classifier
// asked, under the given label, with the question then wantNote as its prompt.
// With today set the prompt ends with a TODAY line, checked by its shape.
func wantWalkerRoute(t *testing.T, q, label, wantNote string, today bool) {
	t.Helper()
	for name, metadata := range askMetadatas {
		t.Run(name+"/"+q, func(t *testing.T) {
			f := &askFake{score: 0.9, answers: askAnswers("YES", "partial")}
			out := runAsk(t, f, q, metadata)
			if f.decCalls != 0 {
				t.Errorf("decision calls = %d, want 0: the rule answers first", f.decCalls)
			}
			wantSpawns(t, f, "kb/walker")
			wantRouteLine(t, out, "ROUTE: walker | SCORE: none | CLASSIFIER: "+label+" | RUNS: r1:completed")
			pr, _ := f.inputs[0]["prompt"].(string)
			if today {
				i := strings.LastIndex(pr, "\n")
				if i < 0 || !todayRE.MatchString(pr[i+1:]) {
					t.Fatalf("the walker's prompt does not end with a TODAY line:\n%s", pr)
				}
				pr = pr[:i]
			}
			if want := q + "\n\n" + wantNote; pr != want {
				t.Errorf("the walker's prompt =\n%s\nwant\n%s", pr, want)
			}
			if !strings.HasPrefix(out, askWalkerReturned+"\n\nROUTE:") {
				t.Errorf("want the walker's answer:\n%s", out)
			}
		})
	}
}

// wantCascade runs one question through kb/ask, both with the cascade and
// with classifier=agent, and checks no walker rule took it: the classifier
// was asked and the overview went to the survey with the question alone.
func wantCascade(t *testing.T, q string) {
	t.Helper()
	for name, metadata := range askMetadatas {
		t.Run(name+"/"+q, func(t *testing.T) {
			f := &askFake{score: 0.9, answers: askAnswers("YES", "complete")}
			out := runAsk(t, f, q, metadata)
			if metadata == nil {
				if f.decCalls != 1 {
					t.Errorf("decision calls = %d, want 1", f.decCalls)
				}
				wantSpawns(t, f, "kb/survey")
				wantRouteLine(t, out, "ROUTE: survey | SCORE: 0.9 | CLASSIFIER: decision | RUNS: r1:completed")
			} else {
				wantSpawns(t, f, "kb/classify", "kb/survey")
				wantRouteLine(t, out, "ROUTE: survey | SCORE: none | CLASSIFIER: agent | RUNS: r1:completed, r2:completed")
			}
			if pr, _ := f.inputs[len(f.inputs)-1]["prompt"].(string); pr != q {
				t.Errorf("the survey's prompt = %q, want the question alone", pr)
			}
		})
	}
}

// TestKbAsk_PeriodQuestionRoutesToTheWalkerWithBothBounds: a question naming
// a period by both its ends ("in November 2026", "during March", "in 2026",
// "between 1 May and 1 June 2026") wants a listing created inside it, which
// only the walker makes. Measured: "which plans did we write in November
// 2026" went to the survey and named 5 of 10 every run. The walker's note
// says to bound both ends, and how to read a month with no year, followed by
// the run's own date, never a word of the question.
func TestKbAsk_PeriodQuestionRoutesToTheWalkerWithBothBounds(t *testing.T) {
	for _, p := range datedRangePairs {
		wantWalkerRoute(t, p.pos, "dated", datedRangeNote, true)
	}
}

// TestKbAsk_UnboundedTwinOfAPeriodQuestionStaysWithTheCascade: a month or a
// year that does not bound the question (before a noun, as an ordinary word,
// inside a version name) leaves it to the classifiers as before.
func TestKbAsk_UnboundedTwinOfAPeriodQuestionStaysWithTheCascade(t *testing.T) {
	for _, p := range datedRangePairs {
		wantCascade(t, p.twin)
	}
}

// TestKbAsk_OverviewWithoutAListingShapeIsUnchanged: an overview question with
// no listing, status, kind or period shape gives exactly what it gave before
// these rules, depth from the wording included.
func TestKbAsk_OverviewWithoutAListingShapeIsUnchanged(t *testing.T) {
	for _, tc := range []struct{ q, tail, note string }{
		{"What do we hold on the external-document catalogue?", "", ""},
		{"Briefly, what do we hold on the external-document catalogue?", " | DEPTH: quick (wording)",
			"\n\nDEPTH: quick. The caller wants a short answer: the few documents that answer best, one short line each; do not widen."},
		{"What is the classification graph, and how much of it is built?", "", ""},
	} {
		t.Run(tc.q, func(t *testing.T) {
			f := &askFake{score: 0.9, answers: askAnswers("YES", "complete")}
			out := runAsk(t, f, tc.q, nil)
			if want := askSurveyAnswer + "\n\nROUTE: survey | SCORE: 0.9 | CLASSIFIER: decision | RUNS: r1:completed" + tc.tail; out != want {
				t.Errorf("answer =\n%s\nwant\n%s", out, want)
			}
			if pr, _ := f.inputs[0]["prompt"].(string); pr != tc.q+tc.note {
				t.Errorf("the survey's prompt = %q, want %q", pr, tc.q+tc.note)
			}
		})
	}
}
