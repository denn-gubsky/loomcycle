package http

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/loop"
)

func sysSeg(text string) loop.PromptSegment {
	return loop.PromptSegment{Role: "system", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: text}}}
}
func userSeg(text string) loop.PromptSegment {
	return loop.PromptSegment{Role: "user", Content: []loop.PromptContentBlock{{Type: "text", Text: text}}}
}

// TestInjectMetadataSegments_TrustedAndUntrusted pins that an LLM agent gets
// the trusted metadata as a system trusted-text block and the untrusted
// payload metadata as a user untrusted-block (kind run_metadata), inserted
// after the leading system prompt and before the user content.
func TestInjectMetadataSegments_TrustedAndUntrusted(t *testing.T) {
	base := []loop.PromptSegment{sysSeg("you are a reviewer"), userSeg("review this")}
	out := injectMetadataSegments(base, false, // LLM: metadataViaInput=false → serialize into segments
		map[string]any{"policy": "strict"},
		map[string]any{"repo": "acme/app"},
	)
	if len(out) != 4 {
		t.Fatalf("want 4 segments (system, trusted-meta, untrusted-meta, user), got %d", len(out))
	}
	if out[0].Role != "system" || !strings.Contains(out[0].Content[0].Text, "you are a reviewer") {
		t.Errorf("agent system prompt must stay first; got %+v", out[0])
	}
	// trusted metadata: system trusted-text containing the JSON.
	if out[1].Role != "system" || out[1].Content[0].Type != "trusted-text" || !strings.Contains(out[1].Content[0].Text, `"policy"`) {
		t.Errorf("trusted metadata segment wrong: %+v", out[1])
	}
	// untrusted payload metadata: user untrusted-block kind run_metadata.
	b := out[2].Content[0]
	if out[2].Role != "user" || b.Type != "untrusted-block" || b.Kind != "run_metadata" || !strings.Contains(b.Text, `"repo"`) {
		t.Errorf("untrusted payload-metadata segment wrong: %+v", out[2])
	}
	if out[3].Content[0].Text != "review this" {
		t.Errorf("user content must come last; got %+v", out[3])
	}
}

// TestInjectMetadataSegments_StructuredInputNoop pins that a metadataViaInput
// provider (code-js) gets NOTHING injected — it receives metadata via
// RunMeta → input.metadata; a user-role block here would shadow the
// latest-user-text the provider reads as prompt.
func TestInjectMetadataSegments_StructuredInputNoop(t *testing.T) {
	base := []loop.PromptSegment{userSeg("go")}
	out := injectMetadataSegments(base, true, // metadataViaInput=true (code-js)
		map[string]any{"policy": "strict"}, map[string]any{"repo": "acme/app"})
	if len(out) != 1 || out[0].Content[0].Text != "go" {
		t.Errorf("metadataViaInput provider must be a no-op; got %d segments %+v", len(out), out)
	}
}

// TestInjectMetadataSegments_EmptyNoop pins that empty maps add no segment.
func TestInjectMetadataSegments_EmptyNoop(t *testing.T) {
	base := []loop.PromptSegment{sysSeg("sp"), userSeg("go")}
	out := injectMetadataSegments(base, false, nil, nil)
	if len(out) != 2 {
		t.Errorf("empty metadata must add no segment; got %d", len(out))
	}
}
