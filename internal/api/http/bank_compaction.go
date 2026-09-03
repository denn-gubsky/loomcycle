package http

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// bankCompactedSpanFn builds the loop's RFC BL P3 banking callback, or nil when
// the agent has not opted in — which is the default, so an unopted agent's
// compaction path is byte-identical to before.
//
// Scope resolution mirrors `Memory op=add`, with one hard requirement:
// **user scope**. That is not a preference. The consolidator fans out over user
// scopes only — `resolveScope("agent")` would resolve to the CONSOLIDATOR's own
// name, so an agent-scope fan-out points every child at one keyspace — which means
// a pending row banked at agent scope would be enqueued and then never drained by
// anything. Banked and silently forgotten is worse than not banked, so this
// refuses instead, by name.
//
// The callback is installed whenever `memory_flush` OR the context block's
// `harvest_to_memory` (RFC CT P2) is set — the latter generalizes the L0-only
// flush to every distillation mode (recap / stateful too), banking evicted spans
// for the same consolidator. It is installed even when it cannot bank: returning
// nil for a misconfigured agent would make the misconfiguration invisible; an error
// in the distillation marker on every pass is noisy in exactly the way an operator
// needs. `harvestToMemory` is the resolved (per-run > per-agent) context flag.
func (s *Server) bankCompactedSpanFn(agentDef config.AgentDef, harvestToMemory bool, tenantID, userID, agentName, runID, sessionID string) func(context.Context, []providers.Message) (string, error) {
	memoryFlush := agentDef.Compaction != nil && agentDef.Compaction.MemoryFlush != nil && *agentDef.Compaction.MemoryFlush
	if !memoryFlush && !harvestToMemory {
		return nil
	}
	return func(ctx context.Context, dropped []providers.Message) (string, error) {
		if s.store == nil {
			return "", fmt.Errorf("memory harvest: no persistent store on this instance")
		}
		if !containsString(agentDef.MemoryScopes, "user") {
			return "", fmt.Errorf("memory harvest is enabled but agent %q has no `user` in memory_scopes; consolidation only drains user scopes, so a banked span would never be consolidated", agentName)
		}
		if userID == "" {
			return "", fmt.Errorf("memory harvest is enabled but this run carries no user_id, so there is no user scope to bank into")
		}
		msgs := memrank.ConversationFromMessages(dropped)
		return memrank.BankSpan(ctx, s.store, memrank.BankSpanRequest{
			TenantID:  tenantID,
			Scope:     "user",
			ScopeID:   userID,
			RunID:     runID,
			SessionID: sessionID,
			Messages:  msgs,
			// Metadata rides into the payload the consolidator reads. `source`
			// tells a pass (and an operator reading a queue row) that these turns
			// were rescued from a context distillation rather than volunteered by an
			// agent (covers compaction, recap, and stateful alike).
			Metadata: map[string]string{"source": "distillation", "agent": agentName},
		})
	}
}

// containsString is a local membership check for the scope allowlist.
func containsString(hay []string, needle string) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}
