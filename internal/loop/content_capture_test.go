package loop

import (
	"encoding/json"
	"testing"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
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
