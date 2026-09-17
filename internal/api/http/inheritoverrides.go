package http

import (
	"context"
	"log"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// inheritOverridesForChild applies the parent's overrides to a sub-run, when
// the child is the SAME DEFINITION (RFC DC D9).
//
// Three outcomes, and the middle one is the point:
//
//   - no parent overrides, or a DIFFERENT definition → the child runs on its own
//     definition, unchanged. This is the common case and is byte-identical to
//     before P5.
//   - same definition, and the override still validates → the child inherits.
//   - same definition, but the override no longer validates → the child runs on
//     its own definition and the runtime says why.
//
// That last case is why inheritance RE-VALIDATES rather than trusting what the
// parent holds. A definition can be promoted between the parent's start and the
// child's spawn, so an inherited model may be one the agent is no longer allowed
// — inheritance is a default, not a bypass.
//
// It DROPS rather than refuses, because inheritance is something the runtime
// offers rather than something the caller asked for. Failing a spawn over a
// default nobody requested would turn a promoted definition into an outage for
// every fan-out beneath it. A parent that wants a specific override on a child
// will be able to say so explicitly (P6), and that form should refuse.
func (s *Server) inheritOverridesForChild(
	ctx context.Context,
	def config.AgentDef,
	childCfg runConfigRecord,
	childDefID, childName string,
) (config.AgentDef, runConfigRecord) {
	parent := tools.RunOverrides(ctx)
	if len(parent.Record) == 0 {
		return def, childCfg
	}
	if !parent.SameDefinitionAs(childDefID, childName) {
		return def, childCfg
	}
	rec, ok := decodeRunConfig(parent.Record)
	if !ok {
		return def, childCfg
	}
	ov := runOverrides{Routing: rec.Routing, Resources: rec.Resources, Tuning: rec.Tuning}
	if ov.Routing.isZero() && ov.Resources.isZero() && ov.Tuning.isZero() {
		return def, childCfg
	}

	inherited, err := s.effectiveDef(ctx, def, ov)
	if err != nil {
		log.Printf("spawn: %q does not inherit its parent's overrides — they no longer validate "+
			"against the definition (%v); the child runs on its own settings", childName, err)
		return def, childCfg
	}

	// The child's OWN record carries what it inherited, so its row says what it
	// ran with and a resume restores the same thing. An inherited override that
	// lived only in memory would vanish on the child's first pause.
	childCfg.Routing = rec.Routing
	childCfg.Resources = rec.Resources
	childCfg.Tuning = rec.Tuning
	return inherited, childCfg
}
