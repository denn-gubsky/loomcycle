package tools

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// RunPolicy is the trust-boundary policy of one run: what its agent may reach
// and how far. It travels on the run's ctx as ONE value (WithRunPolicy /
// PolicyOf), built once per run entry point and, for a spawned child, derived
// from its parent's by Inherit, which decides every field explicitly.
//
// Before RunPolicy each of these rode its own ctx key with its own setter, and
// a child inherited whatever its spawn path forgot to restamp: a per-spawn
// override, a ledger index, a fan-out cap wider than the child's own. With one
// value there is one place that says what a child keeps (Inherit) and a test
// that refuses a field nobody wrote a rule for (inheritRules).
//
// What is NOT here, on purpose:
//   - plumbing: run ids, the dispatcher, emitters, observers, the pause gate,
//     and per-call transients (ToolUseID, HookedExecute, ExtraAllowedHosts);
//   - identity: RunIdentity (already one typed value, persisted per run);
//   - the operator plane's own authority (SubstrateOperator and the
//     operator-only def policies): a run never holds it (stripOperatorPlane).
type RunPolicy struct {
	// Tools is the agent's effective tool names (after the definition's and
	// the caller's narrowing): the ceiling the Skill, AgentDef and SkillDef
	// subset checks hold a request to.
	Tools []string
	// ToolPatterns is the agent's RAW tool declaration, globs preserved, for
	// the Skill tool's subset check (a resolved list can never cover a glob).
	ToolPatterns []string

	// Hosts is the caller's HTTP host narrowing (see HostPolicyValue).
	Hosts HostPolicyValue
	// Volumes is the run's filesystem-volume confinement (see
	// VolumePolicyValue). Zero = bound to no volume, every file tool refuses.
	Volumes VolumePolicyValue
	// EphemeralReadOnly makes every ephemeral volume read-only for this run.
	EphemeralReadOnly bool
	// VolumeDef gates VolumeDef create/delete/purge.
	VolumeDef VolumeDefPolicyValue

	Memory     MemoryPolicyValue
	SqlMem     SqlMemPolicyValue
	CoreBlocks CoreBlocksPolicyValue
	// Decision is the agent's narrowing of the operator's decision models;
	// nil narrows nothing.
	Decision *config.AgentDecision
	History  HistoryPolicyValue

	Channel ChannelPolicyValue
	// Interruption is the run's live interruption policy. A holder rather
	// than a value so a retune of a parked run reaches the Interruption tool
	// on its next turn without rebuilding ctx; nil grants nothing.
	Interruption *InterruptionPolicyHolder

	AgentDef   AgentDefPolicyValue
	Skill      SkillPolicyValue
	Evaluation EvaluationPolicyValue
	Crypto     CryptoPolicyValue
	// OperatorAuthored is whether the run's agent definition was written by
	// an operator rather than by an agent at runtime.
	OperatorAuthored bool

	// FanoutCap is the run's own parallel_spawn width; 0 lets the calling
	// agent's definition decide, by name, at spawn time.
	FanoutCap int
	// Overrides offers the run's override record to children of the same
	// definition (see RunOverridesValue).
	Overrides RunOverridesValue
	// Compaction and Context are the run's resolved-but-sparse compaction and
	// layered-context settings, which flow down the spawn tree.
	Compaction *config.Compaction
	Context    *config.Context

	// The per-spawn inputs the Agent tool hands the ONE sub-run it is about to
	// start. They are this spawn's, never the child's own: Inherit clears them.
	//
	// CompactionOverride is a spawn entry's `compaction`, blended over the
	// child's effective policy.
	CompactionOverride *config.Compaction
	// SpawnIndex is a parallel_spawn child's index + 1 (0 = not a
	// parallel_spawn child; see SpawnChildIndex).
	SpawnIndex int
	// SpawnTimeoutMs is a parallel_spawn child's timeout_ms (0 = none).
	SpawnTimeoutMs int
}

type ctxKeyRunPolicy struct{}

// WithRunPolicy attaches p as the run's whole policy, replacing any policy ctx
// already carried.
func WithRunPolicy(ctx context.Context, p RunPolicy) context.Context {
	return context.WithValue(ctx, ctxKeyRunPolicy{}, p)
}

// PolicyOf returns the run's policy, or the zero RunPolicy when ctx carries
// none — which grants nothing (see the field docs for what zero means each).
func PolicyOf(ctx context.Context) RunPolicy {
	p, _ := ctx.Value(ctxKeyRunPolicy{}).(RunPolicy)
	return p
}

// UpdateRunPolicy returns ctx carrying a copy of its policy with fn applied.
// The copy is shallow: fn must replace a slice or pointer field, never write
// through one, since the original ctx still shares it.
func UpdateRunPolicy(ctx context.Context, fn func(*RunPolicy)) context.Context {
	p := PolicyOf(ctx)
	fn(&p)
	return WithRunPolicy(ctx, p)
}

// SpawnChildIndex returns a parallel_spawn child's index and whether this
// spawn is one.
func (p RunPolicy) SpawnChildIndex() (int, bool) {
	if p.SpawnIndex <= 0 {
		return 0, false
	}
	return p.SpawnIndex - 1, true
}
