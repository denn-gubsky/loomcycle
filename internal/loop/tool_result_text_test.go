package loop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/providers"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func dur(d time.Duration) *time.Duration { return &d }

// decodeInband parses a rendered failure, failing the test when it is not the
// structured error object.
func decodeInband(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("a failure did not render as one JSON object: %v\n%s", err, s)
	}
	if m["isError"] != true {
		t.Fatalf("rendered failure lacks isError:true:\n%s", s)
	}
	return m
}

// A success is the tool's own output, byte for byte: the structured shape is
// for failures only, so it changes no successful call's prompt.
func TestRenderToolResultText_SuccessIsUntouched(t *testing.T) {
	for _, text := range []string{"ordinary output", `{"value":null}`, ""} {
		if got := renderToolResultText(tools.Result{Text: text}); got != text {
			t.Errorf("success text altered:\n got %q\nwant %q", got, text)
		}
	}
}

// An unclassified failure is still the object — the model reads one shape for
// every failure — but claims nothing it does not know: no category, and no
// isRetryable, which is only known once a failure is classified.
func TestRenderToolResultText_UnclassifiedFailureClaimsNothing(t *testing.T) {
	for _, res := range []tools.Result{
		{Text: "a failure nobody classified", IsError: true},
		{Text: "a failure nobody classified", IsError: true, Error: &tools.ErrorInfo{}},
	} {
		got := renderToolResultText(res)
		if got != `{"isError":true,"error":"a failure nobody classified"}` {
			t.Errorf("unclassified failure rendered as:\n%s", got)
		}
	}
}

func TestRenderToolResultText_CarriesTheDecision(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  tools.Result
		want map[string]any
		deny []string
	}{
		{
			name: "transient with a backoff",
			res: tools.Result{
				Text:    "spawn_run: backpressure",
				IsError: true,
				Error: &tools.ErrorInfo{
					Category:    tools.CategoryTransient,
					Retryable:   true,
					Description: "The runtime is at a concurrency limit.",
					RetryAfter:  dur(5 * time.Second),
				},
			},
			want: map[string]any{
				"error": "spawn_run: backpressure", "errorCategory": "transient", "isRetryable": true,
				"retryAfterSeconds": float64(5), "description": "The runtime is at a concurrency limit.",
			},
		},
		{
			name: "business is explicitly NOT retryable",
			res: tools.Result{
				Text:    "spawn_run: token_limit_exceeded",
				IsError: true,
				Error: &tools.ErrorInfo{
					Category:    tools.CategoryBusiness,
					Retryable:   false,
					Description: "The token budget for this scope is exhausted.",
				},
			},
			want: map[string]any{"errorCategory": "business", "isRetryable": false},
			// A backoff here would tell the model to wait for something that
			// cannot clear without a human.
			deny: []string{"retryAfterSeconds"},
		},
		{
			name: "retryable with no hint says nothing about timing",
			res: tools.Result{
				Text:    "memory: backend unavailable",
				IsError: true,
				Error:   &tools.ErrorInfo{Category: tools.CategoryTransient, Retryable: true},
			},
			want: map[string]any{"errorCategory": "transient", "isRetryable": true},
			deny: []string{"retryAfterSeconds", "description"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeInband(t, renderToolResultText(tc.res))
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v", k, got[k], v)
				}
			}
			for _, d := range tc.deny {
				if _, ok := got[d]; ok {
					t.Errorf("unexpectedly carries %q: %v", d, got)
				}
			}
			// The tool's own output is never lost.
			if got["error"] != tc.res.Text {
				t.Errorf("error = %v, want the tool's own text %q", got["error"], tc.res.Text)
			}
		})
	}
}

// A zero backoff must not render. "Wait as you judge best" and "retry
// immediately" are opposite instructions and 0 would silently mean the second.
func TestRenderToolResultText_ZeroBackoffIsNotRendered(t *testing.T) {
	got := decodeInband(t, renderToolResultText(tools.Result{
		Text:    "x",
		IsError: true,
		Error: &tools.ErrorInfo{
			Category: tools.CategoryTransient, Retryable: true, RetryAfter: dur(0),
		},
	}))
	if _, ok := got["retryAfterSeconds"]; ok {
		t.Errorf("rendered a zero backoff as a hint: %v", got)
	}
}

