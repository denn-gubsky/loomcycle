package memory

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type stubRerankModel struct {
	reply  string
	err    error
	prompt string
}

func (s *stubRerankModel) Complete(_ context.Context, prompt string) (string, error) {
	s.prompt = prompt
	return s.reply, s.err
}

var fiveTexts = []string{"a", "b", "c", "d", "e"}

// TestRerankTexts_AWellFormedRankingReorders — the success path, with the
// candidates the model left out kept, in their original relative order, after
// the ones it ranked.
func TestRerankTexts_AWellFormedRankingReorders(t *testing.T) {
	m := &stubRerankModel{reply: "[3, 1]"}
	order, rep := RerankTexts(context.Background(), m, "q", fiveTexts, 1200)
	if want := []int{2, 0, 1, 3, 4}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	if !rep.Applied || rep.Reason != "" || rep.Candidates != 5 {
		t.Errorf("report = %+v, want applied over 5 candidates", rep)
	}
}

// TestRerankTexts_EveryFailureKeepsSearchOrder — the RFC's three named faults:
// a malformed ranking, a timeout, and an erroring call each return the ORIGINAL
// order with the reason, never an error.
func TestRerankTexts_EveryFailureKeepsSearchOrder(t *testing.T) {
	identity := []int{0, 1, 2, 3, 4}
	for _, c := range []struct {
		name   string
		model  *stubRerankModel
		reason string
	}{
		{"malformed", &stubRerankModel{reply: "The third passage is best."}, RerankUnparseable},
		{"no known numbers", &stubRerankModel{reply: "[9, 0, -2]"}, RerankUnparseable},
		{"timeout", &stubRerankModel{err: fmt.Errorf("provider: %w", context.DeadlineExceeded)}, RerankTimeout},
		{"erroring", &stubRerankModel{err: errors.New("connection refused")}, RerankCallFailed},
	} {
		order, rep := RerankTexts(context.Background(), c.model, "q", fiveTexts, 1200)
		if !reflect.DeepEqual(order, identity) {
			t.Errorf("%s: order = %v, want search's own %v", c.name, order, identity)
		}
		if rep.Applied || rep.Reason != c.reason {
			t.Errorf("%s: report = %+v, want not applied with reason %q", c.name, rep, c.reason)
		}
	}
}

// TestRerankTexts_NothingToCallWithoutAModelOrAPool — no reranker and a
// one-candidate pool make no call at all.
func TestRerankTexts_NothingToCallWithoutAModelOrAPool(t *testing.T) {
	if _, rep := RerankTexts(context.Background(), nil, "q", fiveTexts, 1200); rep.Reason != RerankNotConfigured {
		t.Errorf("nil model: %+v", rep)
	}
	m := &stubRerankModel{reply: "[1]"}
	if _, rep := RerankTexts(context.Background(), m, "q", []string{"only"}, 1200); rep.Reason != RerankTooFewCandidates {
		t.Errorf("one candidate: %+v", rep)
	}
	if m.prompt != "" {
		t.Error("a one-candidate pool must not reach the model")
	}
}

// TestParseRanking_RepairsRatherThanTrusts — every repair the RFC lists, plus the
// two reply shapes a local model actually produces around the array.
func TestParseRanking_RepairsRatherThanTrusts(t *testing.T) {
	for _, c := range []struct {
		reply string
		want  []int
		ok    bool
	}{
		{"[2, 2, 7, 1]", []int{1, 0, 2, 3, 4}, true},                                      // repeat + unknown dropped
		{`Here you go: [4, "5"] — done`, []int{3, 4, 0, 1, 2}, true},                      // prose around it; a quoted number
		{"<think>maybe [1] or [5]</think>\n[5, 4]", []int{4, 3, 0, 1, 2}, true},           // reasoning is not the answer
		{"[1.5, 2]", []int{1, 0, 2, 3, 4}, true},                                          // a non-integer is not a number
		{"Passages [1] and [4] cover it. Ranking: [4, 2, 1]", []int{3, 1, 0, 2, 4}, true}, // a citation is not the answer
		{"[2] fits, but so does [3]", []int{2, 0, 1, 3, 4}, true},                         // a tie goes to the later span
		{"<think>the best is [2", []int{0, 1, 2, 3, 4}, false},                            // reasoning cut off: no answer
		{"[]", []int{0, 1, 2, 3, 4}, false},
		{"[1, 2", []int{0, 1, 2, 3, 4}, false},
		{"", []int{0, 1, 2, 3, 4}, false},
	} {
		got, ok := ParseRanking(c.reply, 5)
		if !reflect.DeepEqual(got, c.want) || ok != c.ok {
			t.Errorf("ParseRanking(%q) = %v,%v, want %v,%v", c.reply, got, ok, c.want, c.ok)
		}
	}
}

// TestBuildRerankPrompt_IsTheMeasuredPrompt — the wording is part of the measured
// result, so this pins it: numbering from 1, the question line, the instruction,
// and per-candidate truncation by characters (never splitting a rune).
func TestBuildRerankPrompt_IsTheMeasuredPrompt(t *testing.T) {
	p := BuildRerankPrompt("who wrote it", []string{"Doc — Intro\nfirst", "ÄÄÄÄ"}, 3)
	want := "Question: who wrote it\n\nCandidate passages:\n\n[1] Doc\n\n[2] ÄÄÄ\n\n" +
		"Rank the passages by how useful they are for answering the question, most useful first. " +
		"Answer only with a JSON array of passage numbers, e.g. [3, 1, 7], listing at least the 2 most useful."
	if p != want {
		t.Errorf("prompt =\n%q\nwant\n%q", p, want)
	}
	long := BuildRerankPrompt("q", make([]string, 20), 10)
	if !strings.HasSuffix(long, "listing at least the 8 most useful.") {
		t.Errorf("a full pool asks for at least 8: %q", long[len(long)-60:])
	}
}

func TestRerankOptions_DefaultsAreTheMeasuredValues(t *testing.T) {
	var o RerankOptions
	if o.EffectiveCandidates() != 20 || o.EffectiveMaxChars() != 1200 {
		t.Errorf("defaults = %d/%d, want 20/1200", o.EffectiveCandidates(), o.EffectiveMaxChars())
	}
	o = RerankOptions{Candidates: 30, MaxChars: 800}
	if o.EffectiveCandidates() != 30 || o.EffectiveMaxChars() != 800 {
		t.Errorf("explicit = %d/%d", o.EffectiveCandidates(), o.EffectiveMaxChars())
	}
}

// TestSearchQuery_CanReturnDocumentsHonoursThePrefix — a prefix outside the chunk
// namespace can match no chunk body, so it is not a document search whatever
// the source selector says, and must not pay for a rerank.
func TestSearchQuery_CanReturnDocumentsHonoursThePrefix(t *testing.T) {
	for _, c := range []struct {
		q    SearchQuery
		want bool
	}{
		{SearchQuery{}, true},
		{SearchQuery{Prefix: "doc."}, true},
		{SearchQuery{Prefix: DocumentChunkKeyPrefix}, true},
		{SearchQuery{Prefix: DocumentChunkKeyPrefix + "abc"}, true},
		{SearchQuery{Prefix: "notes/"}, false},
		{SearchQuery{Sources: []Source{SourceFacts, SourceNotes}}, false},
		{SearchQuery{Prefix: "doc.", Sources: []Source{SourceDocuments}}, true},
	} {
		if got := c.q.CanReturnDocuments(); got != c.want {
			t.Errorf("%+v: CanReturnDocuments = %v, want %v", c.q, got, c.want)
		}
	}
}
