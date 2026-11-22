package ollama

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestToolImage_OllamaFollowsToolMessagesWithUserImages: a tool's picture is
// sent in a user message after the turn's tool messages, carrying the naming
// text and the picture in images.
func TestToolImage_OllamaFollowsToolMessagesWithUserImages(t *testing.T) {
	msgs := flattenMessage(providers.Message{Role: "user", Content: []providers.ContentBlock{
		{Type: "tool_result", ToolUseID: "c1", ToolName: "Workspace", Text: "a chart",
			Images: []providers.ImagePart{{MediaType: "image/png", Data: visTestB64}}},
		{Type: "tool_result", ToolUseID: "c2", ToolName: "Read", Text: "file text"},
	}})
	body, _ := json.Marshal(msgs)
	if len(msgs) != 3 || msgs[0].Role != "tool" || msgs[1].Role != "tool" || msgs[2].Role != "user" {
		t.Fatalf("want tool, tool, user; got %s", body)
	}
	if msgs[0].Content != "a chart" || len(msgs[0].Images) != 0 {
		t.Errorf("tool message changed: %s", body)
	}
	if !strings.Contains(msgs[2].Content, "c1") || len(msgs[2].Images) != 1 || msgs[2].Images[0] != visTestB64 {
		t.Errorf("trailing user message wrong: %s", body)
	}
}