// A description that merely restates the tool's message adds tokens and no
// decision, so it is not printed twice.
func TestRenderToolResultText_DoesNotRestateTheSameSentence(t *testing.T) {
	const msg = "the runtime is paused"
	got := decodeInband(t, renderToolResultText(tools.Result{
		Text:    msg,
		IsError: true,
		Error: &tools.ErrorInfo{
			Category: tools.CategoryTransient, Retryable: true, Description: msg,
		},
	}))
	if _, ok := got["description"]; ok {
		t.Errorf("the description restates the error: %v", got)
	}
}

// The correct call format rides as its own section, not as prose in the error.
func TestRenderToolResultText_CarriesTheCorrectCallFormat(t *testing.T) {
	got := decodeInband(t, renderToolResultText(tools.Result{
		Text:    "Document: unknown argument \"text\" — nothing was done.",
		IsError: true,
		Error: &tools.ErrorInfo{
			Category:    tools.CategoryValidation,
			Description: "Pass only this tool's own arguments, at the top level.",
			CallFormat: &tools.CallFormat{
				Tool: "Document", Op: "create_chunk",
				Example:   json.RawMessage(`{"op":"create_chunk","document_id":"d1","title":"Flights"}`),
				Reference: &tools.CallRef{Tool: "Context", Input: json.RawMessage(`{"op":"help","topic":"Document/create_chunk"}`)},
			},
		},
	}))
	cf, ok := got["correctCallFormat"].(map[string]any)
	if !ok {
		t.Fatalf("no correctCallFormat section: %v", got)
	}
	if cf["tool"] != "Document" || cf["op"] != "create_chunk" {
		t.Errorf("correctCallFormat = %v", cf)
	}
	if ex, _ := cf["example"].(map[string]any); ex["title"] != "Flights" {
		t.Errorf("the example is not the argument object: %v", cf["example"])
	}
	if strings.Contains(got["error"].(string), "Flights") {
		t.Errorf("the example leaked into the error text: %v", got["error"])
	}
}

// Rendering is deterministic, and happens once: the emitted event is what gets
// persisted, and replayTranscript rebuilds the model's tool_result block from
// that persisted text without rendering it again. So the same call reads the
// same before and after a resume.
func TestRenderToolResultText_IsStableForPersistAndReplay(t *testing.T) {
	res := tools.Result{
		Text:    "spawn_run: runtime paused",
		IsError: true,
		Error: &tools.ErrorInfo{
			Category:    tools.CategoryTransient,
			Retryable:   true,
			Description: "An operator resume will clear it.",
			RetryAfter:  dur(15 * time.Second),
		},
	}
	if first, second := renderToolResultText(res), renderToolResultText(res); first != second {
		t.Fatalf("renderer is not deterministic:\n1: %s\n2: %s", first, second)
	}
}

// --- the placement, not just the renderer ---

type classifiedTool struct{ res tools.Result }

