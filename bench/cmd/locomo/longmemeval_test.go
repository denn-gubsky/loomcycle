package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A minimal instance set covering what the loader has to get right: the six task
// types, an abstention instance, and the evidence label.
const lmeFixture = `[
 {"question_id":"q1","question_type":"single-session-user","question":"Where did Dave move?","answer":"Berlin",
  "question_date":"2023-08-01","haystack_session_ids":["s0","s1"],
  "haystack_dates":["2023-07-07 19:56","2023-07-20 08:00"],
  "haystack_sessions":[
    [{"role":"user","content":"I moved to Berlin.","has_answer":true},{"role":"assistant","content":"Nice."}],
    [{"role":"user","content":"The weather is fine."}]
  ]},
 {"question_id":"q2_abs","question_type":"knowledge-update","question":"What car do I drive?","answer":"no information",
  "question_date":"2023-08-01","haystack_session_ids":["s0"],
  "haystack_dates":["2023-07-09 10:00"],
  "haystack_sessions":[[{"role":"user","content":"I like cycling."}]]},
 {"question_id":"q3","question_type":"temporal-reasoning","question":"When?","answer":"July",
  "haystack_session_ids":["s0"],"haystack_dates":["2023-07-07 19:56"],
  "haystack_sessions":[[{"role":"user","content":"It happened in July.","has_answer":true}]]},
 {"question_id":"q4","question_type":"not-a-real-type","question":"x","answer":"y",
  "haystack_session_ids":["s0"],"haystack_dates":["2023-01-01"],
  "haystack_sessions":[[{"role":"user","content":"z","has_answer":true}]]},
 {"question_id":"q5","question_type":"multi-session","question":"","answer":"y",
  "haystack_session_ids":["s0"],"haystack_dates":["2023-01-01"],
  "haystack_sessions":[[{"role":"user","content":"z","has_answer":true}]]},
 {"question_id":"q6","question_type":"multi-session","question":"no evidence anywhere","answer":"y",
  "haystack_session_ids":["s0"],"haystack_dates":["2023-01-01"],
  "haystack_sessions":[[{"role":"user","content":"z"}]]}
]`

func writeLMEFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "lme.json")
	if err := os.WriteFile(p, []byte(lmeFixture), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

// TestLoadLongMemEval_ConvertsInstancesAndCountsDefects.
func TestLoadLongMemEval_ConvertsInstancesAndCountsDefects(t *testing.T) {
	convs, d, err := LoadLongMemEval(writeLMEFixture(t), 0)
	if err != nil {
		t.Fatalf("LoadLongMemEval: %v", err)
	}
	// q1, q2_abs, q3 load. q4 unknown type, q5 no question, q6 no evidence.
	if len(convs) != 3 {
		t.Fatalf("loaded %d instances, want 3; defects = %s", len(convs), d)
	}
	if d.UnknownType != 1 || d.NoQuestion != 1 || d.NoEvidence != 1 {
		t.Errorf("defects = %s, want one of each drop counted — a silent drop moves the "+
			"denominator and stops two arms being comparable", d)
	}
	if d.Abstention != 1 {
		t.Errorf("abstention count = %d, want 1", d.Abstention)
	}
}

// TestLoadLongMemEval_AbstentionInstanceSurvivesWithNoEvidence is the one the
// loader is most likely to get wrong: an `_abs` instance has NO evidence turns by
// construction, so the "no evidence" defect rule would drop exactly the slice
// LongMemEval was adopted for.
func TestLoadLongMemEval_AbstentionInstanceSurvivesWithNoEvidence(t *testing.T) {
	convs, _, err := LoadLongMemEval(writeLMEFixture(t), 0)
	if err != nil {
		t.Fatalf("LoadLongMemEval: %v", err)
	}
	var found bool
	for _, c := range convs {
		if c.SampleID != "lme-q2_abs" {
			continue
		}
		found = true
		q := c.Queries[0]
		if !q.Abstain {
			t.Errorf("`_abs` instance did not set Abstain, so refusing it would be scored as a miss")
		}
		if len(q.Expected) != 0 {
			t.Errorf("Expected = %v, want empty for an abstention instance", q.Expected)
		}
		if q.Category != LMECatKnowledgeUpdate {
			t.Errorf("category = %d, want knowledge-update (%d)", q.Category, LMECatKnowledgeUpdate)
		}
	}
	if !found {
		t.Fatal("the abstention instance was DROPPED — the no-evidence rule must not apply to `_abs`")
	}
}

// TestLoadLongMemEval_SessionDateReachesTheTurnText. The date lives on the
// session; the extractor now reads a turn's leading timestamp to stamp
// observed_at, so the date has to be IN the turn rather than in a sidecar field.
func TestLoadLongMemEval_SessionDateReachesTheTurnText(t *testing.T) {
	convs, _, err := LoadLongMemEval(writeLMEFixture(t), 0)
	if err != nil {
		t.Fatalf("LoadLongMemEval: %v", err)
	}
	for _, c := range convs {
		if c.SampleID != "lme-q1" {
			continue
		}
		body := c.Turns[0].Body()
		if want := "[2023-07-07 19:56]"; !strings.Contains(body, want) {
			t.Errorf("turn body = %q, want the session date %s prefixed — without it the temporal "+
				"slice is unretrievable and observed_at cannot be stamped", body, want)
		}
		// Second session's turns carry the SECOND date, not the first.
		last := c.Turns[len(c.Turns)-1].Body()
		if !strings.Contains(last, "[2023-07-20 08:00]") {
			t.Errorf("last turn body = %q, want the second session's date", last)
		}
	}
}

// TestLoadLongMemEval_EvidenceTurnsBecomeTheAnswerKey.
func TestLoadLongMemEval_EvidenceTurnsBecomeTheAnswerKey(t *testing.T) {
	convs, _, err := LoadLongMemEval(writeLMEFixture(t), 0)
	if err != nil {
		t.Fatalf("LoadLongMemEval: %v", err)
	}
	for _, c := range convs {
		if c.SampleID != "lme-q1" {
			continue
		}
		q := c.Queries[0]
		if len(q.Expected) != 1 || q.Expected[0] != "q1:s0:t0" {
			t.Errorf("Expected = %v, want exactly the has_answer turn's key", q.Expected)
		}
	}
}

// TestLoadLongMemEval_LimitIsDeterministic. Every arm of a paired comparison must
// see the same instances; a random sample would make two arms incomparable while
// looking fine.
func TestLoadLongMemEval_LimitIsDeterministic(t *testing.T) {
	p := writeLMEFixture(t)
	a, _, err := LoadLongMemEval(p, 2)
	if err != nil {
		t.Fatalf("load a: %v", err)
	}
	b, _, err := LoadLongMemEval(p, 2)
	if err != nil {
		t.Fatalf("load b: %v", err)
	}
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("limit=2 gave %d and %d instances", len(a), len(b))
	}
	for i := range a {
		if a[i].SampleID != b[i].SampleID {
			t.Errorf("instance %d differs between loads: %s vs %s — the sample must be deterministic",
				i, a[i].SampleID, b[i].SampleID)
		}
	}
}

