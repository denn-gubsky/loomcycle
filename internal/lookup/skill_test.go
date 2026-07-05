package lookup_test

import (
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// TestSkill_DriftDetection pins the substrate-shape invariant for
// SkillDef. SubstrateSkillDef and the runtime consumer (skillDefOverlay
// in api/http/server.go before commit 2; now consumes lookup.SubstrateSkillDef
// directly) must stay in lockstep. Adding a field to one without the
// other would silently drop it on unmarshal.
//
// Unlike SubstrateAgentDef, this side is currently SYMMETRIC across
// marshal + unmarshal (both ends use json-tagged structs), so the
// AgentDef-style "yaml-only consumer" bug PR #184 fixed CAN'T fire
// here today. This test is forward-looking: if a future refactor
// introduces a yaml-only intermediate struct, the symmetry would
// break + the equivalent of PR #184 would land for SkillDef. The
// drift test catches that intent.
func TestSkill_DriftDetection(t *testing.T) {
	want := map[string]bool{
		"body":        true,
		"description": true,
		"tools":       true,
	}
	have := jsonTagsOf(reflect.TypeOf(lookup.SubstrateSkillDef{}))
	for tag := range want {
		if !have[tag] {
			t.Errorf("SubstrateSkillDef missing json tag %q (must mirror skillDefOverlay in internal/tools/builtin/skilldef.go)", tag)
		}
	}
	for tag := range have {
		if !want[tag] {
			t.Errorf("SubstrateSkillDef has json tag %q not in expected set — if added deliberately, update the `want` map", tag)
		}
	}
}
