package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/memory/eval"
)

// fixture is a HAND-WRITTEN stand-in for locomo10.json, deliberately not an
// excerpt of it: the real dataset is CC BY-NC 4.0 and nothing derived from it
// belongs in this repo. It reproduces the SHAPES that matter — including the
// malformations the released file actually contains.
const fixture = `[
 {"sample_id":"conv-1",
  "conversation":{
    "speaker_a":"Ada","speaker_b":"Bo",
    "session_1_date_time":"1:00 pm on 3 May, 2023",
    "session_1":[
      {"speaker":"Ada","dia_id":"D1:1","text":"I adopted a greyhound."},
      {"speaker":"Bo","dia_id":"D1:2","text":"Here she is","blip_caption":"a grey dog on a sofa"}],
    "session_2_date_time":"9:00 am on 10 May, 2023",
    "session_2":[
      {"speaker":"Ada","dia_id":"D2:1","text":"We started agility class."}],
    "session_9_date_time":"9:00 am on 1 June, 2023"
  },
  "qa":[
    {"question":"What pet did Ada adopt?","answer":"a greyhound","category":4,"evidence":["D1:1"]},
    {"question":"Two ids in one entry","answer":"x","category":1,"evidence":["D1:1; D2:1"]},
    {"question":"Space separated ids","answer":"x","category":1,"evidence":["D1:1 D1:2"]},
    {"question":"Transposed colon","answer":"x","category":2,"evidence":["D:2:1"]},
    {"question":"Bare marker","answer":"x","category":2,"evidence":["D"]},
    {"question":"Names a turn that is not here","answer":"x","category":4,"evidence":["D7:9"]},
    {"question":"No evidence at all","answer":"x","category":3,"evidence":[]},
    {"question":"Numeric answer","answer":3,"category":4,"evidence":["D2:1"]},
    {"question":"Adversarial with no answer","answer":null,"category":5,"evidence":["D1:1"]}
  ]},
 {"sample_id":"conv-2",
  "conversation":{
    "session_1_date_time":"2:00 pm on 4 May, 2023",
    "session_1":[
      {"speaker":"Cy","dia_id":"D1:1","text":"A different conversation entirely."}]
  },
  "qa":[
    {"question":"Whose conversation is this?","answer":"Cy","category":4,"evidence":["D1:1"]}
  ]}
]`

func parseFixture(t *testing.T, cats ...int) ([]Conversation, *Defects) {
	t.Helper()
	if len(cats) == 0 {
		cats = DefaultCategories
	}
	convs, defects, err := Parse([]byte(fixture), cats)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return convs, defects
}

func TestParseEvidence_RecoversTheMalformationsTheReleasedFileContains(t *testing.T) {
	cases := []struct {
		in       string
		wantIDs  []string
		wantBadN int
	}{
		{"D23:1", []string{"D23:1"}, 0},
		// Two ids packed into one entry, semicolon-separated.
		{"D8:6; D9:17", []string{"D8:6", "D9:17"}, 0},
		// Space-separated run of ids.
		{"D9:1 D4:4 D4:6", []string{"D9:1", "D4:4", "D4:6"}, 0},
		// Transposed colon: unambiguously D11:26.
		{"D:11:26", []string{"D11:26"}, 0},
		// A bare marker carries no id and must be REPORTED, not guessed at.
		{"D", nil, 1},
		{"", nil, 0},
	}
	for _, tc := range cases {
		ids, bad := parseEvidence(tc.in)
		if strings.Join(ids, ",") != strings.Join(tc.wantIDs, ",") {
			t.Errorf("parseEvidence(%q) ids = %v, want %v", tc.in, ids, tc.wantIDs)
		}
		if len(bad) != tc.wantBadN {
			t.Errorf("parseEvidence(%q) malformed = %v, want %d", tc.in, bad, tc.wantBadN)
		}
	}
}

// TestParse_ScopeIDIsPerConversationSoCollidingDiaIDsStaySeparate is the
// load-bearing isolation test: both fixture conversations contain a turn
// "D1:1", exactly as the released file's 10 conversations do (5,882 turns,
// 1,033 distinct dia_ids). A shared keyspace would overwrite one with the
// other and let a probe retrieve the wrong conversation's turn.
func TestParse_ScopeIDIsPerConversationSoCollidingDiaIDsStaySeparate(t *testing.T) {
	convs, _ := parseFixture(t)
	if len(convs) != 2 {
		t.Fatalf("got %d conversations, want 2", len(convs))
	}
	if convs[0].ScopeID() == convs[1].ScopeID() {
		t.Fatalf("both conversations share scope_id %q", convs[0].ScopeID())
	}
	var a, b bool
	for _, turn := range convs[0].Turns {
		a = a || turn.DiaID == "D1:1"
	}
	for _, turn := range convs[1].Turns {
		b = b || turn.DiaID == "D1:1"
	}
	if !a || !b {
		t.Fatal("precondition: both conversations must contain a colliding D1:1 turn")
	}
}

func TestParse_EvidenceNamingNoTurnInItsConversationIsDroppedAndCounted(t *testing.T) {
	convs, defects := parseFixture(t)
	for _, q := range convs[0].Queries {
		for _, e := range q.Expected {
			if e == "D7:9" {
				t.Fatalf("query %q kept evidence D7:9, which names no turn in conv-1", q.Question)
			}
		}
		if q.Question == "Names a turn that is not here" {
			t.Fatalf("a query whose only evidence is unresolvable must be dropped, not scored")
		}
	}
	if defects.UnresolvedEvidenceIDs != 1 {
		t.Errorf("UnresolvedEvidenceIDs = %d, want 1", defects.UnresolvedEvidenceIDs)
	}
}

