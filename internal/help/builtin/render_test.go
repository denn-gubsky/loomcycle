package builtin

import (
	"errors"
	"strings"
	"testing"
)

// TestRenderExamples_EmitsEveryCallAndNoResult pins the rendered form against
// the article it comes from: Document/search has four ```json call fences and
// three ```json result fences. Every call renders as one `Tool {compact JSON}`
// line, in article order; nothing from a result fence and nothing from the
// prose reaches the output.
func TestRenderExamples_EmitsEveryCallAndNoResult(t *testing.T) {
	got, err := RenderExamples("Document/search")
	if err != nil {
		t.Fatalf("RenderExamples: %v", err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want the article's 4 call examples:\n%s", len(lines), got)
	}
	if want := `Document {"op":"search","query":"how do we roll back a failed database migration"}`; lines[0] != want {
		t.Errorf("line 0 = %q, want %q", lines[0], want)
	}
	if want := `Document {"op":"search","query":"how do we roll back a failed database migration","scope":"tenant","limit":5}`; lines[2] != want {
		t.Errorf("line 2 = %q, want %q", lines[2], want)
	}
	for i, ln := range lines {
		if !strings.HasPrefix(ln, `Document {"op":"search",`) {
			t.Errorf("line %d is not a search call card: %q", i, ln)
		}
	}
	// Fields that only appear in the result fences.
	for _, leak := range []string{"chunk_id", "rank_score", "matched_documents", "expanded_via", "Rollback plan"} {
		if strings.Contains(got, leak) {
			t.Errorf("rendered examples carry %q from a result fence:\n%s", leak, got)
		}
	}
	// And nothing from the prose around the fences.
	if strings.Contains(got, "Find where the rollback procedure") {
		t.Errorf("rendered examples carry article prose:\n%s", got)
	}
}

// TestRenderExamples_ToolAttributeNamesTheTool: a topic that is not a tool
// article names each example's tool on its fence; the line carries that tool.
func TestRenderExamples_ToolAttributeNamesTheTool(t *testing.T) {
	got, err := RenderExamples("scopes")
	if err != nil {
		t.Fatalf("RenderExamples: %v", err)
	}
	for _, ln := range strings.Split(got, "\n") {
		tool, _, _ := strings.Cut(ln, " ")
		if tool == "" || strings.HasPrefix(tool, "{") {
			t.Errorf("line names no tool: %q", ln)
		}
	}
}

func TestRenderExamples_RefusesUnknownTopic(t *testing.T) {
	for _, name := range []string{
		"Document/no_such_op",
		"no-such-topic",
		"",
		"../loader",
		"tools/Document",
		"Document/../Memory",
		"Document/search/extra",
	} {
		if _, err := RenderExamples(name); !errors.Is(err, ErrUnknownTopic) {
			t.Errorf("RenderExamples(%q) err = %v, want ErrUnknownTopic", name, err)
		}
	}
}

func TestRenderExamples_RefusesTopicWithoutExamples(t *testing.T) {
	// The loomcycle overview topic is narrative, with no `## Examples` section.
	if _, err := RenderExamples("loomcycle"); !errors.Is(err, ErrNoExamples) {
		t.Fatalf("err = %v, want ErrNoExamples", err)
	}
}
