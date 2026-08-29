package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseVerdict_ReadsAJudgeThatWrapsItsJSONInProse(t *testing.T) {
	cases := []struct {
		in   string
		want Verdict
	}{
		{`{"verdict":"correct","why":"same date"}`, VerdictCorrect},
		{`{"verdict":"partial","why":"missing the month"}`, VerdictPartial},
		{`{"verdict":"wrong","why":"different person"}`, VerdictWrong},
		{"Sure! Here is my grade:\n```json\n{\"verdict\":\"correct\",\"why\":\"ok\"}\n```\nHope that helps.", VerdictCorrect},
		{`{"VERDICT":"Correct"}`, VerdictCorrect},
		// A judge that answers in prose only is a judge malfunction, not a wrong
		// answer — it must not be scored as one.
		{"The answer looks right to me.", VerdictUnparsed},
		{`{"grade":"A"}`, VerdictUnparsed},
		{"", VerdictUnparsed},
	}
	for _, tc := range cases {
		got, _ := ParseVerdict(tc.in)
		if got != tc.want {
			t.Errorf("ParseVerdict(%.40q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestVerdict_ScoreFollowsTheLoCoMoConvention(t *testing.T) {
	for v, want := range map[Verdict]float64{
		VerdictCorrect: 1, VerdictPartial: 0.5, VerdictWrong: 0, VerdictUnparsed: 0,
	} {
		if got := v.Score(); got != want {
			t.Errorf("%q.Score() = %v, want %v", v, got, want)
		}
	}
}

// TestAggregateAnswers_UnparsedIsExcludedFromAccuracyNotScoredZero — folding a
// judge malfunction into the denominator as a miss would understate the memory
// system, which is the opposite of what a benchmark should do when its own
// instrument fails.
func TestAggregateAnswers_UnparsedIsExcludedFromAccuracyNotScoredZero(t *testing.T) {
	rs := []AnswerResult{
		{Verdict: VerdictCorrect},
		{Verdict: VerdictUnparsed},
	}
	st := AggregateAnswers("t", rs)
	if st.Queries != 2 {
		t.Errorf("Queries = %d, want 2 (everything attempted)", st.Queries)
	}
	if st.Graded != 1 {
		t.Errorf("Graded = %d, want 1 (the unparsed one is not graded)", st.Graded)
	}
	if st.Accuracy != 1.0 {
		t.Errorf("Accuracy = %v, want 1.0 — the one graded answer was correct; "+
			"scoring the unparsed verdict as wrong would have given 0.5", st.Accuracy)
	}
	if st.Unparsed != 1 {
		t.Errorf("Unparsed = %d, want 1 reported separately", st.Unparsed)
	}
}

func TestAggregateAnswers_PartialCountsHalfAndNotFoundRateIsReported(t *testing.T) {
	st := AggregateAnswers("t", []AnswerResult{
		{Verdict: VerdictCorrect},
		{Verdict: VerdictPartial},
		{Verdict: VerdictWrong, NotFound: true},
		{Verdict: VerdictWrong},
	})
	if st.Accuracy != (1+0.5+0+0)/4 {
		t.Errorf("Accuracy = %v, want %v", st.Accuracy, 1.5/4)
	}
	if st.NotFoundRate != 0.25 {
		t.Errorf("NotFoundRate = %v, want 0.25", st.NotFoundRate)
	}
}

func TestAggregateAnswers_EmptyIsZeroNotNaN(t *testing.T) {
	st := AggregateAnswers("t", nil)
	if st.Accuracy != 0 || st.NotFoundRate != 0 || st.Queries != 0 {
		t.Errorf("empty aggregate = %+v, want zeroes", st)
	}
}

// TestLayerMessages_MapsTwoSpeakersToRolesButKeepsTheirNames — role alone loses
// identity, and LoCoMo asks who said what.
func TestLayerMessages_MapsTwoSpeakersToRolesButKeepsTheirNames(t *testing.T) {
	convs, _ := parseFixture(t)
	sessions := convs[0].LayerMessages()
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	first := sessions[0]
	if first[0].Role != "user" {
		t.Errorf("first speaker mapped to %q, want user", first[0].Role)
	}
	if len(first) < 2 || first[1].Role != "assistant" {
		t.Errorf("second speaker mapped to %q, want assistant", first[1].Role)
	}
	if !strings.Contains(first[0].Content, "Ada") {
		t.Errorf("content %q dropped the speaker name", first[0].Content)
	}
	if !strings.Contains(first[0].Content, "3 May, 2023") {
		t.Errorf("content %q dropped the session timestamp", first[0].Content)
	}
	// The same speaker keeps the same role across sessions.
	if sessions[1][0].Role != "user" {
		t.Errorf("Ada mapped to %q in session 2, want the same role as session 1", sessions[1][0].Role)
	}
}

func TestSampleQueries_IsDeterministicAndCoversEveryCategory(t *testing.T) {
	var qs []Query
	for i := 0; i < 40; i++ {
		qs = append(qs, Query{Question: "q", Category: 4, Expected: []string{"x"}})
	}
	for i := 0; i < 8; i++ {
		qs = append(qs, Query{Question: "m", Category: 1, Expected: []string{"x"}})
	}
	qs = append(qs, Query{Question: "o", Category: 3, Expected: []string{"x"}})

	a := SampleQueries(qs, 12)
	b := SampleQueries(qs, 12)
	if len(a) != len(b) {
		t.Fatalf("sample sizes differ between runs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Question != b[i].Question || a[i].Category != b[i].Category {
			t.Fatalf("sample is not deterministic at %d — a regression could not be distinguished from resampling", i)
		}
	}
	if len(a) > 12 {
		t.Errorf("sample size %d exceeds the requested 12", len(a))
	}
	// The one-question category must still be represented, or a whole ability
	// silently drops out of the report.
	if countCat(a, 3) == 0 {
		t.Error("the single open-domain question was dropped from the sample")
	}
	if n := SampleQueries(qs, 0); len(n) != len(qs) {
		t.Errorf("sample 0 returned %d, want all %d", len(n), len(qs))
	}
}

func TestUnframeSSE_ExtractsTheDataLine(t *testing.T) {
	framed := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1}\n\n"
	if got := string(unframeSSE([]byte(framed))); got != `{"jsonrpc":"2.0","id":1}` {
		t.Errorf("unframeSSE = %q", got)
	}
	plain := `{"jsonrpc":"2.0","id":1}`
	if got := string(unframeSSE([]byte(plain))); got != plain {
		t.Errorf("unframeSSE mangled a plain JSON body: %q", got)
	}
}

// mcpStub serves just enough MCP for the drain tests, answering spawn_run with
// the supplied pass reports in order.
func mcpStub(t *testing.T, reports []string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Mcp-Session-Id", "sess-1")
		if req.Method == "initialize" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		n := atomic.AddInt32(&calls, 1)
		idx := int(n) - 1
		if idx >= len(reports) {
			idx = len(reports) - 1
		}
		ack, _ := json.Marshal(map[string]any{
			"run_id": "r_1", "status": "completed", "final_text": reports[idx],
		})
		out, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": string(ack)}}},
		})
		_, _ = w.Write(out)
	}))
	return srv, &calls
}

