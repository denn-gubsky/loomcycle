package http

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/help/builtin"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// fullTextCountingStore counts MemoryFullTextSearch calls over a real store.
type fullTextCountingStore struct {
	store.Store
	calls *atomic.Int32
}

func (s fullTextCountingStore) MemoryFullTextSearch(ctx context.Context, tenantID string, scope store.MemoryScope, scopeID string, filter store.MemorySearchFilter, queryText string, topK int) ([]store.MemorySearchEntry, error) {
	s.calls.Add(1)
	return s.Store.MemoryFullTextSearch(ctx, tenantID, scope, scopeID, filter, queryText, topK)
}

// TestHelpInject_HelpOnlyPromptRunsNoMemorySearch: a prompt whose only
// placeholder is {{help:}} leaves the fast path, but must not pay for the
// search_request retrieval it never asked for. With the placeholder present,
// the search still runs.
func TestHelpInject_HelpOnlyPromptRunsNoMemorySearch(t *testing.T) {
	real, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })
	var calls atomic.Int32
	s := &Server{store: fullTextCountingStore{Store: real, calls: &calls}}
	mi := memInject{Tenant: "t1", UserID: "u1", AgentName: "a", InitialInput: "roll back a migration"}

	s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "{{help:Document/search}}"}, mi)
	if n := calls.Load(); n != 0 {
		t.Fatalf("a help-only prompt ran %d memory searches, want 0", n)
	}
	s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "{{memory:search_request}}"}, mi)
	if n := calls.Load(); n != 1 {
		t.Fatalf("{{memory:search_request}} ran %d memory searches, want 1", n)
	}
}

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