func (c *classifiedTool) Name() string                 { return "failer" }
func (c *classifiedTool) Description() string          { return "fails in a classified way" }
func (c *classifiedTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (c *classifiedTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	return c.res, nil
}

// TestExecutePendingTools_EventAndBlockCarryTheSameText covers WHERE the
// rendering happens, which the renderer's own tests cannot see.
//
// A probe confirmed the gap: reverting the emitted event to the raw text left
// every renderer test green, because they exercise the function and not its
// two call sites. That matters more here than usual — the emitted event is
// what gets persisted, and replayTranscript rebuilds the model's tool_result
// block from that persisted Text. If the two diverge, a resumed run feeds the
// model a different prompt than the original run did, and the classification
// disappears on replay.
func TestExecutePendingTools_EventAndBlockCarryTheSameText(t *testing.T) {
	tool := &classifiedTool{res: tools.Result{
		Text:    "spawn_run: backpressure",
		IsError: true,
		Error: &tools.ErrorInfo{
			Category:    tools.CategoryTransient,
			Retryable:   true,
			Description: "The runtime is at a concurrency limit.",
			RetryAfter:  dur(5 * time.Second),
		},
	}}

	var emitted []providers.Event
	blocks := executePendingTools(
		context.Background(),
		tools.NewDispatcher([]tools.Tool{tool}),
		[]providers.ToolUse{{ID: "tu-1", Name: "failer", Input: json.RawMessage(`{}`)}},
		1, nil, hooks.Identity{},
		func(ev providers.Event) { emitted = append(emitted, ev) },
	)

	if len(blocks) != 1 {
		t.Fatalf("expected one block, got %d", len(blocks))
	}
	var toolResultEvents []providers.Event
	for _, ev := range emitted {
		if ev.Type == providers.EventToolResult {
			toolResultEvents = append(toolResultEvents, ev)
		}
	}
	if len(toolResultEvents) != 1 {
		t.Fatalf("expected one tool_result event, got %d", len(toolResultEvents))
	}

	blockText := blocks[0].Text
	eventText := toolResultEvents[0].Text

	if decodeInband(t, blockText)["errorCategory"] != "transient" {
		t.Errorf("the MODEL's block is missing the classification:\n%s", blockText)
	}
	if eventText != blockText {
		t.Errorf("the PERSISTED event text differs from what the model saw — a resumed run "+
			"would replay a different prompt and lose the classification:\n event: %q\n block: %q",
			eventText, blockText)
	}
}

// A failed call's tool_result EVENT carries the failure's structure as a field,
// so an SSE or gRPC consumer reads the category and the correct call format
// without parsing the text the model reads — and on the SSE wire it arrives
// under the keys the client declares.
func TestExecutePendingTools_TheFailedEventCarriesItsErrorInfo(t *testing.T) {
	tool := &classifiedTool{res: tools.Result{
		Text:    "Document: unknown argument",
		IsError: true,
		Error: &tools.ErrorInfo{
			Category: tools.CategoryValidation,
			CallFormat: &tools.CallFormat{Tool: "Document", Op: "create_chunk",
				Example: json.RawMessage(`{"op":"create_chunk"}`)},
		},
	}}
	var emitted []providers.Event
	executePendingTools(
		context.Background(),
		tools.NewDispatcher([]tools.Tool{tool}),
		[]providers.ToolUse{{ID: "tu-1", Name: "failer", Input: json.RawMessage(`{}`)}},
		1, nil, hooks.Identity{},
		func(ev providers.Event) { emitted = append(emitted, ev) },
	)
	var ev *providers.Event
	for i := range emitted {
		if emitted[i].Type == providers.EventToolResult {
			ev = &emitted[i]
		}
	}
	if ev == nil || ev.ErrorInfo == nil || ev.ErrorInfo.Category != tools.CategoryValidation || ev.ErrorInfo.CallFormat == nil {
		t.Fatalf("tool_result event = %+v; want its error_info", ev)
	}
	raw, _ := json.Marshal(ev)
	var wire struct {
		ErrorInfo map[string]any `json:"error_info"`
	}
	_ = json.Unmarshal(raw, &wire)
	if wire.ErrorInfo["category"] != "validation" || wire.ErrorInfo["correct_call_format"] == nil {
		t.Errorf("SSE error_info = %v; want category and correct_call_format", wire.ErrorInfo)
	}
}

// An unclassified tool must leave both paths byte-identical to before, so this
// phase touches only failing calls.
func TestExecutePendingTools_UnclassifiedIsUnchangedOnBothPaths(t *testing.T) {
	const raw = "ordinary tool output"
	tool := &classifiedTool{res: tools.Result{Text: raw}}

	var emitted []providers.Event
	blocks := executePendingTools(
		context.Background(),
		tools.NewDispatcher([]tools.Tool{tool}),
		[]providers.ToolUse{{ID: "tu-1", Name: "failer", Input: json.RawMessage(`{}`)}},
		1, nil, hooks.Identity{},
		func(ev providers.Event) { emitted = append(emitted, ev) },
	)
	if blocks[0].Text != raw {
		t.Errorf("block text altered: %q", blocks[0].Text)
	}
	for _, ev := range emitted {
		if ev.Type == providers.EventToolResult && ev.Text != raw {
			t.Errorf("event text altered: %q", ev.Text)
		}
		if ev.Type == providers.EventToolResult && ev.ErrorInfo != nil {
			t.Errorf("a success carries error_info: %+v", ev.ErrorInfo)
		}
	}
}

// transientFailure is the classified failure the hook-path tests run through.
func transientFailure() tools.Result {
	return tools.Result{
		Text:    "upstream: connection reset",
		IsError: true,
		Error: &tools.ErrorInfo{
			Category:    tools.CategoryTransient,
			Retryable:   true,
			Description: "The upstream dropped the connection.",
		},
	}
}

// postHookServer answers every Post call with rewrite(original).
func postHookServer(t *testing.T, rewrite func(hooks.ToolResult) *hooks.ToolResult) *hooks.Dispatcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call hooks.PostHookCall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hooks.PostHookResult{Result: rewrite(call.ToolResult)})
	}))
	t.Cleanup(srv.Close)
	reg := hooks.NewSet()
	if _, err := reg.Register(&hooks.Hook{
		Owner: "test", Name: "post", Phase: hooks.PhasePost,
		CallbackURL: srv.URL, Tools: []string{"failer"},
	}); err != nil {
		t.Fatalf("register hook: %v", err)
	}
	return hooks.NewDispatcher(reg, nil)
}

