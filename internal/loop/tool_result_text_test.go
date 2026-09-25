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

// The overwhelming majority of tool results are unclassified, and those must
// come through byte-identical — otherwise this phase changes every prompt in
// the runtime rather than only the failing ones.
func TestRenderToolResultText_UnclassifiedIsUntouched(t *testing.T) {
	for _, res := range []tools.Result{
		{Text: "ordinary output"},
		{Text: "a failure nobody classified", IsError: true},
		{Text: "classified-but-empty", IsError: true, Error: &tools.ErrorInfo{}},
	} {
		if got := renderToolResultText(res); got != res.Text {
			t.Errorf("text altered for an unclassified result:\n got %q\nwant %q", got, res.Text)
		}
	}
}

func TestRenderToolResultText_CarriesTheDecision(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  tools.Result
		want []string
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
			want: []string{"[transient", "retryable", "retry in 5s", "concurrency limit", "spawn_run: backpressure"},
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
			want: []string{"[business", "not retryable", "budget"},
			// A backoff here would tell the model to wait for something that
			// cannot clear without a human.
			deny: []string{"retry in"},
		},
		{
			name: "retryable with no hint says nothing about timing",
			res: tools.Result{
				Text:    "memory: backend unavailable",
				IsError: true,
				Error: &tools.ErrorInfo{
					Category:  tools.CategoryTransient,
					Retryable: true,
				},
			},
			want: []string{"[transient", "retryable"},
			deny: []string{"retry in", "0s"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderToolResultText(tc.res)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("unexpectedly contains %q in:\n%s", d, got)
				}
			}
			// The tool's own output is never lost — the prefix is additional
			// signal, not a replacement.
			if !strings.Contains(got, tc.res.Text) {
				t.Errorf("tool output dropped:\n%s", got)
			}
		})
	}
}

// A zero backoff must not render. "Wait as you judge best" and "retry
// immediately" are opposite instructions and 0s would silently mean the second.
func TestRenderToolResultText_ZeroBackoffIsNotRendered(t *testing.T) {
	got := renderToolResultText(tools.Result{
		Text:    "x",
		IsError: true,
		Error: &tools.ErrorInfo{
			Category: tools.CategoryTransient, Retryable: true, RetryAfter: dur(0),
		},
	})
	if strings.Contains(got, "retry in") {
		t.Errorf("rendered a zero backoff as a hint: %s", got)
	}
}

// A description that merely restates the tool's message adds tokens and no
// decision, so it is not printed twice.
func TestRenderToolResultText_DoesNotRestateTheSameSentence(t *testing.T) {
	const msg = "the runtime is paused"
	got := renderToolResultText(tools.Result{
		Text:    msg,
		IsError: true,
		Error: &tools.ErrorInfo{
			Category: tools.CategoryTransient, Retryable: true, Description: msg,
		},
	})
	if strings.Count(got, msg) != 1 {
		t.Errorf("message rendered %d times:\n%s", strings.Count(got, msg), got)
	}
}

// THE SUBTLE ONE. The emitted event is what gets persisted, and
// replayTranscript rebuilds the model's tool_result block from that persisted
// Text. Rendering at the block and not at the event would make the same tool
// call read differently before and after a resume — the classification would
// silently vanish on replay, which is the same class of bug as a continuation
// losing IsError.
//
// Asserted as a property of the renderer being deterministic and applied once:
// whatever text the event carries is exactly what the block carries.
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

	// What the event carries, and what the block carries, come from one call
	// in executePendingTools. Rendering twice must therefore be identical, or
	// a replay could not reproduce the original prompt.
	first := renderToolResultText(res)
	second := renderToolResultText(res)
	if first != second {
		t.Fatalf("renderer is not deterministic:\n1: %s\n2: %s", first, second)
	}

	// And re-rendering an already-rendered result must not double-prefix,
	// which is what a replay path would do if it classified again.
	replayed := renderToolResultText(tools.Result{Text: first, IsError: true})
	if replayed != first {
		t.Errorf("replaying a rendered result changed it:\n got %s\nwant %s", replayed, first)
	}
	if strings.Count(replayed, "[transient") != 1 {
		t.Errorf("double-prefixed on replay:\n%s", replayed)
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

	if !strings.Contains(blockText, "[transient") {
		t.Errorf("the MODEL's block is missing the classification:\n%s", blockText)
	}
	if eventText != blockText {
		t.Errorf("the PERSISTED event text differs from what the model saw — a resumed run "+
			"would replay a different prompt and lose the classification:\n event: %q\n block: %q",
			eventText, blockText)
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
	if !strings.Contains(text, "[transient") {
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
	if !strings.Contains(text, "[transient") {
		t.Errorf("a wrapping Post hook dropped the classification:\n%s", text)
	}
	if !strings.Contains(text, "<untrusted>upstream: connection reset</untrusted>") {
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
