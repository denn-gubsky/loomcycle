package http

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// tuningOverride is a run's own shaping: how hard the runtime retries a
// stumbling provider, how much memory it injects into the prompt, and whether
// it injects the generated tool guide.
//
// None of these changes what the agent may REACH — which is why they are free
// to set in either direction, unlike the fan-out ceiling. They change how one
// run is assembled.
//
// Every field is a pointer. Each has a meaningful zero: retry_attempts 0 means
// "do not retry", memory_inject_max_tokens 0 means "inject nothing",
// inject_tool_guide false means "leave it out" — so a plain value could express
// "raise it" but never "turn it off", and turning it off for one run is the
// commonest reason to reach for these at all.
type tuningOverride struct {
	RetryAttempts         *int  `json:"retry_attempts,omitempty"`
	MemoryInjectMaxTokens *int  `json:"memory_inject_max_tokens,omitempty"`
	MemoryIndexMaxBytes   *int  `json:"memory_index_max_bytes,omitempty"`
	InjectToolGuide       *bool `json:"inject_tool_guide,omitempty"`
}

func (o *tuningOverride) isZero() bool {
	return o == nil || (o.RetryAttempts == nil && o.MemoryInjectMaxTokens == nil &&
		o.MemoryIndexMaxBytes == nil && o.InjectToolGuide == nil)
}

func applyTuningOverride(def config.AgentDef, ov *tuningOverride) config.AgentDef {
	if ov.isZero() {
		return def
	}
	if ov.RetryAttempts != nil {
		def.RetryAttempts = ov.RetryAttempts
	}
	if ov.MemoryInjectMaxTokens != nil {
		def.MemoryInjectMaxTokens = *ov.MemoryInjectMaxTokens
	}
	if ov.MemoryIndexMaxBytes != nil {
		def.MemoryIndexMaxBytes = *ov.MemoryIndexMaxBytes
	}
	if ov.InjectToolGuide != nil {
		def.InjectToolGuide = *ov.InjectToolGuide
	}
	return def
}

func persistedTuning(ov *tuningOverride) *tuningOverride {
	if ov.isZero() {
		return nil
	}
	return ov
}

// runOverrides is everything a RUN chooses for itself, applied in one place.
//
// It exists because the alternative — a second definition variable living
// beside the first — has already cost twice. `agentDef` and `routedDef` side by
// side let RunOnce build its loop options from the un-overridden one unnoticed,
// AND made the fail-before probe for that gap pass, because the two names held
// the same value. One effective definition cannot be read from the wrong place.
type runOverrides struct {
	Routing   *routingOverride
	Resources *resourceOverride
	Tuning    *tuningOverride
	// Context is the run's own context block (mode, keep_last_n, thresholds).
	//
	// ⚠️ ITS ABSENCE WAS A BUG, not a simplification. The effective-config
	// report's `inert` array is computed from the definition this returns, and
	// its whole justification over the boot-time warning is that it sees a
	// PER-RUN override introducing a trap on a clean definition. Without this
	// field it structurally could not, so the array answered from the stored
	// definition while the same response's `fields.context` answered from the
	// run — one payload, two verdicts about one setting.
	Context *config.Context
}

// effectiveDef returns the definition this run actually runs under: the stored
// one with the run's own choices applied, as a COPY. The stored definition and
// its content hash are never touched, which is what keeps an override out of
// the definition's identity (D2).
func (s *Server) effectiveDef(ctx context.Context, def config.AgentDef, ov runOverrides) (config.AgentDef, error) {
	out, err := s.applyRoutingOverride(ctx, def, ov.Routing)
	if err != nil {
		return def, err
	}
	if out, err = applyResourceOverride(out, ov.Resources); err != nil {
		return def, err
	}
	out = applyTuningOverride(out, ov.Tuning)
	// Per-field merge, matching how the run path itself applies it — a run that
	// sets only `mode` must not blank the agent's keep_last_n.
	if ov.Context != nil {
		out.Context = config.MergeContext(out.Context, ov.Context)
	}
	return out, nil
}
