package anthropic

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// EnforcesToolChoice must agree with what buildRequestBody does, since it is
// what the loop reports from: a model that refuses forcing, or budgeted thinking
// on a model that still takes a budget, drops the choice; adaptive thinking and
// `none` keep it.
func TestEnforcesToolChoice_MatchesWhatTheRequestBuilderSends(t *testing.T) {
	d := &Driver{}
	forced := providers.ToolChoice{Mode: providers.ToolChoiceTool, Name: "t"}
	for _, tc := range []struct {
		model, effort string
		choice        providers.ToolChoice
		want          bool
	}{
		{"claude-opus-5-5", "", forced, false},
		{"claude-fable-5-1", "", providers.ToolChoice{Mode: providers.ToolChoiceRequired}, false},
		{"claude-opus-5-5", "", providers.ToolChoice{Mode: providers.ToolChoiceNone}, true},
		{"claude-opus-5", "high", forced, true},
		{"claude-sonnet-4-6", "high", forced, false},
		{"claude-sonnet-4-6", "", forced, true},
		{"claude-sonnet-4-6", "low", forced, true},
	} {
		if got := d.EnforcesToolChoice(tc.model, tc.effort, tc.choice); got != tc.want {
			t.Errorf("%s effort=%q %+v: EnforcesToolChoice = %v, want %v", tc.model, tc.effort, tc.choice, got, tc.want)
		}
		// Cross-check against the wire: the choice must be on the body exactly
		// when EnforcesToolChoice says it is enforced.
		req := baseReq()
		req.Model, req.Effort, req.MaxTokens, req.ToolChoice = tc.model, tc.effort, 16384, tc.choice
		_, onWire := bodyOf(t, req)["tool_choice"]
		if onWire != tc.want {
			t.Errorf("%s effort=%q %+v: tool_choice on the wire = %v, but EnforcesToolChoice = %v", tc.model, tc.effort, tc.choice, onWire, tc.want)
		}
	}
}
