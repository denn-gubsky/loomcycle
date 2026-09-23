package providers

import (
	"context"
	"testing"
)

type capsOnly struct{ supports bool }

func (c capsOnly) ID() string                                   { return "caps" }
func (c capsOnly) Probe(context.Context) error                  { return nil }
func (c capsOnly) ListModels(context.Context) ([]string, error) { return nil, nil }
func (c capsOnly) Capabilities() Capabilities                   { return Capabilities{SupportsToolChoice: c.supports} }
func (c capsOnly) Call(context.Context, Request) (<-chan Event, error) {
	return nil, nil
}

type perModel struct{ capsOnly }

func (perModel) EnforcesToolChoice(model, _ string, _ ToolChoice) bool { return model == "yes" }

// A choice that constrains nothing is always enforced; otherwise a driver's
// per-model answer wins over the coarse provider bit.
func TestEnforcesToolChoice_PrecedenceOfAnswers(t *testing.T) {
	forced := ToolChoice{Mode: ToolChoiceRequired}
	if !EnforcesToolChoice(capsOnly{false}, "m", "", ToolChoice{}) {
		t.Error("auto must count as enforced on any provider")
	}
	if EnforcesToolChoice(capsOnly{false}, "m", "", forced) || !EnforcesToolChoice(capsOnly{true}, "m", "", forced) {
		t.Error("without a per-model answer the Capabilities bit decides")
	}
	if !EnforcesToolChoice(perModel{capsOnly{false}}, "yes", "", forced) || EnforcesToolChoice(perModel{capsOnly{true}}, "no", "", forced) {
		t.Error("the driver's per-model answer must win over the Capabilities bit")
	}
}
