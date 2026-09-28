package http

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A run's own `interruption` block NARROWS the definition's policy; it never
// grants the Interruption tool. Asking a person touches no data and no host, so
// a run may adjust it — but which tools an agent holds is the definition's
// decision, and a run may only give tools up (see "Tools" in
// agentDefOverridability). So on an agent that does not hold the tool the block
// is inert, and the run says so once at start.
//
// Within that, each field narrows:
//   - enabled: the tool stays usable only while the block says true. It is a
//     plain bool on every wire (proto, TS, Python), so "absent" and "false"
//     cannot be told apart; failing toward off is the reading that can never
//     widen anything.
//   - kinds: the intersection with the definition's kinds (an empty list there
//     means the default, question). Nothing left in common switches it off —
//     an empty list would otherwise read as the default again.
//   - max_pending: the smaller of the two, where 0 on either side means "no
//     agent cap; the operator's global applies".

// interruptionPolicyForRun is the interruption policy a run actually has.
//
// It cannot grant because every branch returns the definition's policy or
// something narrower: an agent whose definition does not enable the tool has a
// disabled base, and a disabled base stays disabled. Whether the model can
// reach the tool at all is the dispatcher's, from the run's own tool set.
func (s *Server) interruptionPolicyForRun(def config.AgentDef, run *config.AgentInterruptionACL) tools.InterruptionPolicyValue {
	base := s.interruptionPolicyForAgent(def)
	if run == nil {
		return base
	}
	if !run.Enabled || !base.Enabled {
		return tools.InterruptionPolicyValue{}
	}
	out := base
	if len(run.Kinds) > 0 {
		out.Kinds = narrowInterruptionKinds(base.Kinds, run.Kinds)
		if len(out.Kinds) == 0 {
			return tools.InterruptionPolicyValue{}
		}
	}
	if run.MaxPending > 0 && (base.MaxPending == 0 || run.MaxPending < base.MaxPending) {
		out.MaxPending = run.MaxPending
	}
	return out
}

// narrowInterruptionKinds keeps the run's kinds the definition also allows.
func narrowInterruptionKinds(def, run []string) []string {
	allowed := def
	if len(allowed) == 0 {
		allowed = []string{store.InterruptKindQuestion}
	}
	in := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		in[k] = true
	}
	var out []string
	for _, k := range run {
		if in[k] {
			out = append(out, k)
		}
	}
	return out
}

// holdsInterruptionTool reports whether the run's own tool set includes the
// Interruption tool — after the definition's allowlist and the run's own
// `tools` narrowing, which is what decides whether the model can call it.
func holdsInterruptionTool(allowed []tools.Tool) bool {
	for _, t := range allowed {
		if t.Name() == interruptionToolName {
			return true
		}
	}
	return false
}

// runInterruption is a run's live interruption policy: set at start and
// re-derived at each operator turn, so a retune of a parked run reaches the
// Interruption tool on the turn after it — the same boundary routing adopts a
// retune at.
type runInterruption struct {
	holder *tools.InterruptionPolicyHolder
}

// startRunInterruption attaches the run's live interruption policy to ctx.
func (s *Server) startRunInterruption(ctx context.Context, def config.AgentDef, run *config.AgentInterruptionACL) (context.Context, *runInterruption) {
	live := &runInterruption{holder: tools.NewInterruptionPolicyHolder(s.interruptionPolicyForRun(def, run))}
	return tools.WithInterruptionPolicyHolder(ctx, live.holder), live
}

// adoptRunInterruption re-derives the policy from the run's current record.
func (s *Server) adoptRunInterruption(live *runInterruption, def config.AgentDef, run *config.AgentInterruptionACL) {
	if live == nil {
		return
	}
	live.holder.Store(s.interruptionPolicyForRun(def, run))
}

// emitInertInterruption reports a run that asked for interruptions on an agent
// that does not hold the tool. Once, at start, on the same event other inert
// grants use: the setting is accepted and recorded, and without this line the
// caller would learn it did nothing only when the agent never asked.
func emitInertInterruption(holdsTool bool, run *config.AgentInterruptionACL, emit func(providers.Event)) {
	if holdsTool || run == nil || !run.Enabled {
		return
	}
	const msg = "this run's interruption setting has no effect: the agent does not hold the Interruption tool. " +
		"A run can only narrow interruptions; add Interruption to the agent's tools to allow them."
	emit(providers.Event{
		Type: providers.EventCapabilityInert,
		Text: msg,
		CapabilityInert: &providers.CapabilityInertInfo{
			Tool: interruptionToolName, Gate: "interruption", Message: msg,
		},
	})
}
