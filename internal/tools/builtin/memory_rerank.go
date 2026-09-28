package builtin

import (
	"context"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// rerankOptions is what the agent's `memory_rerank` asks of a search. It comes
// from the agent definition on ctx and from nowhere else: there is no tool
// parameter, because a rerank is a model call per search and the model is not
// the party that pays for it.
func rerankOptions(ctx context.Context) memrank.RerankOptions {
	r := tools.MemoryPolicy(ctx).Rerank
	if !r.On() {
		return memrank.RerankOptions{}
	}
	return memrank.RerankOptions{Enabled: true, Candidates: r.Candidates, MaxChars: r.MaxChars}
}

// renderRerank reports the rerank on a search or recall response: `reranked`,
// and when it is false, `rerank_reason`. Only for an agent that enabled it, so
// every other response keeps its exact shape.
//
// A backend that returned no report did not rerank at all (a remote memory
// layer), and that is said out loud rather than left as an absent key — the
// same reasoning as `sources_applied`: an agent configured for a rerank must not
// read an unreranked page as a reranked one.
func renderRerank(out map[string]any, opts memrank.RerankOptions, rep *memrank.RerankReport) {
	if !opts.Enabled {
		return
	}
	if rep == nil {
		rep = &memrank.RerankReport{Reason: memrank.RerankBackendUnsupported}
	}
	out["reranked"] = rep.Applied
	if !rep.Applied {
		out["rerank_reason"] = rep.Reason
	}
}

// noUnits reports whether the agent opted its searches out of Document derived
// search units (`memory_units: false`). Unset means units are used wherever an
// operator generated them.
func noUnits(ctx context.Context) bool {
	u := tools.MemoryPolicy(ctx).Units
	return u != nil && !*u
}
