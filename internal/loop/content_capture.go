package loop

import (
	"strings"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Content capture (RFC GG P3): what a model call is shown and answers, in the
// shape lcotel records on its span when the run's tenant captures content.
// lcotel redacts and cuts each field; here a block becomes a part, a binary
// block a descriptor that never carries its data, and a tool result a
// no-trace run handed over a marker.

// notRetainedContent stands in for content a no-trace run handed over: the
// model reads it, nothing the run keeps holds it, a span included.
const notRetainedContent = "[content not retained]"

func capturedParts(blocks []providers.ContentBlock) []lcotel.ContentPart {
	out := make([]lcotel.ContentPart, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, capturedPart(b))
	}
	return out
}

func capturedPart(b providers.ContentBlock) lcotel.ContentPart {
	switch {
	case b.Data != "" || b.MediaType != "":
		mt := b.MediaType
		if mt == "" {
			mt = "application/octet-stream"
		}
		return lcotel.ContentPart{Type: b.Type, MediaType: mt, Bytes: base64DecodedLen(b.Data)}
	case b.Type == "tool_use":
		return lcotel.ContentPart{Type: lcotel.ContentPartToolCall, ID: b.ToolUseID, Name: b.ToolName, Arguments: b.ToolInput}
	case b.Type == "tool_result":
		text := b.Text
		if b.NotRetained {
			text = notRetainedContent
		}
		return lcotel.ContentPart{Type: lcotel.ContentPartToolCallResponse, ID: b.ToolUseID, Response: text}
	case b.NotRetained:
		return lcotel.ContentPart{Type: lcotel.ContentPartText, Text: notRetainedContent}
	}
	return lcotel.ContentPart{Type: lcotel.ContentPartText, Text: b.Text}
}

func capturedMessages(msgs []providers.Message) []lcotel.ContentMessage {
	out := make([]lcotel.ContentMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, lcotel.ContentMessage{Role: m.Role, Parts: capturedParts(m.Content)})
	}
	return out
}

// base64DecodedLen is the size of the bytes s (standard base64, padded or
// not) encodes, without decoding it.
func base64DecodedLen(s string) int {
	s = strings.TrimRight(s, "=")
	return len(s) * 3 / 4
}

// capturedOutput collects a model call's answer from its stream, while the
// call's run captures content.
type capturedOutput struct {
	text  strings.Builder
	parts []lcotel.ContentPart
}

func (c *capturedOutput) observe(ev providers.Event) {
	switch ev.Type {
	case providers.EventText:
		c.text.WriteString(ev.Text)
	case providers.EventToolCall:
		if tu := ev.ToolUse; tu != nil {
			c.flushText()
			c.parts = append(c.parts, lcotel.ContentPart{Type: lcotel.ContentPartToolCall, ID: tu.ID, Name: tu.Name, Arguments: tu.Input})
		}
	}
}

func (c *capturedOutput) flushText() {
	if c.text.Len() > 0 {
		c.parts = append(c.parts, lcotel.ContentPart{Type: lcotel.ContentPartText, Text: c.text.String()})
		c.text.Reset()
	}
}

func (c *capturedOutput) message(finishReason string) []lcotel.ContentMessage {
	c.flushText()
	return []lcotel.ContentMessage{{Role: "assistant", Parts: c.parts, FinishReason: finishReason}}
}
