package http

import (
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// resourceOverride is a run's own budget: how long it may think, how much it
// may say, and how wide it may fan out.
//
// The asymmetry here is deliberate and is the whole design (D7). Three of these
// may be RAISED by the caller; one may only be LOWERED. That is not squeamishness
// about cost — it is which of them is load-bearing as a BOUND.
type resourceOverride struct {
	// MaxTokens and MaxIterations are raisable. Both are bounded in practice by
	// the per-scope token budget, which is enforced at admission on every
	// trigger surface, so raising them changes what one run costs and not what
	// the deployment can spend.
	MaxTokens     int `json:"max_tokens,omitempty"`
	MaxIterations int `json:"max_iterations,omitempty"`

	// UnboundedIterations is a pointer so a caller can turn it OFF as well as
	// on. A plain bool could only ever raise the cap, which would make
	// "bound this runaway agent for one run" unexpressible.
	UnboundedIterations *bool `json:"unbounded_iterations,omitempty"`

	// MaxConcurrentChildren may ONLY be lowered, because it is the only bound
	// on sub-agent fan-out that exists. A child bypasses every other gate: it
	// calls loop.Run directly rather than RunOnce, so it takes no global
	// admission slot and is not counted against the per-user cap; the budget
	// check lives inside RunOnce, so it is not budget-checked at spawn; and it
	// deliberately skips the per-provider gate for its parent's provider to
	// avoid a parent-holds-slot deadlock.
	//
	// Let a caller raise this and fan-out width has no ceiling anywhere.
	MaxConcurrentChildren int `json:"max_concurrent_children,omitempty"`
}

func (o *resourceOverride) isZero() bool {
	return o == nil || (o.MaxTokens == 0 && o.MaxIterations == 0 &&
		o.UnboundedIterations == nil && o.MaxConcurrentChildren == 0)
}

// applyResourceOverride returns the definition this run should run under.
// Like its routing sibling it returns a COPY, so the stored definition and its
// content hash are untouched.
func applyResourceOverride(def config.AgentDef, ov *resourceOverride) (config.AgentDef, error) {
	if ov.isZero() {
		return def, nil
	}
	if ov.MaxTokens > 0 {
		def.MaxTokens = ov.MaxTokens
	}
	if ov.MaxIterations > 0 {
		def.MaxIterations = ov.MaxIterations
	}
	if ov.UnboundedIterations != nil {
		def.UnboundedIterations = *ov.UnboundedIterations
	}
	if ov.MaxConcurrentChildren > 0 {
		ceiling := fanoutCeiling(def)
		if ov.MaxConcurrentChildren > ceiling {
			return def, fmt.Errorf(
				"%w: max_concurrent_children %d exceeds this agent's ceiling of %d — fan-out width "+
					"may only be lowered by a run, never raised",
				runner.ErrInvalidArgument, ov.MaxConcurrentChildren, ceiling)
		}
		def.MaxConcurrentChildren = ov.MaxConcurrentChildren
	}
	return def, nil
}

// fanoutCeiling is the width this definition is already allowed. A definition
// that declares nothing is not unbounded — it gets the Agent tool's own
// default, which is what its children would actually have been capped at.
// Reading the ceiling from anywhere else would let "declares nothing" mean
// "unlimited", which is the one thing this bound exists to prevent.
func fanoutCeiling(def config.AgentDef) int {
	if def.MaxConcurrentChildren > 0 {
		return def.MaxConcurrentChildren
	}
	return builtin.DefaultMaxConcurrentChildren
}

// persistedResources drops an all-empty override, so a run that set no budget
// records none — the same reason persistedRouting exists.
func persistedResources(ov *resourceOverride) *resourceOverride {
	if ov.isZero() {
		return nil
	}
	return ov
}
