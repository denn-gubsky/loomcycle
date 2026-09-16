package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal corpus in the shape gen_corpus.py emits: two sessions, one question
// whose two supports live in different sessions.
const synthFixture = `{
 "seed": 7,
 "entity_counts": {"people": 1, "orgs": 1, "cities": 1},
 "facts": [
  {"id": "f1", "session": "s1", "turn_dia_id": "s1:t2", "text": "Ines Farrow works at Velmara Instruments.", "s": "Ines Farrow", "r": "works_at", "o": "Velmara Instruments"},
  {"id": "f2", "session": "s2", "turn_dia_id": "s2:t2", "text": "Velmara Instruments is based in Aldbury.", "s": "Velmara Instruments", "r": "based_in", "o": "Aldbury"}
 ],
 "sessions": [
  {"id": "s1", "date": "2026-01-05", "fact_ids": ["f1"], "turns": [
    {"role": "user", "dia_id": "s1:t0", "text": "Notes."},
    {"role": "assistant", "dia_id": "s1:t1", "text": "Logged."},
    {"role": "user", "dia_id": "s1:t2", "text": "Ines Farrow works at Velmara Instruments."},
    {"role": "assistant", "dia_id": "s1:t3", "text": "Noted."}]},
  {"id": "s2", "date": "2026-01-06", "fact_ids": ["f2"], "turns": [
    {"role": "user", "dia_id": "s2:t0", "text": "Notes."},
    {"role": "assistant", "dia_id": "s2:t1", "text": "Logged."},
    {"role": "user", "dia_id": "s2:t2", "text": "Velmara Instruments is based in Aldbury."},
    {"role": "assistant", "dia_id": "s2:t3", "text": "Noted."}]}
 ],
 "questions": [
  {"id": "q1", "hops": 2, "q": "Which city does Ines Farrow work in?", "gold": "Aldbury",
   "anchor": "Ines Farrow", "relations": ["works_at", "based_in"],
   "supports": ["f1", "f2"], "guess_floor": 0.02}
 ]
}`

