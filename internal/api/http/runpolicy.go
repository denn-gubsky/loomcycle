package http

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// runPolicySource is what an entry point resolved for a run beyond its
// definition — the inputs that differ between a fresh run, a resumed one and
// a spawned child's own grant. Everything else in the run's policy is read off
// the definition by agentRunPolicy.
type runPolicySource struct {
	// Tools is the run's tool set after every narrowing (definition, caller,
	// hosts).
	Tools []tools.Tool
	// Hosts is the caller's host narrowing the run was started (or resumed)
	// with.
	Hosts tools.HostPolicyValue
	// Volumes is the run's resolved volume confinement.
	Volumes tools.VolumePolicyValue
	// EphemeralReadOnly is a read-only setting the run carries in from
	// outside its definition (a resumed run's record). The definition's own
	// `ephemeral_volumes: ro` is added by agentRunPolicy.
	EphemeralReadOnly bool
	// CoreBlocks is the run's effective core-block set (its own + inherited),
	// as applyMemoryInjection resolved it.
	CoreBlocks []config.CoreBlock
	// Crypto is the run's key-scope grant.
	Crypto tools.CryptoPolicyValue
	// Compaction and Context are the run's resolved compaction and
	// layered-context settings.
	Compaction *config.Compaction
	Context    *config.Context
	// FanoutCap is the run's own parallel_spawn width (0 = its definition's,
	// by name, at spawn time).
	FanoutCap int
	// Overrides is the override record the run offers its children.
	Overrides tools.RunOverridesValue
	// Interruption is the run's own interruption block (its record's), which
	// narrows the definition's.
	Interruption *config.AgentInterruptionACL
}

// agentRunPolicy builds a run's whole policy from its resolved definition and
// what its entry point resolved beyond it. It is the one constructor every run
// entry point uses — RunOnce, resumePausedRun, and a spawned child's own grant
// in prepareSubRunValues — so a grant read off the definition is read the same
// way everywhere.
//
// ctx must already carry the run's identity and principal: the memory, SQL and
// history defaults resolve from the user id, the tenant and isolation, and the
// channel set from the tenant and team scope.
//
// The returned runInterruption is the run's live interruption policy, which an
// operator turn re-derives after a retune (adoptRunInterruption); its holder is
// the policy's Interruption.
func (s *Server) agentRunPolicy(ctx context.Context, def config.AgentDef, name string, src runPolicySource) (tools.RunPolicy, *runInterruption) {
	adPolicy, evPolicy := s.substratePoliciesForAgent(def, name)
	live := &runInterruption{holder: tools.NewInterruptionPolicyHolder(s.interruptionPolicyForRun(def, src.Interruption))}
	return tools.RunPolicy{
		Tools:             toolNames(src.Tools),
		ToolPatterns:      def.Tools, // raw globs for the Skill subset check
		Hosts:             src.Hosts,
		Volumes:           src.Volumes,
		EphemeralWritable: !src.EphemeralReadOnly && def.EphemeralVolumes != config.EphemeralVolumesReadOnly,
		VolumeDef:         s.volumeDefPolicyForAgent(def),
		Memory: tools.MemoryPolicyValue{
			AllowedScopes:      tools.EffectiveMemoryScopes(ctx, def.MemoryScopes),
			QuotaBytes:         def.MemoryQuotaBytes,
			Backend:            def.MemoryBackend,
			Consolidation:      def.MemoryConsolidation,
			RecallIncludeTurns: def.RecallIncludeTurns,
			RecallAttachTraces: def.RecallAttachTraces,
			Rerank:             def.MemoryRerank,
			Units:              def.MemoryUnits,
		},
		// RFC AA: the agent's SQL Memory ACL. Empty sql_scopes → default-deny.
		SqlMem: tools.SqlMemPolicyValue{
			AllowedScopes: tools.EffectiveSqlScopes(ctx, def.SqlScopes),
			QuotaBytes:    def.SqlQuotaBytes,
		},
		// RFC BL P1: read by the Memory tool to enforce read_only/limit_bytes,
		// and the parent blocks an inherit_core_blocks child picks up.
		CoreBlocks:       tools.CoreBlocksPolicyValue{Blocks: src.CoreBlocks},
		Decision:         def.Decision,
		History:          s.historyPolicyForAgent(ctx, def),
		Channel:          s.channelPolicyForAgent(ctx, def),
		Interruption:     live.holder,
		AgentDef:         adPolicy,
		Skill:            s.skillPolicyForAgent(def),
		Evaluation:       evPolicy,
		Crypto:           src.Crypto,
		OperatorAuthored: def.OperatorAuthored,
		FanoutCap:        max(src.FanoutCap, 0),
		Overrides:        overridesOffer(src.Overrides),
		// A setting with no field set is no setting: nil, as the per-value
		// setters left it, so what a child inherits and what op=self reports
		// do not depend on whether a caller sent an empty object.
		Compaction: nilIfZeroCompaction(src.Compaction),
		Context:    nilIfZeroContext(src.Context),
	}, live
}

// overridesOffer is an offer with no record as no offer at all.
func overridesOffer(v tools.RunOverridesValue) tools.RunOverridesValue {
	if len(v.Record) == 0 {
		return tools.RunOverridesValue{}
	}
	return v
}

func nilIfZeroCompaction(c *config.Compaction) *config.Compaction {
	if c.IsZero() {
		return nil
	}
	return c
}

func nilIfZeroContext(c *config.Context) *config.Context {
	if c.IsZero() {
		return nil
	}
	return c
}
