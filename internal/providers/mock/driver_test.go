package mock

import (
	"context"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// userTextMessage builds a synthetic user-text message — used to
// seed the initial conversation (no prior tool_results) for FSM
// step-0 tests.
func userTextMessage(text string) providers.Message {
	return providers.Message{
		Role: "user",
		Content: []providers.ContentBlock{
			{Type: "text", Text: text},
		},
	}
}

// toolResultMessage wraps a tool_result block. Drives the FSM
// counter (countToolResults) forward by exactly one.
func toolResultMessage(text string) providers.Message {
	return providers.Message{
		Role: "user",
		Content: []providers.ContentBlock{
			{Type: "tool_result", Text: text},
		},
	}
}

func drain(t *testing.T, ch <-chan providers.Event) []providers.Event {
	t.Helper()
	var out []providers.Event
	for e := range ch {
		out = append(out, e)
	}
	return out
}

func findToolCall(events []providers.Event) *providers.ToolUse {
	for _, e := range events {
		if e.Type == providers.EventToolCall && e.ToolUse != nil {
			return e.ToolUse
		}
	}
	return nil
}

func decodeInput(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode tool input: %v (raw=%s)", err, string(raw))
	}
	return m
}

// TestMockResearcher_FirstTurnEmitsMemorySet — empty history must
// produce Memory.set as the first tool call with scope=user and the
// canonical c{N}-research key.
func TestMockResearcher_FirstTurnEmitsMemorySet(t *testing.T) {
	d := New()
	d.latencyBase = 0
	d.latencyJitter = 0

	ch, err := d.Call(context.Background(), providers.Request{
		Model:    "mock-researcher",
		Messages: []providers.Message{userTextMessage("Your circuit_id is c7. Question: What does TCP stand for?")},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	events := drain(t, ch)
	tu := findToolCall(events)
	if tu == nil {
		t.Fatalf("no tool_use in events: %+v", events)
	}
	if tu.Name != "Memory" {
		t.Errorf("tool name = %q, want Memory", tu.Name)
	}
	in := decodeInput(t, tu.Input)
	if in["op"] != "set" {
		t.Errorf("op = %v, want set", in["op"])
	}
	if in["scope"] != "user" {
		t.Errorf("scope = %v, want user", in["scope"])
	}
	if in["key"] != "c7-research" {
		t.Errorf("key = %v, want c7-research", in["key"])
	}
	if got := events[len(events)-1]; got.Type != providers.EventDone || got.StopReason != "tool_use" {
		t.Errorf("last event = %+v, want Done(tool_use)", got)
	}
}

// TestMockEditor_FullSequence drives the 5-step FSM by feeding
// tool_results one at a time and asserts each iteration's tool_use
// shape. At step 4 the editor's Channel.publish must carry
// editor_run_id extracted from the prior Context.self tool_result.
func TestMockEditor_FullSequence(t *testing.T) {
	d := New()
	d.latencyBase = 0
	d.latencyJitter = 0

	circuitPrompt := userTextMessage("circuit_id is c12. Edit the research.")

	// Step 0: empty history → Channel.subscribe
	ch, _ := d.Call(context.Background(), providers.Request{
		Model:    "mock-editor",
		Messages: []providers.Message{circuitPrompt},
	})
	tu := findToolCall(drain(t, ch))
	if tu == nil || tu.Name != "Channel" {
		t.Fatalf("step 0: want Channel.subscribe, got %+v", tu)
	}
	in := decodeInput(t, tu.Input)
	if in["op"] != "subscribe" || in["channel"] != "research-done/c12" {
		t.Errorf("step 0: input = %+v", in)
	}

	// Step 1: + 1 tool_result → Memory.get
	ch, _ = d.Call(context.Background(), providers.Request{
		Model: "mock-editor",
		Messages: []providers.Message{
			circuitPrompt,
			toolResultMessage(`{"channel":"research-done/c12","messages":[{"value":{"circuit_id":"c12"}}]}`),
		},
	})
	tu = findToolCall(drain(t, ch))
	in = decodeInput(t, tu.Input)
	if tu.Name != "Memory" || in["op"] != "get" || in["key"] != "c12-research" {
		t.Errorf("step 1: name=%s in=%+v", tu.Name, in)
	}

	// Step 2: Memory.set (edited)
	ch, _ = d.Call(context.Background(), providers.Request{
		Model: "mock-editor",
		Messages: []providers.Message{
			circuitPrompt,
			toolResultMessage(`{"channel":"…"}`),
			toolResultMessage(`{"value":{"text":"the original research"}}`),
		},
	})
	tu = findToolCall(drain(t, ch))
	in = decodeInput(t, tu.Input)
	if tu.Name != "Memory" || in["op"] != "set" || in["key"] != "c12-research-edited" {
		t.Errorf("step 2: name=%s in=%+v", tu.Name, in)
	}

	// Step 3: Context.self
	ch, _ = d.Call(context.Background(), providers.Request{
		Model: "mock-editor",
		Messages: []providers.Message{
			circuitPrompt,
			toolResultMessage(`{}`), toolResultMessage(`{}`), toolResultMessage(`{}`),
		},
	})
	tu = findToolCall(drain(t, ch))
	in = decodeInput(t, tu.Input)
	if tu.Name != "Context" || in["op"] != "self" {
		t.Errorf("step 3: name=%s in=%+v", tu.Name, in)
	}

	// Step 4: Channel.publish, editor_run_id extracted from
	// Context.self tool_result (the last one in history).
	ch, _ = d.Call(context.Background(), providers.Request{
		Model: "mock-editor",
		Messages: []providers.Message{
			circuitPrompt,
			toolResultMessage(`{}`), toolResultMessage(`{}`), toolResultMessage(`{}`),
			toolResultMessage(`{"agent_id":"editor-c12","run_id":"r_abcdef123"}`),
		},
	})
	tu = findToolCall(drain(t, ch))
	in = decodeInput(t, tu.Input)
	if tu.Name != "Channel" || in["op"] != "publish" {
		t.Fatalf("step 4: name=%s in=%+v", tu.Name, in)
	}
	if in["channel"] != "editing-done/c12" {
		t.Errorf("step 4 channel = %v", in["channel"])
	}
	payload, _ := in["value"].(map[string]any)
	if payload["editor_run_id"] != "r_abcdef123" {
		t.Errorf("step 4 editor_run_id = %v (raw value = %+v)", payload["editor_run_id"], payload)
	}
}

// TestMockEvaluator_ScoreIsDeterministic: same editor_run_id +
// circuit_id → same score across calls.
func TestMockEvaluator_ScoreIsDeterministic(t *testing.T) {
	d := New()
	d.latencyBase = 0
	d.latencyJitter = 0

	subscribeResult := `{"messages":[{"value":{"editor_run_id":"r_abc","circuit_id":"c42"}}]}`
	build := func() providers.Request {
		return providers.Request{
			Model: "mock-evaluator",
			Messages: []providers.Message{
				userTextMessage("circuit c42"),
				toolResultMessage(subscribeResult),
				toolResultMessage(`{"value":"…"}`),
				toolResultMessage(`{"value":"…"}`),
				toolResultMessage(`{"ok":true}`),
			},
		}
	}

	ch1, _ := d.Call(context.Background(), build())
	tu1 := findToolCall(drain(t, ch1))
	in1 := decodeInput(t, tu1.Input)
	if tu1.Name != "Evaluation" || in1["op"] != "submit" {
		t.Fatalf("not the submit step: name=%s in=%+v", tu1.Name, in1)
	}

	ch2, _ := d.Call(context.Background(), build())
	tu2 := findToolCall(drain(t, ch2))
	in2 := decodeInput(t, tu2.Input)

	if in1["score"] != in2["score"] {
		t.Errorf("score not deterministic: %v vs %v", in1["score"], in2["score"])
	}
	if in1["run_id"] != "r_abc" || in2["run_id"] != "r_abc" {
		t.Errorf("run_id missing: %v / %v", in1["run_id"], in2["run_id"])
	}
}

// TestMockEvaluator_ScoreIsDistributed: across many synthetic
// run_ids the scores span the [0.50, 0.99] band without all
// clumping in one bucket.
func TestMockEvaluator_ScoreIsDistributed(t *testing.T) {
	buckets := map[int]int{}
	for i := 0; i < 1000; i++ {
		s := deriveScore("r_" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26)))
		if s < 0.50 || s > 0.99 {
			t.Errorf("score out of range: %f", s)
		}
		b := int((s - 0.50) * 100) // 0..49
		buckets[b]++
	}
	if len(buckets) < 30 {
		t.Errorf("score distribution too clumped: %d unique buckets out of 50, want >=30", len(buckets))
	}
}

