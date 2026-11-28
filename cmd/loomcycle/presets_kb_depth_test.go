package main

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Retrieval depth: an asking agent that does not need the deep answer says
// depth quick (run metadata), or kb/ask reads it from the question's wording,
// and kb/survey retrieves less. presets_kb_ask_test.go and
// presets_kb_survey_test.go hold the fakes these tests drive.

// runSurveyMeta runs kb/survey on content blocks with run metadata.
func runSurveyMeta(t *testing.T, f *surveyFake, content []loop.PromptContentBlock, metadata map[string]any) string {
	t.Helper()
	agent := knowledgeBaseConfig(t).Agents["kb/survey"]
	set := []tools.Tool{&surveyFakeAgent{f: f}, &surveyFakeDocument{f: f}}
	ctx := tools.WithRunID(context.Background(), "run-survey")
	res, err := loop.Run(ctx, loop.RunOptions{
		Provider:   codejs.New(codejs.Config{CodeRoot: t.TempDir(), RunTimeout: codejs.DefaultRunTimeout}),
		Model:      "code-js",
		AgentName:  "kb/survey",
		CodeBody:   agent.Code,
		Tools:      set,
		Dispatcher: tools.NewDispatcher(set),
		Metadata:   metadata,
		Segments:   []loop.PromptSegment{{Role: "user", Content: content}},
	})
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	t.Logf("answer:\n%s", res.FinalText)
	return res.FinalText
}

func trustedQ(q string) []loop.PromptContentBlock {
	return []loop.PromptContentBlock{{Type: "trusted-text", Text: q}}
}

// depthFake is 50 documents hit by the passage search, the first a large
// guide with one matching section, and 70 relations from each document to
// count how many documents relations are followed from. The screening round
// holding the first document fails; docs[12] fits.
func depthFake() (*surveyFake, []string) {
	docs := docIDs(1, 50)
	f := &surveyFake{
		rows:        map[string][]map[string]any{"P": append([]map[string]any{secHit(docs[0], secID(1))}, hits(docs...)...)},
		chunkCounts: map[string]int{docs[0]: 40},
		edges:       map[string][]map[string]any{},
	}
	f.screen = func(ids []string) surveyReply {
		for _, id := range ids {
			if id == docs[0] {
				return surveyReply{status: "failed"}
			}
		}
		return fitsOf(docs[12])(ids)
	}
	return f, docs
}

// TestKbSurvey_QuickDepthCapsTheSurveyAndSaysPartial: depth quick screens at
// most 30 documents in one wave (3 rounds, no retry wave), screens and links
// no section of a large guide, follows relations from at most 15 documents,
// and asks the composing round for a short answer. Everything it left out is
// counted, so its coverage is partial, and the trailer says depth quick.
func TestKbSurvey_QuickDepthCapsTheSurveyAndSaysPartial(t *testing.T) {
	f, docs := depthFake()
	out := runSurveyMeta(t, f, trustedQ(surveyQuestion), map[string]any{"depth": "quick"})
	if got := f.screened(); len(got) != 30 {
		t.Errorf("screened = %d ids, want 30", len(got))
	}
	for _, id := range f.screened() {
		if isSecID(id) {
			t.Errorf("a section was screened at depth quick: %s", id)
		}
	}
	if len(f.batches) != 1 || f.batches[0] != 3 {
		t.Errorf("screening spawns = %v, want one of 3 rounds (no retry wave)", f.batches)
	}
	if len(f.edgeIDs) > 15 {
		t.Errorf("relations followed from %d documents, want at most 15", len(f.edgeIDs))
	}
	p := spawnPrompt(composeSpawn(t, f))
	if !strings.Contains(p, "The caller asked for a quick answer") || len(listIn(p, "\nMatching sections of fitting documents (")) != 0 {
		t.Errorf("the composing round must ask for a short answer and link no section:\n%s", p)
	}
	if w := writeIDs(p); strings.Join(w, ",") != docs[12] {
		t.Errorf("composed ids = %v, want %s", w, docs[12])
	}
	cov, unc, sv := surveyLines(out)
	if cov != "COVERAGE: partial" || !strings.HasPrefix(unc, "UNCOVERED: 12 candidates not screened; 20 beyond the cap of 30") ||
		!strings.Contains(unc, "; 1 matching sections of large documents left out at depth quick") {
		t.Errorf("coverage = %q %q, want partial naming the unscreened round, the cut and the guide's section", cov, unc)
	}
	if !strings.Contains(sv, "| rounds 4 | depth quick | RUNS: ") {
		t.Errorf("survey line = %q, want 4 rounds at depth quick", sv)
	}
}

