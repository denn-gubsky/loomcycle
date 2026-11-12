package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func TestCapturedPart_BinaryIsADescriptorAndNotRetainedIsAMarker(t *testing.T) {
	img := capturedPart(providers.ContentBlock{Type: "image", MediaType: "image/png", Data: "aGVsbG8gd29ybGQ="})
	if img.MediaType != "image/png" || img.Bytes != 11 || img.Text != "" || img.Type != "image" {
		t.Errorf("image part = %+v, want a descriptor of 11 bytes and no data", img)
	}
	res := capturedPart(providers.ContentBlock{Type: "tool_result", ToolUseID: "t1", Text: "child said x", NotRetained: true})
	if res.Type != lcotel.ContentPartToolCallResponse || res.Response != notRetainedContent || res.ID != "t1" {
		t.Errorf("not-retained result = %+v, want the marker", res)
	}
	call := capturedPart(providers.ContentBlock{Type: "tool_use", ToolUseID: "t2", ToolName: "Read", ToolInput: json.RawMessage(`{"p":1}`)})
	if call.Type != lcotel.ContentPartToolCall || call.Name != "Read" || string(call.Arguments) != `{"p":1}` {
		t.Errorf("tool call = %+v", call)
	}
}

func TestCapturedOutput_KeepsTextAndToolCallsInStreamOrder(t *testing.T) {
	var c capturedOutput
	for _, ev := range []providers.Event{
		{Type: providers.EventText, Text: "let me "},
		{Type: providers.EventText, Text: "look"},
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "t1", Name: "Read", Input: json.RawMessage(`{}`)}},
		{Type: providers.EventThinking, Text: "hidden reasoning"},
		{Type: providers.EventText, Text: "done"},
	} {
		c.observe(ev)
	}
	msgs := c.message("tool_use")
	if len(msgs) != 1 || msgs[0].Role != "assistant" || msgs[0].FinishReason != "tool_use" {
		t.Fatalf("messages = %+v", msgs)
	}
	p := msgs[0].Parts
	if len(p) != 3 || p[0].Text != "let me look" || p[1].Name != "Read" || p[2].Text != "done" {
		t.Errorf("parts = %+v, want text, the tool call, text — and no reasoning", p)
	}
}

// notCapturedTool hands back the answer of a run whose agent never captures
// content, marked as the Agent tool marks it.
type notCapturedTool struct{ reply string }

func (n *notCapturedTool) Name() string                 { return "Echo" }
func (n *notCapturedTool) Description() string          { return "hands back a never run's answer" }
func (n *notCapturedTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (n *notCapturedTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	tools.MarkContentNotCaptured(ctx)
	return tools.Result{Text: n.reply}, nil
}

// A never child's answer reaches its capturing parent as a tool result. The
// parent's model reads it, and the transcript keeps it, but the parent's next
// model call records a marker in its place on the captured input.
func TestRun_ANeverChildsAnswerIsAMarkerInTheParentsCapturedInput(t *testing.T) {
	const secret = "NEVER-CHILD-ANSWER-7c21"
	restoreRouter := lcotel.SetTenantRouterForTest(lcotel.NewTenantRouter(lcotel.TenantRouterConfig{}))
	defer restoreRouter()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	restore := lcotel.SetTracerProviderForTest(tp)
	defer func() { restore(); _ = tp.Shutdown(context.Background()) }()

	ctx := lcotel.WithTenant(context.Background(), "acme")
	ctx = lcotel.WithContentCapture(ctx, &lcotel.ContentCapture{Tenant: "acme", MaxChars: lcotel.MaxContentMaxChars,
		Redact: func(s string) string { return s }, RedactArgs: func(_ string, b []byte) []byte { return b }})
	tool := &notCapturedTool{reply: secret}
	var stored []providers.Event
	if _, err := Run(ctx, RunOptions{
		Provider: &toolThenEndProvider{tool: "Echo"}, Model: "x",
		Tools: []tools.Tool{tool}, Dispatcher: tools.NewDispatcher([]tools.Tool{tool}),
		Segments: userSeg("go"),
		OnEvent:  func(ev providers.Event) { stored = append(stored, ev) },
	}); err != nil {
		t.Fatal(err)
	}
	var inputs []string
	for _, s := range exp.GetSpans() {
		for _, kv := range s.Attributes {
			if string(kv.Key) == "gen_ai.input.messages" {
				inputs = append(inputs, kv.Value.AsString())
			}
		}
	}
	if len(inputs) < 2 {
		t.Fatalf("captured inputs = %d, want the two model calls'", len(inputs))
	}
	for _, in := range inputs {
		if strings.Contains(in, secret) {
			t.Errorf("a captured input carries the never child's answer: %s", in)
		}
	}
	if !strings.Contains(strings.Join(inputs, "\n"), notCapturedContent) {
		t.Errorf("no captured input names the marker: %v", inputs)
	}
	kept := false
	for _, ev := range stored {
		if ev.Type == providers.EventToolResult && strings.Contains(ev.Text, secret) && !ev.ContentNotRetained {
			kept = true
		}
	}
	if !kept {
		t.Error("the never child's answer is not kept in the parent's transcript")
	}
}
