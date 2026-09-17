package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The identity rule, both directions (RFC DC V10). A rule that only fires one
// way is half-tested: "inherits when it should" and "does not when it should
// not" are separate claims and the second is the one with teeth.
func TestOverrideInheritance_IdentityRule(t *testing.T) {
	for _, tc := range []struct {
		name         string
		parent       tools.RunOverridesValue
		childDefID   string
		childName    string
		wantInherits bool
		why          string
	}{
		{
			name:         "same def_id on both sides",
			parent:       tools.RunOverridesValue{Record: []byte("{}"), DefID: "d1", AgentName: "researcher"},
			childDefID:   "d1",
			childName:    "researcher",
			wantInherits: true,
			why:          "exact identity",
		},
		{
			name:         "different def_id",
			parent:       tools.RunOverridesValue{Record: []byte("{}"), DefID: "d1", AgentName: "researcher"},
			childDefID:   "d2",
			childName:    "researcher",
			wantInherits: false,
			why:          "a different definition has a different declared set",
		},
		{
			// The commonest shape, and the one an id-only rule would break: a
			// TOP-LEVEL parent pins no def_id, so it has none to match against
			// its child's.
			name:         "top-level parent (no def_id) spawning the same agent",
			parent:       tools.RunOverridesValue{Record: []byte("{}"), AgentName: "researcher"},
			childDefID:   "d1",
			childName:    "researcher",
			wantInherits: true,
			why:          "identity falls back to the name when either side has no def_id",
		},
		{
			name:         "top-level parent spawning a DIFFERENT agent",
			parent:       tools.RunOverridesValue{Record: []byte("{}"), AgentName: "researcher"},
			childDefID:   "d9",
			childName:    "summariser",
			wantInherits: false,
			why:          "a cheap summariser must not silently inherit a frontier model",
		},
		{
			name:         "two static agents, same name",
			parent:       tools.RunOverridesValue{Record: []byte("{}"), AgentName: "chatty"},
			childName:    "chatty",
			wantInherits: true,
			why:          "neither has a def row; the name is all there is",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.parent.SameDefinitionAs(tc.childDefID, tc.childName); got != tc.wantInherits {
				t.Errorf("SameDefinitionAs = %v, want %v — %s", got, tc.wantInherits, tc.why)
			}
		})
	}
}

// RFC DC V11. An inherited override is re-validated against the CHILD's
// definition, so a definition promoted between the parent's start and the
// child's spawn cannot carry a now-invalid model through inheritance.
//
// Inheritance is a default, not a bypass.
func TestOverrideInheritance_RevalidatesAgainstTheChildsDefinition(t *testing.T) {
	srv := routingServer(t)
	ctx := context.Background()

	// A parent that chose model-b, which its own definition allowed.
	parentRec := runConfigRecord{Routing: &routingOverride{Model: "model-b"}}
	ctx = tools.WithRunOverrides(ctx, tools.RunOverridesValue{
		Record: parentRec.marshal(), AgentName: "router",
	})

	// The child's definition declares ONLY model-a — the shape of a definition
	// promoted since the parent started.
	narrowed := config.AgentDef{
		Tier:   "middle",
		Models: map[string][]config.TierCandidate{"middle": {{Provider: "primary", Model: "model-a"}}},
	}

	got, cfg := srv.inheritOverridesForChild(ctx, narrowed, runConfigRecord{}, "", "router")
	if got.Model == "model-b" {
		t.Error("the child inherited a model its own definition no longer allows — inheritance " +
			"must re-validate, not trust what the parent holds")
	}
	if cfg.Routing != nil {
		t.Error("the child recorded an override it did not adopt; its row would claim a model " +
			"it never ran on")
	}
}

// The happy path: same definition, still valid → the child runs on it AND
// records it, so a resume restores the same thing rather than reverting to the
// child's own definition on its first pause.
func TestOverrideInheritance_InheritedValueIsAlsoRecorded(t *testing.T) {
	srv := routingServer(t)
	ctx := context.Background()

	parentRec := runConfigRecord{Routing: &routingOverride{Model: "model-b"}}
	ctx = tools.WithRunOverrides(ctx, tools.RunOverridesValue{
		Record: parentRec.marshal(), AgentName: "router",
	})

	got, cfg := srv.inheritOverridesForChild(ctx, config.AgentDef{Tier: "middle"}, runConfigRecord{}, "", "router")
	if got.Model != "model-b" {
		t.Errorf("child model = %q, want model-b inherited", got.Model)
	}
	if cfg.Routing == nil || cfg.Routing.Model != "model-b" {
		t.Errorf("child record = %+v, want the inherited override persisted — one that lived "+
			"only in memory would vanish on the child's first pause", cfg.Routing)
	}
}

// No parent overrides, or a different definition, leaves the child exactly as
// it was — the common case, byte-identical to before this phase.
func TestOverrideInheritance_NoParentLeavesTheChildUntouched(t *testing.T) {
	srv := routingServer(t)
	def := config.AgentDef{Tier: "middle", Effort: "low"}

	got, cfg := srv.inheritOverridesForChild(context.Background(), def, runConfigRecord{}, "", "router")
	if got.Tier != "middle" || got.Effort != "low" || got.Model != "" {
		t.Errorf("a child with no parent overrides was changed: %+v", got)
	}
	if cfg.Routing != nil || cfg.Resources != nil || cfg.Tuning != nil {
		t.Errorf("a child with no parent overrides recorded one: %+v", cfg)
	}
}