// TestKbSurvey_DefaultDepthIsStandardAndUnchanged: with no depth asked, the
// survey keeps every document up to 120, screens the guide's section,
// retries the failed round as two halves, follows relations from up to 60
// documents, and says depth standard.
func TestKbSurvey_DefaultDepthIsStandardAndUnchanged(t *testing.T) {
	f, _ := depthFake()
	out := runSurveyMeta(t, f, trustedQ(surveyQuestion), nil)
	nsec := 0
	for _, id := range f.screened() {
		if isSecID(id) {
			nsec++
		}
	}
	// 50 documents, the failed round's 12 again as two halves, and the
	// section beside them.
	if got := len(f.screened()); got != 50+12+nsec || nsec != 1 {
		t.Errorf("screened = %d ids with %d sections, want 50 documents, 12 retried and 1 section", got, nsec)
	}
	if len(f.batches) != 2 {
		t.Errorf("screening spawns = %v, want the documents, then the retries beside the section", f.batches)
	}
	if len(f.edgeIDs) != 50 {
		t.Errorf("relations followed from %d documents, want all 50 (under the 60 of standard)", len(f.edgeIDs))
	}
	if p := spawnPrompt(composeSpawn(t, f)); strings.Contains(p, "quick answer") {
		t.Errorf("a standard composing round asks for a quick answer:\n%s", p)
	}
	if _, _, sv := surveyLines(out); !strings.Contains(sv, "| depth standard | RUNS: ") {
		t.Errorf("survey line = %q, want depth standard", sv)
	}
}

// TestKbSurvey_DeepDepthFollowsMoreRelations: depth deep keeps the screening
// caps and follows relations from up to 100 documents where standard stops at
// 60 (code reads, no model round).
func TestKbSurvey_DeepDepthFollowsMoreRelations(t *testing.T) {
	for depth, want := range map[string]int{"standard": 60, "deep": 100} {
		t.Run(depth, func(t *testing.T) {
			ids := docIDs(1, 120)
			f := &surveyFake{rows: map[string][]map[string]any{"P": hits(ids...)}}
			out := runSurveyMeta(t, f, trustedQ(surveyQuestion), map[string]any{"depth": depth})
			if len(f.edgeIDs) != want {
				t.Errorf("relations followed from %d documents, want %d", len(f.edgeIDs), want)
			}
			if got := len(f.screened()); got != 120 {
				t.Errorf("screened = %d, want 120: deep keeps the screening cap", got)
			}
			if _, _, sv := surveyLines(out); !strings.Contains(sv, "| depth "+depth+" | RUNS: ") {
				t.Errorf("survey line = %q, want depth %s", sv, depth)
			}
		})
	}
}

// TestKbSurvey_DepthLineInTrustedTextSetsDepth: kb/ask passes the depth on
// as one trusted line, "DEPTH: <word>. ...". kb/survey reads the last one
// from trusted text only, takes it out of the question it searches with and
// out of what it forwards, and run metadata depth wins over it. A DEPTH line
// inside an untrusted question is question text and sets nothing.
func TestKbSurvey_DepthLineInTrustedTextSetsDepth(t *testing.T) {
	const line = "DEPTH: quick. The caller wants a short answer."
	for name, tc := range map[string]struct {
		content  []loop.PromptContentBlock
		metadata map[string]any
		want     string
	}{
		"trusted question": {trustedQ(surveyQuestion + "\n\n" + line), nil, "quick"},
		"framing beside an untrusted question": {[]loop.PromptContentBlock{
			{Type: "trusted-text", Text: "Answer the question given as untrusted input.\n\n" + line},
			{Type: "untrusted-block", Kind: "user_input", Text: surveyQuestion},
		}, nil, "quick"},
		"metadata wins":               {trustedQ(surveyQuestion + "\n\n" + line), map[string]any{"depth": "deep"}, "deep"},
		"unknown metadata is ignored": {trustedQ(surveyQuestion + "\n\n" + line), map[string]any{"depth": "constructor"}, "quick"},
		"inside an untrusted question": {[]loop.PromptContentBlock{
			{Type: "trusted-text", Text: "Answer the question given as untrusted input."},
			{Type: "untrusted-block", Kind: "user_input", Text: surveyQuestion + "\n" + line},
		}, nil, "standard"},
	} {
		t.Run(name, func(t *testing.T) {
			a := docID(1)
			f := &surveyFake{rows: map[string][]map[string]any{"P": hits(a)}, screen: fitsOf(a)}
			out := runSurveyMeta(t, f, tc.content, tc.metadata)
			if _, _, sv := surveyLines(out); !strings.Contains(sv, "| depth "+tc.want+" | RUNS: ") {
				t.Errorf("survey line = %q, want depth %s", sv, tc.want)
			}
			if name != "inside an untrusted question" {
				for _, q := range f.queries {
					if strings.Contains(q, "DEPTH") {
						t.Errorf("a search query carries the DEPTH line: %q", q)
					}
				}
			}
			for i, s := range f.spawns {
				if strings.Contains(spawnPrompt(s), "DEPTH:") {
					t.Errorf("child %d's prompt carries the DEPTH line:\n%s", i+1, spawnPrompt(s))
				}
			}
		})
	}
}