func TestConsolidateDrain_StopsWhenTheQueueReachesZero(t *testing.T) {
	srv, calls := mcpStub(t, []string{
		"facts written 4; queued items 3, acked 1; watermark unchanged",
		"facts written 2; queued items 0, acked 3; watermark advanced",
	})
	defer srv.Close()
	var out bytes.Buffer
	mc := NewMCPClient(srv.URL, "tok", 5*time.Second)
	facts, passes, err := consolidateDrain(context.Background(), mc, "u", 12, &out)
	if err != nil {
		t.Fatalf("consolidateDrain: %v", err)
	}
	if passes != 2 {
		t.Errorf("passes = %d, want 2 (stop as soon as the queue is empty)", passes)
	}
	if facts != 6 {
		t.Errorf("facts = %d, want 6 summed across passes", facts)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("spawned %d runs, want 2 — extra passes are extractor spend", got)
	}
}

func TestConsolidateDrain_HonoursThePassCeiling(t *testing.T) {
	srv, calls := mcpStub(t, []string{"facts written 1; queued items 9, acked 1"})
	defer srv.Close()
	var out bytes.Buffer
	mc := NewMCPClient(srv.URL, "tok", 5*time.Second)
	if _, _, err := consolidateDrain(context.Background(), mc, "u", 3, &out); err != nil {
		t.Fatalf("consolidateDrain: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("spawned %d runs, want exactly the 3-pass ceiling", got)
	}
	if !strings.Contains(out.String(), "still queued") {
		t.Errorf("a truncated drain was not reported: %q", out.String())
	}
}

// TestAnswerOne_AbstentionSkipsTheJudgeAndScoresWrong — NOT_FOUND cannot match a
// gold answer, so spending a judge call on it only adds a chance to mis-grade.
func TestAnswerOne_AbstentionSkipsTheJudgeAndScoresWrong(t *testing.T) {
	srv, calls := mcpStub(t, []string{"NOT_FOUND"})
	defer srv.Close()
	mc := NewMCPClient(srv.URL, "tok", 5*time.Second)
	res := answerOne(context.Background(), mc, "u",
		Query{Question: "when?", Category: 2, Answer: "May 2023"}, "a", "j", false)
	if res.Verdict != VerdictWrong {
		t.Errorf("Verdict = %q, want wrong", res.Verdict)
	}
	if !res.NotFound {
		t.Error("NotFound = false, want the abstention flagged for the report")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("made %d runs, want 1 — the judge call should be skipped", got)
	}
}

// TestConsolidateDrain_ReportsALeasedTargetAsItsOwnError — the failure that
// produced a false 0.0000 in the first live run. Every pass answered "target
// busy", which matched no queue-depth pattern, so the loop treated a blocked
// target as a drained one and grading proceeded against an empty store.
func TestConsolidateDrain_ReportsALeasedTargetAsItsOwnError(t *testing.T) {
	srv, calls := mcpStub(t, []string{
		"target busy: another consolidator holds this target's lease. Nothing read, nothing written.",
	})
	defer srv.Close()
	var out bytes.Buffer
	mc := NewMCPClient(srv.URL, "tok", 5*time.Second)
	_, _, err := consolidateDrain(context.Background(), mc, "u", 12, &out)
	if err == nil {
		t.Fatal("a leased target was reported as a successful drain")
	}
	var busy *ErrConsolidatorBusy
	if !errors.As(err, &busy) {
		t.Fatalf("err = %T %v, want ErrConsolidatorBusy so the caller can tell it apart", err, err)
	}
	if !strings.Contains(err.Error(), "30 minutes") {
		t.Errorf("the error does not tell the operator how long the lease lasts: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("spawned %d passes against a leased target, want 1", got)
	}
}

// TestConsolidateDrain_TreatsAReportWithoutAQueueDepthAsDrained — a pass with
// nothing to do says so in prose. That is a legitimate stop, distinct from the
// leased case above, and must not be an error.
func TestConsolidateDrain_TreatsAReportWithoutAQueueDepthAsDrained(t *testing.T) {
	srv, _ := mcpStub(t, []string{"nothing to consolidate for this target"})
	defer srv.Close()
	var out bytes.Buffer
	mc := NewMCPClient(srv.URL, "tok", 5*time.Second)
	if _, passes, err := consolidateDrain(context.Background(), mc, "u", 12, &out); err != nil {
		t.Fatalf("a nothing-to-do pass was reported as an error: %v", err)
	} else if passes != 1 {
		t.Errorf("passes = %d, want 1", passes)
	}
	if !strings.Contains(out.String(), "treating as drained") {
		t.Errorf("stop reason not explained: %q", out.String())
	}
}

// memoryStub answers Memory-tool calls by op, recording the ops it saw in order.
// It exists because the flush's whole content is a SEQUENCE — lease, then drain,
// then ack, then release — and the ordering is what makes it correct: acking
// without the lease is refused by the server, and releasing before the last ack
// would hand the target away mid-flush.
func memoryStub(t *testing.T, acquired bool, pages [][]string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	ops := []string{}
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Mcp-Session-Id", "sess-1")
		if req.Method == "initialize" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		op, _ := req.Params.Arguments["op"].(string)
		mu.Lock()
		ops = append(ops, op)
		var payload map[string]any
		switch op {
		case "cursor_lease":
			payload = map[string]any{"acquired": acquired, "leased_by": "someone-else",
				"lease_expires_at": "2026-08-28T09:00:00Z"}
		case "pending_drain":
			rows := []any{}
			if page < len(pages) {
				for _, id := range pages[page] {
					rows = append(rows, map[string]any{"id": id})
				}
				page++
			}
			payload = map[string]any{"pending": rows}
		case "pending_ack":
			ids, _ := req.Params.Arguments["ids"].([]any)
			payload = map[string]any{"ok": true, "acked": len(ids)}
		default:
			payload = map[string]any{"ok": true}
		}
		mu.Unlock()
		body, _ := json.Marshal(payload)
		out, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": string(body)}}},
		})
		_, _ = w.Write(out)
	}))
	return srv, &ops
}

