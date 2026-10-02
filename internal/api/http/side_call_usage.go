package http

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RecordRunSideCallUsage charges a model call made on a run's behalf OUTSIDE the
// loop — today, the search rerank a Memory tool call runs — to that run: one
// token_usage row attributed by the run identity on ctx, counted against the
// run's token budget like any other call.
//
// Without it the rerank would be free in every report and invisible to the
// budgets, while costing about 6,000 prompt tokens per search.
//
// A ctx with no run id (an operator route calling the tool directly) records
// nothing: there is no run to charge. The budget counters are incremented, but a
// tier this call newly crosses is not announced here — there is no event stream
// at a tool call's depth — and the next loop call that crosses reports it.
func (s *Server) RecordRunSideCallUsage(ctx context.Context, u *providers.Usage) {
	runID := tools.RunID(ctx)
	if u == nil {
		return
	}
	if runID == "" {
		s.observeCall("", u) // nothing to charge, but the call still measured the model
		return
	}
	rid := tools.RunIdentity(ctx)
	_ = s.recordCallUsage(ctx, runID, rid, rid.SessionID, loop.IterationOf(ctx), u)
}