func writeSynthFixture(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "corpus.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSynthesis_WholeCorpusIsOneConversation(t *testing.T) {
	// One scope, many sessions. Splitting per session would make every question
	// unanswerable for a reason unrelated to traversal.
	convs, d, err := LoadSynthesis(writeSynthFixture(t, synthFixture), 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if d.Any() {
		t.Fatalf("unexpected defects: %s", d)
	}
	if len(convs) != 1 {
		t.Fatalf("want 1 conversation, got %d", len(convs))
	}
	if got, want := len(convs[0].Turns), 8; got != want {
		t.Errorf("turns = %d, want %d", got, want)
	}
	if got, want := len(convs[0].Queries), 1; got != want {
		t.Fatalf("queries = %d, want %d", got, want)
	}
	if got := convs[0].ScopeID(); !strings.Contains(got, "db2-7") {
		t.Errorf("scope %q does not carry the corpus seed", got)
	}
}

func TestLoadSynthesis_EvidenceNamesTheSupportingTurns(t *testing.T) {
	// The answer key must point at the exact turns, or recall@k is scored
	// against a key the corpus does not actually have.
	convs, _, err := LoadSynthesis(writeSynthFixture(t, synthFixture), 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	q := convs[0].Queries[0]
	want := []string{"s1:t2", "s2:t2"}
	if len(q.Expected) != len(want) {
		t.Fatalf("expected = %v, want %v", q.Expected, want)
	}
	for i := range want {
		if q.Expected[i] != want[i] {
			t.Errorf("expected[%d] = %q, want %q", i, q.Expected[i], want[i])
		}
	}
}

func TestLoadSynthesis_CategoryIsTheHopCount(t *testing.T) {
	// A per-category report has to read as a per-depth one, and the numbering
	// must not collide with LoCoMo's 1-4 or LongMemEval's 101-106.
	convs, _, err := LoadSynthesis(writeSynthFixture(t, synthFixture), 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got, want := convs[0].Queries[0].Category, 202; got != want {
		t.Errorf("category = %d, want %d", got, want)
	}
	for _, c := range []int{1, 2, 3, 4, 101, 102, 103, 104, 105, 106} {
		if SynthCategory(2) == c || SynthCategory(3) == c || SynthCategory(4) == c {
			t.Errorf("synthesis category collides with existing category %d", c)
		}
	}
}

func TestLoadSynthesis_CarriesTheSessionDate(t *testing.T) {
	// The extractor stamps observed_at from the date and Turn.Body() prefixes it
	// into the embedded text, so a dropped date changes how the corpus ingests.
	convs, _, err := LoadSynthesis(writeSynthFixture(t, synthFixture), 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := convs[0].Turns[0].DateTime; got != "2026-01-05" {
		t.Errorf("DateTime = %q, want the session date", got)
	}
	if body := convs[0].Turns[2].Body(); !strings.Contains(body, "2026-01-05") ||
		!strings.Contains(body, synthUserSpeaker) {
		t.Errorf("Body() = %q, want the date and speaker prefixed", body)
	}
}

func TestLoadSynthesis_QuestionWithAnUnresolvableSupportIsDroppedAndCounted(t *testing.T) {
	// Silently keeping it would score the question against a shorter answer key
	// than it claims to have.
	var c map[string]any
	if err := json.Unmarshal([]byte(synthFixture), &c); err != nil {
		t.Fatal(err)
	}
	qs := c["questions"].([]any)
	qs[0].(map[string]any)["supports"] = []any{"f1", "f404"}
	blob, _ := json.Marshal(c)

	_, d, err := LoadSynthesis(writeSynthFixture(t, string(blob)), 0)
	if err == nil {
		t.Fatal("want an error when no question survives, got nil")
	}
	if d.UnresolvableSupports != 1 || d.QuestionsWithoutSupports != 1 {
		t.Errorf("defects = %s, want one unresolvable support and one dropped question", d)
	}
}

func TestLoadSynthesis_FactWithoutATurnIsCounted(t *testing.T) {
	var c map[string]any
	if err := json.Unmarshal([]byte(synthFixture), &c); err != nil {
		t.Fatal(err)
	}
	c["facts"].([]any)[1].(map[string]any)["turn_dia_id"] = ""
	blob, _ := json.Marshal(c)

	_, d, err := LoadSynthesis(writeSynthFixture(t, string(blob)), 0)
	if err == nil {
		t.Fatal("want an error when no question survives, got nil")
	}
	if d.FactsWithoutTurn != 1 {
		t.Errorf("FactsWithoutTurn = %d, want 1 (%s)", d.FactsWithoutTurn, d)
	}
}

func TestLoadSynthesis_RejectsAnEmptyOrWrongShapedCorpus(t *testing.T) {
	if _, _, err := LoadSynthesis(writeSynthFixture(t, `{"sessions": []}`), 0); err == nil {
		t.Error("want an error for a corpus with no sessions")
	}
	if _, _, err := LoadSynthesis(writeSynthFixture(t, `not json`), 0); err == nil {
		t.Error("want an error for a non-JSON file")
	}
}

func TestLoadSynthesis_LimitBoundsQuestionsNotSessions(t *testing.T) {
	// The corpus is one conversation by construction, so -conversations has
	// nothing else to bound; it must not silently drop history instead.
	var c map[string]any
	if err := json.Unmarshal([]byte(synthFixture), &c); err != nil {
		t.Fatal(err)
	}
	q := c["questions"].([]any)[0].(map[string]any)
	second := map[string]any{}
	for k, v := range q {
		second[k] = v
	}
	second["id"] = "q2"
	c["questions"] = []any{q, second}
	blob, _ := json.Marshal(c)

	convs, _, err := LoadSynthesis(writeSynthFixture(t, string(blob)), 1)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := len(convs[0].Queries); got != 1 {
		t.Errorf("queries = %d, want 1 under limit", got)
	}
	if got := len(convs[0].Turns); got != 8 {
		t.Errorf("turns = %d, want all 8 — the limit must not drop history", got)
	}
}

// TestSynthesisCorpus_NoQuestionIsReachableInOneHop guards the property the
// whole phase exists for, on the COMMITTED corpus rather than a fixture.
//
// A question a single retrieved fact can answer measures retrieval volume, not
// traversal. The generator enforces this; if someone regenerates with a template
// that breaks it, the benchmark would still run and would report a traversal
// win that is really a retrieval win. So it is checked here too.
func TestSynthesisCorpus_NoQuestionIsReachableInOneHop(t *testing.T) {
	path := filepath.Join("..", "..", "synthesis", "corpus-db2.json")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("committed corpus not present: %v", err)
	}
	var raw synthCorpus
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("corpus: %v", err)
	}
	if len(raw.Questions) == 0 {
		t.Fatal("corpus holds no questions")
	}
	for _, q := range raw.Questions {
		for _, f := range raw.Facts {
			if strings.Contains(f.Text, q.Anchor) && strings.Contains(f.Text, q.Gold) {
				t.Errorf("%s: one fact holds both the anchor %q and the answer %q: %q",
					q.ID, q.Anchor, q.Gold, f.Text)
			}
		}
	}
}

// TestSynthesisCorpus_NoQuestionIsAnswerableWithinOneSession guards the other
// half: cross-session is the premise of the whole RFC.
func TestSynthesisCorpus_NoQuestionIsAnswerableWithinOneSession(t *testing.T) {
	path := filepath.Join("..", "..", "synthesis", "corpus-db2.json")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("committed corpus not present: %v", err)
	}
	var raw synthCorpus
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatalf("corpus: %v", err)
	}
	sessionOf := make(map[string]string, len(raw.Facts))
	for _, f := range raw.Facts {
		sessionOf[f.ID] = f.Session
	}
	for _, q := range raw.Questions {
		seen := make(map[string]bool, len(q.Supports))
		for _, sup := range q.Supports {
			s, ok := sessionOf[sup]
			if !ok {
				t.Errorf("%s: support %s is not in the corpus", q.ID, sup)
				continue
			}
			if seen[s] {
				t.Errorf("%s: two supports share session %s — answerable in one session", q.ID, s)
			}
			seen[s] = true
		}
		if len(q.Supports) != q.Hops {
			t.Errorf("%s: %d supports but declared %d hops", q.ID, len(q.Supports), q.Hops)
		}
	}
}