// TestLoadLongMemEval_EachInstanceGetsItsOwnScope. Instances SHARE haystack
// sessions, so a shared keyspace would let one instance's question retrieve
// another's evidence and score a hit the system never earned.
func TestLoadLongMemEval_EachInstanceGetsItsOwnScope(t *testing.T) {
	convs, _, err := LoadLongMemEval(writeLMEFixture(t), 0)
	if err != nil {
		t.Fatalf("LoadLongMemEval: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range convs {
		id := c.ScopeID()
		if seen[id] {
			t.Errorf("scope_id %q reused across instances", id)
		}
		seen[id] = true
	}
	if len(seen) != len(convs) {
		t.Errorf("%d distinct scope_ids for %d instances", len(seen), len(convs))
	}
}

// TestLMECategoryName_IsDisjointFromLoCoMo. A shared numbering would silently
// merge two datasets' slices the first time someone compared them.
func TestLMECategoryName_IsDisjointFromLoCoMo(t *testing.T) {
	for c := 1; c <= 5; c++ {
		if n := LMECategoryName(c); n != "" {
			t.Errorf("LMECategoryName(%d) = %q, want empty — LoCoMo's range must not resolve here", c, n)
		}
	}
	if got := CategoryName(LMECatKnowledgeUpdate); got != "knowledge-update" {
		t.Errorf("CategoryName(%d) = %q, want it to chain to the LongMemEval name",
			LMECatKnowledgeUpdate, got)
	}
	if got := CategoryName(CategoryTemporal); got != "temporal" {
		t.Errorf("CategoryName chaining broke LoCoMo's own names: %q", got)
	}
}

// answerStub serves an MCP spawn_run that returns a fixed answer text, so the
// verdict under test is decided by answerOne's own logic rather than by a model.
// A judge call would be a second spawn — the assertions below check none happens.
func answerStub(t *testing.T, answer string) (*httptest.Server, *int) {
	t.Helper()
	spawns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "initialize" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{}}}`))
			return
		}
		spawns++
		payload := map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"content": []any{map[string]any{
				"type": "text",
				"text": `{"run_id":"r1","status":"completed","final_text":` + strconvQuote(answer) +
					`,"usage":{"provider":"p","model":"m"}}`,
			}}},
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	return srv, &spawns
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestAnswerOne_AbstentionQuestionRewardsARefusal is the scoring inversion, and
// the one place LoCoMo and LongMemEval disagree about what a right answer is.
//
// For a LongMemEval `_abs` instance the history does NOT contain the answer, so
// refusing is correct. The harness otherwise scores NOT_FOUND as a miss, because
// every included LoCoMo category is answerable. Getting this backwards would mark
// correct refusals as failures and reward a system that confabulates — the
// inverse of what the abstention slice measures.
func TestAnswerOne_AbstentionQuestionRewardsARefusal(t *testing.T) {
	srv, spawns := answerStub(t, "NOT_FOUND")
	mc := NewMCPClient(srv.URL, "tok", 30*time.Second)

	q := Query{Question: "What car do I drive?", Category: LMECatKnowledgeUpdate, Abstain: true}
	res := answerOne(context.Background(), mc, "u1", q, "a", "j", false)

	if res.Verdict != VerdictCorrect {
		t.Errorf("verdict = %q (%s), want correct — abstaining IS the gold behaviour here",
			res.Verdict, res.Why)
	}
	if *spawns != 1 {
		t.Errorf("%d spawns, want 1 (the answerer only) — the verdict is decided by the refusal, "+
			"so no judge call is needed", *spawns)
	}
}

// TestAnswerOne_AbstentionQuestionPunishesAConfabulation — the inverse half. An
// `_abs` gold field is not a real answer, so handing the pair to a judge would ask
// it to compare a fabrication against a non-answer.
func TestAnswerOne_AbstentionQuestionPunishesAConfabulation(t *testing.T) {
	srv, spawns := answerStub(t, "You drive a blue Volvo.")
	mc := NewMCPClient(srv.URL, "tok", 30*time.Second)

	q := Query{Question: "What car do I drive?", Category: LMECatKnowledgeUpdate, Abstain: true, Answer: "no information"}
	res := answerOne(context.Background(), mc, "u1", q, "a", "j", false)

	if res.Verdict != VerdictWrong {
		t.Errorf("verdict = %q (%s), want wrong — the history has no answer, so producing one is "+
			"a confabulation", res.Verdict, res.Why)
	}
	if *spawns != 1 {
		t.Errorf("%d spawns, want 1 — an `_abs` gold field is not a real answer, so no judge "+
			"call should be made", *spawns)
	}
}

// TestAnswerOne_NonAbstentionRefusalIsStillAMiss — the LoCoMo behaviour must not
// change. Every included LoCoMo category is answerable, so a refusal there is a
// miss and stays one.
func TestAnswerOne_NonAbstentionRefusalIsStillAMiss(t *testing.T) {
	srv, _ := answerStub(t, "NOT_FOUND")
	mc := NewMCPClient(srv.URL, "tok", 30*time.Second)

	q := Query{Question: "Where did Dave move?", Category: CategorySingleHop, Answer: "Berlin"}
	res := answerOne(context.Background(), mc, "u1", q, "a", "j", false)

	if res.Verdict != VerdictWrong {
		t.Errorf("verdict = %q, want wrong — a LoCoMo refusal is still a miss", res.Verdict)
	}
	if !res.NotFound {
		t.Errorf("NotFound not recorded, so the abstention rate would under-report")
	}
}