// TestFlushPendingQueue_DiscardsEveryPageWithoutSpawningAnything is the point of
// the flush: leftovers go away without an extractor call. The pre-ingest step used
// to run up to twelve full consolidation passes to achieve this, which cost the
// whole extractor spend on data the harness was about to purge and re-ingest —
// measured at ~70 minutes on one run before it reached its own conversation.
func TestFlushPendingQueue_DiscardsEveryPageWithoutSpawningAnything(t *testing.T) {
	srv, ops := memoryStub(t, true, [][]string{
		{"pend-1", "pend-2"},
		{"pend-3"},
		{}, // the queue is empty; the loop must stop here
	})
	defer srv.Close()
	mc := NewMCPClient(srv.URL, "tok", 30*time.Second)

	n, err := flushPendingQueue(context.Background(), mc, io.Discard)
	if err != nil {
		t.Fatalf("flushPendingQueue: %v", err)
	}
	if n != 3 {
		t.Errorf("flushed %d, want 3 across two pages", n)
	}
	got := strings.Join(*ops, ",")
	want := "cursor_lease,pending_drain,pending_ack,pending_drain,pending_ack,pending_drain,cursor_release"
	if got != want {
		t.Errorf("op sequence =\n  %s\nwant\n  %s", got, want)
	}
	// No agent was spawned — that is the whole saving.
	if strings.Contains(got, "spawn") {
		t.Errorf("the flush spawned something: %s", got)
	}
}

