package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/help/builtin"
)

// TestHelpInject_ExamplesReachTheAssembledPrompt is the wiring test: the
// renderer, the expander and applyMemoryInjection connected, asserted on the
// prompt a run gets. The examples are the embedded article's, in place.
func TestHelpInject_ExamplesReachTheAssembledPrompt(t *testing.T) {
	s := &Server{}
	def := config.AgentDef{SystemPrompt: "Search like this:\n{{help:Document/search}}\nThen answer."}
	got, _ := s.applyMemoryInjection(context.Background(), def, memInject{Tenant: "t1", UserID: "u1", AgentName: "a"})

	want, err := builtin.RenderExamples("Document/search")
	if err != nil {
		t.Fatal(err)
	}
	if got.SystemPrompt != "Search like this:\n"+want+"\nThen answer." {
		t.Fatalf("assembled prompt:\n%s", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, `Document {"op":"search","query":"how do we roll back a failed database migration"}`) {
		t.Errorf("first call card missing:\n%s", got.SystemPrompt)
	}
}

// TestHelpInject_EscapedPlaceholderIsUnescapedOnTheFastPath: a prompt whose
// only placeholder is an escaped help one still enters expansion, so the
// backslash is stripped as for every family.
func TestHelpInject_EscapedPlaceholderIsUnescapedOnTheFastPath(t *testing.T) {
	s := &Server{}
	def := config.AgentDef{SystemPrompt: `Write \{{help:Document/search}} to show the call cards.`}
	got, _ := s.applyMemoryInjection(context.Background(), def, memInject{Tenant: "t1", UserID: "u1"})
	if got.SystemPrompt != "Write {{help:Document/search}} to show the call cards." {
		t.Fatalf("got %q", got.SystemPrompt)
	}
}

// TestHelpInject_CallerSegmentRendersExamples: a team node's prompt is
// expanded by the same family.
func TestHelpInject_CallerSegmentRendersExamples(t *testing.T) {
	s := &Server{}
	sys, _ := s.expandCallerSegments(context.Background(), memInject{AgentName: "a"}, nil,
		"{{help:Path/ls}}", "", true, false)
	want, err := builtin.RenderExamples("Path/ls")
	if err != nil {
		t.Fatal(err)
	}
	if sys != want {
		t.Fatalf("got %q, want %q", sys, want)
	}
}