func runFailerThrough(t *testing.T, hd *hooks.Dispatcher) string {
	t.Helper()
	blocks := executePendingTools(
		context.Background(),
		tools.NewDispatcher([]tools.Tool{&classifiedTool{res: transientFailure()}}),
		[]providers.ToolUse{{ID: "tu-1", Name: "failer", Input: json.RawMessage(`{}`)}},
		1, hd, hooks.Identity{},
		func(providers.Event) {},
	)
	if len(blocks) != 1 {
		t.Fatalf("expected one block, got %d", len(blocks))
	}
	return blocks[0].Text
}

// The server ALWAYS wires a hook dispatcher, whether or not any hook matches,
// so the dispatcher path is the production path. The tests above pass nil and
// never took it — which is how rebuilding the result from the hook's two wire
// fields dropped every classification in production while they stayed green.
func TestExecutePendingTools_ClassificationSurvivesAnIdleHookDispatcher(t *testing.T) {
	text := runFailerThrough(t, hooks.NewDispatcher(hooks.NewSet(), nil))
	if decodeInband(t, text)["errorCategory"] != "transient" {
		t.Errorf("a dispatcher with no matching hook dropped the classification:\n%s", text)
	}
}

// The canonical Post hook wraps the text and leaves the failure a failure.
// The classification still describes the call, so it must still reach the model.
func TestExecutePendingTools_ClassificationSurvivesAWrappingPostHook(t *testing.T) {
	hd := postHookServer(t, func(r hooks.ToolResult) *hooks.ToolResult {
		return &hooks.ToolResult{Text: "<untrusted>" + r.Text + "</untrusted>", IsError: r.IsError}
	})
	text := runFailerThrough(t, hd)
	got := decodeInband(t, text)
	if got["errorCategory"] != "transient" {
		t.Errorf("a wrapping Post hook dropped the classification:\n%s", text)
	}
	if got["error"] != "<untrusted>upstream: connection reset</untrusted>" {
		t.Errorf("the hook's rewrite did not reach the model:\n%s", text)
	}
}

// A hook that turns a failure into a success must not leave the model reading
// a retry instruction over a successful result.
func TestExecutePendingTools_PostHookSuccessDropsTheFailureClassification(t *testing.T) {
	hd := postHookServer(t, func(hooks.ToolResult) *hooks.ToolResult {
		return &hooks.ToolResult{Text: "served from cache"}
	})
	text := runFailerThrough(t, hd)
	if text != "served from cache" {
		t.Errorf("a success rewrite still carries the failure's classification:\n%s", text)
	}
}