// TestFlushPendingQueue_RefusesWhenTheTargetIsLeased. Another pass holding the
// lease is READING the queue this would empty, so flushing around it would delete
// the input it is mid-way through. Refuse, and say who holds it.
func TestFlushPendingQueue_RefusesWhenTheTargetIsLeased(t *testing.T) {
	srv, ops := memoryStub(t, false, [][]string{{"pend-1"}})
	defer srv.Close()
	mc := NewMCPClient(srv.URL, "tok", 30*time.Second)

	n, err := flushPendingQueue(context.Background(), mc, io.Discard)
	if err == nil {
		t.Fatal("flushed a target another pass holds")
	}
	var busy *ErrConsolidatorBusy
	if !errors.As(err, &busy) {
		t.Errorf("error = %T (%v), want ErrConsolidatorBusy so the caller can report it as busy", err, err)
	}
	if n != 0 {
		t.Errorf("flushed %d items despite not holding the lease", n)
	}
	// It must NOT have acked anything, and must not hold a lease it never got.
	got := strings.Join(*ops, ",")
	if strings.Contains(got, "pending_ack") {
		t.Errorf("acked without the lease: %s", got)
	}
	if strings.Contains(got, "cursor_release") {
		t.Errorf("released a lease it never acquired: %s", got)
	}
}

// TestSeedTurnRows_WritesEveryTurnEmbeddedIntoTheAnswerersPartition.
//
// The measurement this enables is the whole point, so the properties worth pinning
// are the ones that silently produce a zero: every turn present (a missing turn is
// an unanswerable question), embed ON (an unembedded row is invisible to semantic
// search), keyed by dia_id (the retrieval axis's convention), and the SESSION
// TIMESTAMP carried in the body — LoCoMo dates live on the session, not the turn,
// so without the prefix the temporal category is unretrievable at any embedder
// quality.
//
// Measured before this existed: the answerer had 29 facts distilled from 419 turns
// and abstained on 72% of questions; with the turns seeded it scored 0.7353.
func TestSeedTurnRows_WritesEveryTurnEmbeddedIntoTheAnswerersPartition(t *testing.T) {
	type put struct {
		path string
		body map[string]any
	}
	var mu sync.Mutex
	var puts []put
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		puts = append(puts, put{path: r.URL.Path, body: body})
		mu.Unlock()
		_, _ = w.Write([]byte(`{"key":"k","embedded":true}`))
	}))
	defer srv.Close()

	conv := Conversation{Turns: []Turn{
		{DiaID: "D1:1", Speaker: "Caroline", Text: "I bought a clay pot.", DateTime: "1:36 pm on 3 July, 2023"},
		{DiaID: "D1:2", Speaker: "Melanie", Text: "Nice!", DateTime: "1:36 pm on 3 July, 2023"},
	}}
	rest := NewClient(srv.URL, "tok", 5*time.Second)

	n, err := seedTurnRows(context.Background(), rest, "subj-1", conv, options{concurrency: 2}, io.Discard)
	if err != nil {
		t.Fatalf("seedTurnRows: %v", err)
	}
	if n != 2 {
		t.Errorf("seeded %d, want 2 (one row per turn)", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(puts) != 2 {
		t.Fatalf("PUTs = %d, want one per turn: %+v", len(puts), puts)
	}
	seenDia := map[string]bool{}
	for _, p := range puts {
		// The answerer reads user/<subject>; anything else is a partition it cannot see.
		if !strings.Contains(p.path, "/user/") || !strings.Contains(p.path, "subj-1") {
			t.Errorf("PUT path %q does not address user/subj-1", p.path)
		}
		for _, d := range []string{"D1:1", "D1:2"} {
			if strings.Contains(p.path, d) {
				seenDia[d] = true
			}
		}
		if embed, _ := p.body["embed"].(bool); !embed {
			t.Errorf("PUT %s did not request embedding — an unembedded row is invisible to search: %+v", p.path, p.body)
		}
		val, _ := p.body["value"].(string)
		if !strings.Contains(val, "3 July, 2023") {
			t.Errorf("PUT %s body carries no session timestamp, so temporal questions cannot retrieve it: %q", p.path, val)
		}
	}
	if len(seenDia) != 2 {
		t.Errorf("rows are not keyed by dia_id; saw %v", seenDia)
	}
}