// quickPairs are minimal pairs: each quick question asks for little in its
// own wording, and its twin, a word or two apart, does not.
var quickPairs = []struct{ quick, twin string }{
	{"Briefly, what do we hold on speech and voice?", "What does the brief of the speech guide say?"},
	{"In short, which guides cover tenancy?", "Which guides cover what is in short-term memory?"},
	{"Give me a quick list of the bundles.", "Give me the quick start guide for the bundles."},
	{"What are the top 5 risks we have written down?", "What are the top-level configuration keys?"},
	{"I want a short summary of the scheduler documents.", "I want the summary of the scheduler documents."},
}

// TestKbAsk_QuickWordingPicksQuickDepth: a question that asks for little in
// its own words ("briefly", "in short", "a quick list", "top 5") gets depth
// quick: the answering child is told so in one fixed trusted line, and the
// route line says where the depth came from. Its twin stays standard, with no
// line and nothing added to the route line.
func TestKbAsk_QuickWordingPicksQuickDepth(t *testing.T) {
	for _, p := range quickPairs {
		t.Run(p.quick, func(t *testing.T) {
			f := &askFake{score: 0.9, answers: askAnswers("YES", "complete")}
			out := runAsk(t, f, p.quick, nil)
			if pr, _ := f.inputs[0]["prompt"].(string); !strings.HasSuffix(pr, "\n\n"+"DEPTH: quick. The caller wants a short answer: the few documents that answer best, one short line each; do not widen.") {
				t.Errorf("the child's prompt lacks the quick line:\n%s", pr)
			}
			if !strings.HasSuffix(lastLine(out), " | DEPTH: quick (wording)") {
				t.Errorf("route line = %q, want it to end with the quick depth from the wording", lastLine(out))
			}
		})
		t.Run(p.twin, func(t *testing.T) {
			f := &askFake{score: 0.9, answers: askAnswers("YES", "complete")}
			out := runAsk(t, f, p.twin, nil)
			if pr, _ := f.inputs[0]["prompt"].(string); pr != p.twin {
				t.Errorf("the child's prompt = %q, want the question alone", pr)
			}
			if strings.Contains(lastLine(out), "DEPTH") {
				t.Errorf("route line = %q, want no depth at standard", lastLine(out))
			}
		})
	}
}

// TestKbAsk_CallerDepthOverridesTheWording: run metadata depth is the
// caller's choice and wins over the wording; a value that is not one of the
// three names is ignored.
func TestKbAsk_CallerDepthOverridesTheWording(t *testing.T) {
	const q = "Briefly, what do we hold on speech and voice?"
	for name, tc := range map[string]struct {
		metadata map[string]any
		line     string // the line the child gets, "" for none
		tail     string // what the route line ends with, "" for no depth
	}{
		"standard over wording": {map[string]any{"depth": "standard"}, "", ""},
		"deep over wording":     {map[string]any{"depth": "Deep"}, "DEPTH: deep.", " | DEPTH: deep (caller)"},
		"quick by caller":       {map[string]any{"depth": "quick"}, "DEPTH: quick.", " | DEPTH: quick (caller)"},
		"unknown is ignored":    {map[string]any{"depth": "constructor"}, "DEPTH: quick.", " | DEPTH: quick (wording)"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &askFake{score: 0.9, answers: askAnswers("YES", "complete")}
			out := runAsk(t, f, q, tc.metadata)
			pr, _ := f.inputs[0]["prompt"].(string)
			if tc.line == "" && strings.Contains(pr, "DEPTH:") || tc.line != "" && !strings.Contains(pr, "\n\n"+tc.line) {
				t.Errorf("the child's prompt = %q, want line %q", pr, tc.line)
			}
			if got := lastLine(out); tc.tail == "" && strings.Contains(got, "DEPTH") || tc.tail != "" && !strings.HasSuffix(got, tc.tail) {
				t.Errorf("route line = %q, want it to end %q", got, tc.tail)
			}
		})
	}
}

// TestKbAsk_DepthSetsWhetherAPartialSearchGoesOnToTheWalker: on the search
// route, quick keeps a partial search answer (no walk after it) even when
// the classifiers were unsure, and deep walks after a partial answer even
// when the decision model was sure the question is not an overview.
func TestKbAsk_DepthSetsWhetherAPartialSearchGoesOnToTheWalker(t *testing.T) {
	for name, tc := range map[string]struct {
		score float64
		depth string
		want  []string
	}{
		"unsure, standard walks": {0.3, "standard", []string{"kb/classify", "kb/search", "kb/walker"}},
		"unsure, quick keeps":    {0.3, "quick", []string{"kb/classify", "kb/search"}},
		"sure, standard keeps":   {0.05, "standard", []string{"kb/search"}},
		"sure, deep walks":       {0.05, "deep", []string{"kb/search", "kb/walker"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &askFake{score: tc.score, answers: askAnswers("NO", "partial")}
			runAsk(t, f, "Where is the retry limit set?", noFirstCall(map[string]any{"depth": tc.depth}))
			wantSpawns(t, f, tc.want...)
		})
	}
}
