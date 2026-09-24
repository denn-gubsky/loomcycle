package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A sub-run's context names the run that spawned it and its own id, so a
// tool-use hook can tell a sub-agent's calls from its parent's.
func TestPrepareSubRun_StampsTheParentRun(t *testing.T) {
	h := newReviewHarness(t)
	parent := tools.WithRunID(context.Background(), "r_parent")
	prep, err := h.srv.prepareSubRunValues(parent, "writer", "", "go", "", false, func(providers.Event) {}, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prep.Slot.releaseCurrent()
	defer prep.cleanup()
	if got := tools.ParentRunID(prep.LoopCtx); got != "r_parent" {
		t.Errorf("parent run id = %q, want r_parent", got)
	}
	if got := tools.RunID(prep.LoopCtx); got != prep.RunID || got == "r_parent" {
		t.Errorf("run id = %q, want the child's own (%q)", got, prep.RunID)
	}
}
