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
	out := injectMetadataSegments(base, "anthropic",
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

// TestInjectMetadataSegments_CodeJSNoop pins that code-js agents get NOTHING
// injected (they receive metadata via RunMeta → input.metadata; a user-role
// block here would shadow the latest-user-text the provider reads as prompt).
func TestInjectMetadataSegments_CodeJSNoop(t *testing.T) {
	base := []loop.PromptSegment{userSeg("go")}
	out := injectMetadataSegments(base, "code-js",
		map[string]any{"policy": "strict"}, map[string]any{"repo": "acme/app"})
	if len(out) != 1 || out[0].Content[0].Text != "go" {
		t.Errorf("code-js must be a no-op; got %d segments %+v", len(out), out)
	}
}

// TestPickRunTimeout pins the per-run > per-agent > global precedence.
func TestPickRunTimeout(t *testing.T) {
	if got := pickRunTimeout(900, 300); got != 900 {
		t.Errorf("per-run must win over per-agent; got %d", got)
	}
	if got := pickRunTimeout(0, 300); got != 300 {
		t.Errorf("per-agent applies when no per-run; got %d", got)
	}
	if got := pickRunTimeout(0, 0); got != 0 {
		t.Errorf("neither set ⇒ 0 (provider global default); got %d", got)
	}
}

// TestInjectMetadataSegments_EmptyNoop pins that empty maps add no segment.
func TestInjectMetadataSegments_EmptyNoop(t *testing.T) {
	base := []loop.PromptSegment{sysSeg("sp"), userSeg("go")}
	out := injectMetadataSegments(base, "anthropic", nil, nil)
	if len(out) != 2 {
		t.Errorf("empty metadata must add no segment; got %d", len(out))
	}
}