// TestMockDriver_LatencyHonoursContext — ctx.Done() must short-
// circuit the latency sleep promptly.
func TestMockDriver_LatencyHonoursContext(t *testing.T) {
	d := New()
	d.latencyBase = 500 * time.Millisecond
	d.latencyJitter = 0

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := d.Call(ctx, providers.Request{
		Model:    "mock-generic",
		Messages: []providers.Message{userTextMessage("hello")},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Error("expected ctx error, got nil")
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("ctx cancel didn't short-circuit: elapsed=%v", elapsed)
	}
}

// TestMockDriver_429Injection — rate429=1.0 → 100% of calls return
// an error whose string IsRateLimit classifies as 429.
func TestMockDriver_429Injection(t *testing.T) {
	d := NewWithRNG(rand.New(rand.NewSource(1)))
	d.latencyBase = 0
	d.latencyJitter = 0
	d.rate429 = 1.0

	for i := 0; i < 10; i++ {
		_, err := d.Call(context.Background(), providers.Request{
			Model:    "mock-researcher",
			Messages: []providers.Message{userTextMessage("c1 q?")},
		})
		if err == nil {
			t.Fatalf("call %d: expected 429 injection, got nil", i)
		}
		if !providers.IsRateLimit(err) {
			t.Errorf("call %d: IsRateLimit(err) = false; err=%v", i, err)
		}
	}
}

// TestMockDriver_500Injection — rate500=1.0 + rate429=0 → 500 errors
// matching the "<provider> <code>: msg" shape that errclass parses.
func TestMockDriver_500Injection(t *testing.T) {
	d := NewWithRNG(rand.New(rand.NewSource(1)))
	d.latencyBase = 0
	d.latencyJitter = 0
	d.rate429 = 0
	d.rate500 = 1.0

	_, err := d.Call(context.Background(), providers.Request{
		Model:    "mock-researcher",
		Messages: []providers.Message{userTextMessage("c1 q?")},
	})
	if err == nil {
		t.Fatal("expected 500 injection, got nil")
	}
	if !strings.HasPrefix(err.Error(), "mock 500:") {
		t.Errorf("err shape = %q; want mock 500: prefix", err.Error())
	}
	if providers.IsRateLimit(err) {
		t.Errorf("500 misclassified as rate-limit: %v", err)
	}
}

// TestMockDriver_UsageReflectsRequestSize — longer messages produce
// proportionally larger InputTokens. Sanity that the loop's totalUsage
// merge has non-trivial data to plot.
func TestMockDriver_UsageReflectsRequestSize(t *testing.T) {
	d := New()
	d.latencyBase = 0
	d.latencyJitter = 0

	short := strings.Repeat("a", 100)
	long := strings.Repeat("a", 5000)

	// Usage is attached to EventDone (the loop reads it from there;
	// a separate EventUsage frame would be ignored by the loop's
	// switch arms — see internal/loop/loop.go).
	usageFor := func(text string) *providers.Usage {
		ch, _ := d.Call(context.Background(), providers.Request{
			Model:    "mock-generic",
			Messages: []providers.Message{userTextMessage(text)},
		})
		for _, e := range drain(t, ch) {
			if e.Type == providers.EventDone && e.Usage != nil {
				return e.Usage
			}
		}
		return nil
	}

	su := usageFor(short)
	lu := usageFor(long)
	if su == nil || lu == nil {
		t.Fatalf("missing Usage frame: short=%+v long=%+v", su, lu)
	}
	if lu.InputTokens <= su.InputTokens {
		t.Errorf("long InputTokens not greater: long=%d short=%d", lu.InputTokens, su.InputTokens)
	}
	if lu.Model != "mock-generic" {
		t.Errorf("Model = %q, want mock-generic", lu.Model)
	}
}

// TestMockDriver_ListModelsReturnsKnownSet — sanity that the four
// model ids are reported. The resolver pre-populates the matrix
// from this list, so a typo here cascades into agent 503s.
func TestMockDriver_ListModelsReturnsKnownSet(t *testing.T) {
	d := New()
	models, err := d.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"mock-researcher": false,
		"mock-editor":     false,
		"mock-evaluator":  false,
		"mock-generic":    false,
	}
	for _, m := range models {
		if _, ok := want[m]; !ok {
			t.Errorf("unexpected model: %s", m)
		}
		want[m] = true
	}
	for m, seen := range want {
		if !seen {
			t.Errorf("missing model: %s", m)
		}
	}
}