func TestParse_QueryWithoutUsableEvidenceIsDroppedAndCounted(t *testing.T) {
	convs, defects := parseFixture(t)
	for _, q := range convs[0].Queries {
		if len(q.Expected) == 0 {
			t.Fatalf("query %q has an empty answer key and would score recall against nothing", q.Question)
		}
	}
	// Three: the empty-evidence one, the bare-"D" one, and the unresolvable one.
	if defects.QueriesWithoutEvidence != 3 {
		t.Errorf("QueriesWithoutEvidence = %d, want 3", defects.QueriesWithoutEvidence)
	}
	if defects.MalformedEvidenceFragments != 1 {
		t.Errorf("MalformedEvidenceFragments = %d, want 1 (the bare \"D\")", defects.MalformedEvidenceFragments)
	}
}

func TestParse_CategoryFilterExcludesAdversarialAndCountsIt(t *testing.T) {
	convs, defects := parseFixture(t)
	for _, c := range convs {
		for _, q := range c.Queries {
			if q.Category == CategoryAdversarial {
				t.Fatalf("adversarial query %q survived the default filter", q.Question)
			}
		}
	}
	if defects.QueriesFilteredByCategory != 1 {
		t.Errorf("QueriesFilteredByCategory = %d, want 1", defects.QueriesFilteredByCategory)
	}
	// Asking for it explicitly must include it — the filter is a choice, not a ban.
	convs, _ = parseFixture(t, CategoryAdversarial)
	found := false
	for _, c := range convs {
		for _, q := range c.Queries {
			found = found || q.Category == CategoryAdversarial
		}
	}
	if !found {
		t.Error("-categories=5 did not include the adversarial query")
	}
}

func TestParse_SessionDeclaredWithoutTurnsIsCounted(t *testing.T) {
	_, defects := parseFixture(t)
	if defects.SessionsDeclaredWithoutTurns != 1 {
		t.Errorf("SessionsDeclaredWithoutTurns = %d, want 1 (session_9 has a timestamp and no turns)",
			defects.SessionsDeclaredWithoutTurns)
	}
}

func TestParse_NumericGoldAnswerSurvivesAsText(t *testing.T) {
	convs, _ := parseFixture(t)
	for _, q := range convs[0].Queries {
		if q.Question == "Numeric answer" {
			if q.Answer != "3" {
				t.Errorf("Answer = %q, want \"3\" (the released file has 6 bare-number answers)", q.Answer)
			}
			return
		}
	}
	t.Fatal("the numeric-answer query was dropped")
}

// TestTurnBody_CarriesSessionTimestampAndImageCaption — the temporal category
// asks "when", and in LoCoMo the date is on the SESSION. A row without it
// cannot be retrieved for those questions however good the embedder is.
func TestTurnBody_CarriesSessionTimestampAndImageCaption(t *testing.T) {
	convs, _ := parseFixture(t)
	var withCaption string
	for _, turn := range convs[0].Turns {
		if turn.DiaID == "D1:2" {
			withCaption = turn.Body()
		}
		if turn.DiaID == "D1:1" {
			if !strings.Contains(turn.Body(), "3 May, 2023") {
				t.Errorf("turn body %q carries no session timestamp", turn.Body())
			}
			if !strings.Contains(turn.Body(), "Ada:") {
				t.Errorf("turn body %q carries no speaker; speaker-attribution questions need it", turn.Body())
			}
		}
	}
	if !strings.Contains(withCaption, "a grey dog on a sofa") {
		t.Errorf("image turn body %q dropped the BLIP caption", withCaption)
	}
}

// TestConvert_EmitsADatasetTheRealEvalLoaderAccepts guards the one format this
// harness does not own: it writes memory-eval's JSONL, so the assertion is that
// memory-eval's OWN parser reads it back, not that it matches a copy of the
// schema kept here.
func TestConvert_EmitsADatasetTheRealEvalLoaderAccepts(t *testing.T) {
	convs, _ := parseFixture(t)
	dir := t.TempDir()
	var out bytes.Buffer
	if err := doConvert(convs, options{out: dir, topK: 7}, &out); err != nil {
		t.Fatalf("doConvert: %v", err)
	}
	path := filepath.Join(dir, convs[0].ScopeID()+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open emitted dataset: %v", err)
	}
	defer func() { _ = f.Close() }()
	ds, err := eval.LoadJSONL(f)
	if err != nil {
		t.Fatalf("memory-eval could not load the emitted dataset: %v", err)
	}
	if ds.TopK != 7 {
		t.Errorf("TopK = %d, want 7", ds.TopK)
	}
	if len(ds.Corpus) != len(convs[0].Turns) {
		t.Errorf("corpus rows = %d, want %d", len(ds.Corpus), len(convs[0].Turns))
	}
	if len(ds.Queries) != len(convs[0].Queries) {
		t.Errorf("queries = %d, want %d", len(ds.Queries), len(convs[0].Queries))
	}
	// Every expected key must name a row in the corpus, or the dataset scores
	// recall against keys that cannot be retrieved.
	keys := map[string]bool{}
	for _, it := range ds.Corpus {
		keys[it.Key] = true
	}
	for _, q := range ds.Queries {
		for _, e := range q.Expected {
			if !keys[e] {
				t.Errorf("query %q expects key %q which is not in the corpus", q.Query, e)
			}
		}
	}
}
