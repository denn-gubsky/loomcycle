package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// grantEmbedder is a no-op embedder just to make s.embedder non-nil (recallActive
// requires an embedder — without one, recall is inert and Recall is not granted).
type grantEmbedder struct{}

func (grantEmbedder) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }
func (grantEmbedder) Model() string                                        { return "stub" }
func (grantEmbedder) Provider() string                                     { return "stub" }
func (grantEmbedder) Dimension() int                                       { return 3 }

func recallCtx(on bool) *config.Context { return &config.Context{Recall: &on} }

func countTool(ts []tools.Tool, name string) int {
	n := 0
	for _, t := range ts {
		if t.Name() == name {
			n++
		}
	}
	return n
}

// TestGrantRecallTool pins the RFC CT auto-grant: context.recall makes the Recall
// tool appear in a run's toolset even when the agent's tools: allowlist omits it
// (an empty allowlist grants NOTHING), so enabling recall "just works" — but only
// when an embedder is configured, only when recall is on, and never duplicated.
func TestGrantRecallTool(t *testing.T) {
	recall := namedTool{name: "Recall"}
	read := namedTool{name: "Read"}
	withEmb := &Server{tools: []tools.Tool{read, recall}, embedder: grantEmbedder{}}
	noEmb := &Server{tools: []tools.Tool{read, recall}}

	// recall ON, Recall not already in the (empty-derived) toolset → appended.
	got := withEmb.grantRecallTool([]tools.Tool{}, recallCtx(true))
	if countTool(got, "Recall") != 1 {
		t.Errorf("recall on: Recall should be auto-granted, got %v", toolNames(got))
	}

	// recall ON but Recall already granted (agent listed it) → no duplicate.
	got = withEmb.grantRecallTool([]tools.Tool{read, recall}, recallCtx(true))
	if countTool(got, "Recall") != 1 {
		t.Errorf("Recall duplicated: %v", toolNames(got))
	}

	// recall OFF (explicit false, e.g. a per-run override) → not granted.
	if got := withEmb.grantRecallTool([]tools.Tool{read}, recallCtx(false)); countTool(got, "Recall") != 0 {
		t.Errorf("recall off: Recall must not be granted, got %v", toolNames(got))
	}

	// nil / no context block → default byte-identical, not granted.
	if got := withEmb.grantRecallTool([]tools.Tool{read}, nil); countTool(got, "Recall") != 0 {
		t.Errorf("nil context: Recall must not be granted, got %v", toolNames(got))
	}

	// recall ON but NO embedder → recall is inert, so Recall is not granted.
	if got := noEmb.grantRecallTool([]tools.Tool{read}, recallCtx(true)); countTool(got, "Recall") != 0 {
		t.Errorf("no embedder: Recall must not be granted (inert), got %v", toolNames(got))
	}
}