// TestMockDriver_EnvVarParsing — bad env-var values fall back to
// the documented defaults rather than panicking on start.
func TestMockDriver_EnvVarParsing(t *testing.T) {
	if got := parseRate("garbage", 0.42); got != 0.42 {
		t.Errorf("parseRate(garbage) = %v, want 0.42 default", got)
	}
	if got := parseRate("1.5", 0.42); got != 0.42 {
		t.Errorf("parseRate(out of range) = %v, want 0.42 default", got)
	}
	if got := parseRate("0.3", 0); got != 0.3 {
		t.Errorf("parseRate(0.3) = %v, want 0.3", got)
	}
	if got := parseDurationMS("", 42*time.Millisecond); got != 42*time.Millisecond {
		t.Errorf("parseDurationMS(empty) = %v, want default", got)
	}
	if got := parseDurationMS("-5", 42*time.Millisecond); got != 0 {
		t.Errorf("parseDurationMS(negative) = %v, want 0", got)
	}
	if got := parseDurationMS("100", 0); got != 100*time.Millisecond {
		t.Errorf("parseDurationMS(100) = %v, want 100ms", got)
	}
}

// TestExtractCircuitID — the regex finds c{N} only inside text
// blocks of user-role messages; falls back to c0 when not found.
func TestExtractCircuitID(t *testing.T) {
	cases := []struct {
		name string
		req  providers.Request
		want string
	}{
		{
			name: "happy_path",
			req: providers.Request{Messages: []providers.Message{
				userTextMessage("Your circuit_id is c42. Answer this."),
			}},
			want: "c42",
		},
		{
			name: "no_circuit",
			req: providers.Request{Messages: []providers.Message{
				userTextMessage("just a question, no id"),
			}},
			want: "c0",
		},
		{
			name: "multiple_picks_first",
			req: providers.Request{Messages: []providers.Message{
				userTextMessage("c1 and also c2 in the same line"),
			}},
			want: "c1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractCircuitID(tc.req); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
